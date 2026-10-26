package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/om8P/BeeVM/internal/apitypes"
	"github.com/om8P/BeeVM/internal/codestore"
	"github.com/om8P/BeeVM/internal/registry"
	"github.com/om8P/BeeVM/internal/runner"
)

// fakeRunner stands in for the VM: it records the request and returns what the test sets.
type fakeRunner struct {
	mu       sync.Mutex
	last     runner.Request
	lastCode []byte // the function binary as it was when Run was called
	calls    int
	result   runner.Result
	err      error
}

func (f *fakeRunner) Run(ctx context.Context, req runner.Request) (runner.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.last = req
	f.lastCode, _ = os.ReadFile(req.CodePath)
	return f.result, f.err
}

type env struct {
	t       *testing.T
	ts      *httptest.Server
	reg     *registry.Registry
	code    *codestore.Store
	runner  *fakeRunner
	codeDir string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	reg, err := registry.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	codeDir := t.TempDir()
	code, _ := codestore.New(codeDir)
	fr := &fakeRunner{result: runner.Result{
		Output: json.RawMessage(`{"ok":true}`), VMID: "vm123", BootMS: 1700, FunctionMS: 4, TotalMS: 1800,
	}}
	srv := New(Deps{
		Registry: reg, Code: code, Runner: fr,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxUploadBytes: 4096, MaxPayloadBytes: 1024,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &env{t: t, ts: ts, reg: reg, code: code, runner: fr, codeDir: codeDir}
}

// tenant creates a tenant and returns its first key.
func (e *env) tenant(name string) string {
	e.t.Helper()
	tn, err := e.reg.CreateTenant(name)
	if err != nil {
		e.t.Fatal(err)
	}
	key, _, err := e.reg.CreateKey(tn.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return key
}

func (e *env) do(method, path, key string, body io.Reader, contentType string) *http.Response {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.ts.URL+path, body)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp
}

// elf returns fake "binary" content that starts with the ELF magic bytes.
func elf(tag string) []byte { return append([]byte("\x7fELF"), []byte(tag)...) }

// deploy uploads a function. config may be "" for none.
func (e *env) deploy(key, name string, code []byte, config string) *http.Response {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if code != nil {
		fw, _ := mw.CreateFormFile("code", "fn")
		fw.Write(code)
	}
	if config != "" {
		fw, _ := mw.CreateFormField("config")
		fw.Write([]byte(config))
	}
	mw.Close()
	return e.do("PUT", "/api/v1/functions/"+name, key, &buf, mw.FormDataContentType())
}

func decode[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	defer resp.Body.Close()
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return v
}

func status(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != want {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d, want %d (%s)", resp.StatusCode, want, b)
	}
}

func codeFiles(t *testing.T, dir string) int {
	entries, _ := os.ReadDir(dir)
	return len(entries)
}

// ------------------------------------------------------------------ auth

func TestHealthzNeedsNoKey(t *testing.T) {
	e := newEnv(t)
	status(t, e.do("GET", "/healthz", "", nil, ""), 200)
}

func TestProtectedRoutesRequireAValidKey(t *testing.T) {
	e := newEnv(t)
	good := e.tenant("alice")
	tn, _ := e.reg.GetTenant("alice")
	revoked, info, _ := e.reg.CreateKey(tn.ID)
	e.reg.RevokeKey(info.Prefix)

	routes := []struct{ method, path string }{
		{"GET", "/api/v1/functions"}, {"GET", "/api/v1/functions/x"},
		{"PUT", "/api/v1/functions/x"}, {"DELETE", "/api/v1/functions/x"}, {"POST", "/invoke/x"},
	}
	for _, rt := range routes {
		for name, key := range map[string]string{"no key": "", "wrong key": "bvm_wrong", "revoked key": revoked} {
			resp := e.do(rt.method, rt.path, key, nil, "")
			if resp.StatusCode != 401 {
				t.Errorf("%s %s with %s: status %d, want 401", rt.method, rt.path, name, resp.StatusCode)
			}
			if resp.Header.Get("WWW-Authenticate") == "" {
				t.Errorf("%s %s with %s: missing WWW-Authenticate", rt.method, rt.path, name)
			}
			resp.Body.Close()
		}
	}
	// A wrong scheme is refused even with a valid key.
	req, _ := http.NewRequest("GET", e.ts.URL+"/api/v1/functions", nil)
	req.Header.Set("Authorization", "Basic "+good)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Errorf("Basic scheme: %d", resp.StatusCode)
	}
	resp.Body.Close()
	// And the good key works.
	status(t, e.do("GET", "/api/v1/functions", good, nil, ""), 200)
}

func TestKeyRevokedWhileServerRunning(t *testing.T) {
	e := newEnv(t)
	key := e.tenant("alice")
	status(t, e.do("GET", "/api/v1/functions", key, nil, ""), 200)
	keys, _ := e.reg.ListKeys("alice")
	e.reg.RevokeKey(keys[0].Prefix)
	status(t, e.do("GET", "/api/v1/functions", key, nil, ""), 401)
}

// ------------------------------------------------------------------ deploy and manage

func TestDeployListGetDelete(t *testing.T) {
	e := newEnv(t)
	key := e.tenant("alice")

	resp := e.deploy(key, "hello", elf("v1"), "")
	if resp.StatusCode != 201 {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	f := decode[apitypes.Function](t, resp)
	if f.Name != "hello" || f.MemoryMiB != 128 || f.VCPUs != 1 || f.TimeoutMS != 10000 || f.CodeSize != int64(len(elf("v1"))) {
		t.Errorf("function: %+v", f)
	}

	resp = e.deploy(key, "hello", elf("v2-longer"), `{"memory_mib":256,"timeout_ms":5000}`)
	if resp.StatusCode != 200 {
		t.Fatalf("replace: %d", resp.StatusCode)
	}
	f2 := decode[apitypes.Function](t, resp)
	if f2.MemoryMiB != 256 || f2.TimeoutMS != 5000 || f2.CodeSHA256 == f.CodeSHA256 {
		t.Errorf("replaced function: %+v", f2)
	}
	if n := codeFiles(t, e.codeDir); n != 1 {
		t.Errorf("old code should be removed after a replace; %d files stored", n)
	}

	e.deploy(key, "alpha", elf("a"), "")
	list := decode[apitypes.FunctionList](t, e.do("GET", "/api/v1/functions", key, nil, ""))
	if len(list.Functions) != 2 || list.Functions[0].Name != "alpha" || list.Functions[1].Name != "hello" {
		t.Errorf("list: %+v", list)
	}

	got := decode[apitypes.Function](t, e.do("GET", "/api/v1/functions/hello", key, nil, ""))
	if got.CodeSHA256 != f2.CodeSHA256 {
		t.Errorf("get: %+v", got)
	}

	status(t, e.do("DELETE", "/api/v1/functions/hello", key, nil, ""), 204)
	status(t, e.do("GET", "/api/v1/functions/hello", key, nil, ""), 404)
	status(t, e.do("DELETE", "/api/v1/functions/hello", key, nil, ""), 404)
	e.do("DELETE", "/api/v1/functions/alpha", key, nil, "").Body.Close()
	if n := codeFiles(t, e.codeDir); n != 0 {
		t.Errorf("all code should be removed after deleting everything; %d files stored", n)
	}
}

func TestEmptyListIsAnEmptyArray(t *testing.T) {
	e := newEnv(t)
	key := e.tenant("alice")
	resp := e.do("GET", "/api/v1/functions", key, nil, "")
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `"functions":[]`) {
		t.Errorf("got %s (clients expect [], not null)", b)
	}
}

func TestDeployRejectsBadInput(t *testing.T) {
	e := newEnv(t)
	key := e.tenant("alice")
	cases := []struct {
		name string
		resp *http.Response
		want int
	}{
		{"not an ELF file", e.deploy(key, "f", []byte("#!/bin/sh\necho hi"), ""), 400},
		{"bad function name", e.deploy(key, "Bad_Name", elf("x"), ""), 400},
		{"no code part", e.deploy(key, "f", nil, `{"memory_mib":128}`), 400},
		{"memory too small", e.deploy(key, "f", elf("x"), `{"memory_mib":8}`), 400},
		{"timeout too big", e.deploy(key, "f", elf("x"), `{"timeout_ms":999999}`), 400},
		{"unknown config field", e.deploy(key, "f", elf("x"), `{"cpus":2}`), 400},
		{"config is not JSON", e.deploy(key, "f", elf("x"), `nope`), 400},
		{"code too large", e.deploy(key, "f", append(elf("x"), make([]byte, 5000)...), ""), 413},
		{"not multipart", e.do("PUT", "/api/v1/functions/f", key, strings.NewReader("raw"), "text/plain"), 400},
	}
	for _, c := range cases {
		if c.resp.StatusCode != c.want {
			b, _ := io.ReadAll(c.resp.Body)
			t.Errorf("%s: status %d, want %d (%s)", c.name, c.resp.StatusCode, c.want, b)
		}
		c.resp.Body.Close()
	}
	if n := codeFiles(t, e.codeDir); n != 0 {
		t.Errorf("rejected uploads must not leave code behind; %d files stored", n)
	}
	if l, _ := e.reg.ListTenants(); len(l) == 1 {
		tn, _ := e.reg.GetTenant("alice")
		if fs, _ := e.reg.ListFunctions(tn.ID); len(fs) != 0 {
			t.Errorf("rejected uploads must not create functions: %+v", fs)
		}
	}
}

func TestIdenticalCodeIsSharedAndKeptWhileUsed(t *testing.T) {
	e := newEnv(t)
	a, b := e.tenant("alice"), e.tenant("bob")
	status(t, e.deploy(a, "f", elf("same"), ""), 201)
	status(t, e.deploy(b, "g", elf("same"), ""), 201)
	if n := codeFiles(t, e.codeDir); n != 1 {
		t.Errorf("identical code stored %d times", n)
	}
	status(t, e.do("DELETE", "/api/v1/functions/f", a, nil, ""), 204)
	if n := codeFiles(t, e.codeDir); n != 1 {
		t.Error("bob still uses the code; it must stay")
	}
	status(t, e.do("DELETE", "/api/v1/functions/g", b, nil, ""), 204)
	if n := codeFiles(t, e.codeDir); n != 0 {
		t.Error("nobody uses the code now; it must go")
	}
}

// ------------------------------------------------------------------ tenant isolation

func TestTenantsCannotSeeEachOther(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.tenant("alice"), e.tenant("bob")
	status(t, e.deploy(alice, "secret", elf("alice-code"), ""), 201)

	// Bob gets 404 (not 403), so he cannot even learn that the name exists.
	status(t, e.do("GET", "/api/v1/functions/secret", bob, nil, ""), 404)
	status(t, e.do("DELETE", "/api/v1/functions/secret", bob, nil, ""), 404)
	status(t, e.do("POST", "/invoke/secret", bob, strings.NewReader(`{}`), ""), 404)
	list := decode[apitypes.FunctionList](t, e.do("GET", "/api/v1/functions", bob, nil, ""))
	if len(list.Functions) != 0 {
		t.Errorf("bob sees alice's functions: %+v", list)
	}
	if e.runner.calls != 0 {
		t.Error("bob's call must never reach the runner")
	}
	// Alice's function is untouched.
	status(t, e.do("GET", "/api/v1/functions/secret", alice, nil, ""), 200)

	// The same name for both tenants runs each tenant's own code.
	status(t, e.deploy(bob, "secret", elf("bob-code"), ""), 201)
	e.do("POST", "/invoke/secret", alice, strings.NewReader(`{}`), "").Body.Close()
	if !bytes.Equal(e.runner.lastCode, elf("alice-code")) {
		t.Errorf("alice ran %q", e.runner.lastCode)
	}
	e.do("POST", "/invoke/secret", bob, strings.NewReader(`{}`), "").Body.Close()
	if !bytes.Equal(e.runner.lastCode, elf("bob-code")) {
		t.Errorf("bob ran %q", e.runner.lastCode)
	}
}

// ------------------------------------------------------------------ invoke

func TestInvokeSuccess(t *testing.T) {
	e := newEnv(t)
	key := e.tenant("alice")
	e.deploy(key, "hello", elf("code"), `{"memory_mib":256,"vcpus":2,"timeout_ms":3000}`).Body.Close()

	resp := e.do("POST", "/invoke/hello", key, strings.NewReader(`{"name":"Om"}`), "application/json")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("status %d, type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.TrimSpace(string(body)) != `{"ok":true}` {
		t.Errorf("body: %s", body)
	}
	for h, want := range map[string]string{
		apitypes.HeaderVMID: "vm123", apitypes.HeaderBootMS: "1700",
		apitypes.HeaderFunctionMS: "4", apitypes.HeaderTotalMS: "1800",
	} {
		if got := resp.Header.Get(h); got != want {
			t.Errorf("header %s = %q, want %q", h, got, want)
		}
	}
	req := e.runner.last
	if string(req.Payload) != `{"name":"Om"}` || req.MemoryMiB != 256 || req.VCPUs != 2 || req.TimeoutMS != 3000 {
		t.Errorf("runner request: %+v", req)
	}
	if !bytes.Equal(e.runner.lastCode, elf("code")) {
		t.Errorf("runner got the wrong code: %q", e.runner.lastCode)
	}
}

func TestInvokeEmptyBodyMeansNull(t *testing.T) {
	e := newEnv(t)
	key := e.tenant("alice")
	e.deploy(key, "f", elf("x"), "").Body.Close()
	status(t, e.do("POST", "/invoke/f", key, nil, ""), 200)
	if string(e.runner.last.Payload) != "null" {
		t.Errorf("payload %q", e.runner.last.Payload)
	}
}

func TestInvokeBadRequests(t *testing.T) {
	e := newEnv(t)
	key := e.tenant("alice")
	e.deploy(key, "f", elf("x"), "").Body.Close()

	status(t, e.do("POST", "/invoke/f", key, strings.NewReader(`{not json`), ""), 400)
	status(t, e.do("POST", "/invoke/f", key, strings.NewReader(`"`+strings.Repeat("a", 2000)+`"`), ""), 413)
	status(t, e.do("POST", "/invoke/missing", key, strings.NewReader(`{}`), ""), 404)
	if e.runner.calls != 0 {
		t.Errorf("bad requests must not start a VM (%d calls)", e.runner.calls)
	}
}

func TestInvokeFunctionFailureIs502(t *testing.T) {
	e := newEnv(t)
	key := e.tenant("alice")
	e.deploy(key, "f", elf("x"), "").Body.Close()
	e.runner.result = runner.Result{FunctionError: "function exited with code 3", ExitCode: 3, Stderr: "boom", VMID: "vm9"}

	resp := e.do("POST", "/invoke/f", key, strings.NewReader(`{}`), "")
	if resp.StatusCode != 502 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	body := decode[apitypes.Error](t, resp)
	if body.Error != "function exited with code 3" || body.ExitCode == nil || *body.ExitCode != 3 || body.Stderr != "boom" {
		t.Errorf("body: %+v", body)
	}
}

func TestInvokeBusyIs503WithRetryAfter(t *testing.T) {
	e := newEnv(t)
	key := e.tenant("alice")
	e.deploy(key, "f", elf("x"), "").Body.Close()
	e.runner.err = runner.ErrBusy
	resp := e.do("POST", "/invoke/f", key, strings.NewReader(`{}`), "")
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") == "" {
		t.Errorf("status %d, Retry-After %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	resp.Body.Close()
}

func TestInvokeInternalErrorHidesDetails(t *testing.T) {
	e := newEnv(t)
	key := e.tenant("alice")
	e.deploy(key, "f", elf("x"), "").Body.Close()
	e.runner.err = errors.New("start vm: open /home/secret/path/firecracker: no such file")
	resp := e.do("POST", "/invoke/f", key, strings.NewReader(`{}`), "")
	if resp.StatusCode != 500 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(b), "secret") || strings.Contains(string(b), "firecracker") {
		t.Errorf("internal details leaked to the caller: %s", b)
	}
}

func TestPanicInHandlerBecomes500(t *testing.T) {
	e := newEnv(t)
	key := e.tenant("alice")
	e.deploy(key, "f", elf("x"), "").Body.Close()
	e.runner.err = nil
	e.runner.result.Output = nil // an empty output is handled; now force a real panic:
	srv := New(Deps{Registry: e.reg, Code: e.code, Runner: panicRunner{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	req, _ := http.NewRequest("POST", ts.URL+"/invoke/f", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 500 {
		t.Errorf("status %d, want 500", resp.StatusCode)
	}
	resp.Body.Close()
	status(t, e.do("GET", "/healthz", "", nil, ""), 200) // the original server is unaffected
}

type panicRunner struct{}

func (panicRunner) Run(context.Context, runner.Request) (runner.Result, error) { panic("boom") }
