package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/om8P/BeeVM/internal/apitypes"
	"github.com/om8P/BeeVM/internal/runner"
)

// fakeSnapRunner is a fakeRunner that also builds and forgets function snapshots, and only
// has the runtimes in has.
type fakeSnapRunner struct {
	*fakeRunner
	has map[string]bool

	smu       sync.Mutex
	prepared  []runner.Request
	forgotten []int64
}

func (f *fakeSnapRunner) Prepare(req runner.Request) {
	f.smu.Lock()
	defer f.smu.Unlock()
	f.prepared = append(f.prepared, req)
}

func (f *fakeSnapRunner) ForgetFunction(id int64) {
	f.smu.Lock()
	defer f.smu.Unlock()
	f.forgotten = append(f.forgotten, id)
}

func (f *fakeSnapRunner) HasRuntime(r string) bool { return f.has[r] }

func newSnapEnv(t *testing.T) (*env, *fakeSnapRunner) {
	t.Helper()
	fr := &fakeRunner{result: runner.Result{Output: json.RawMessage(`{"ok":true}`), VMID: "vm1",
		StartKind: runner.StartRestore, Snapshot: runner.SnapshotFunction, LoadMS: 0}}
	sr := &fakeSnapRunner{fakeRunner: fr, has: map[string]bool{"binary": true, "python": true, "node": true}}
	return newEnvWith(t, sr, fr), sr
}

func zipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, _ := zw.Create(name)
		w.Write([]byte(body))
	}
	zw.Close()
	return buf.Bytes()
}

func TestDeployPythonAndNode(t *testing.T) {
	e, sr := newSnapEnv(t)
	key := e.tenant("alice")

	resp := e.deploy(key, "py", zipBytes(t, map[string]string{"handler.py": "def handler(e): return e"}), `{"runtime":"python"}`)
	status(t, resp, http.StatusCreated)
	f := decode[apitypes.Function](t, e.deploy(key, "js", zipBytes(t, map[string]string{"lib/app.mjs": "x"}), `{"runtime":"node","handler":"lib/app.main"}`))
	if f.Runtime != "node" || f.Handler != "lib/app.main" {
		t.Fatalf("node function: %+v", f)
	}
	got := decode[apitypes.Function](t, e.do("GET", "/api/v1/functions/py", key, nil, ""))
	if got.Runtime != "python" || got.Handler != "handler.handler" {
		t.Fatalf("python function: %+v", got)
	}

	// Each deploy started a snapshot build for that function, with its runtime and handler.
	sr.smu.Lock()
	if len(sr.prepared) != 2 || sr.prepared[0].Runtime != "python" || sr.prepared[0].Handler != "handler.handler" ||
		sr.prepared[1].Handler != "lib/app.main" || sr.prepared[0].CodePath == "" || sr.prepared[0].FunctionID == 0 {
		t.Fatalf("prepared: %+v", sr.prepared)
	}
	sr.smu.Unlock()

	// Invoke passes runtime and handler and reports the snapshot kind.
	resp = e.do("POST", "/invoke/py", key, strings.NewReader(`{"a":1}`), "application/json")
	status(t, resp, http.StatusOK)
	if resp.Header.Get(apitypes.HeaderSnapshot) != "function" || resp.Header.Get(apitypes.HeaderLoadMS) != "0" {
		t.Fatalf("headers: %v", resp.Header)
	}
	if e.runner.last.Runtime != "python" || e.runner.last.Handler != "handler.handler" {
		t.Fatalf("request: %+v", e.runner.last)
	}
}

func TestDeployRejectsBadPackages(t *testing.T) {
	e, sr := newSnapEnv(t)
	key := e.tenant("alice")
	cases := []struct {
		name, config, want string
		code               []byte
	}{
		{"elf as python", `{"runtime":"python"}`, "not a zip", elf("x")},
		{"missing handler file", `{"runtime":"python"}`, "handler.py", zipBytes(t, map[string]string{"main.py": "x"})},
		{"zip slip", `{"runtime":"node"}`, "outside", zipBytes(t, map[string]string{"index.js": "x", "../../evil": "x"})},
		{"bad handler", `{"runtime":"python","handler":"nodot"}`, "module.function", zipBytes(t, map[string]string{"handler.py": "x"})},
		{"zip as binary", ``, "not a Linux executable", zipBytes(t, map[string]string{"handler.py": "x"})},
		{"binary with handler", `{"handler":"a.b"}`, "no handler", elf("x")},
		{"unknown runtime", `{"runtime":"ruby"}`, "binary, python or node", elf("x")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := e.deploy(key, "f", c.code, c.config)
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), c.want) {
				t.Fatalf("got %d %s, want 400 with %q", resp.StatusCode, body, c.want)
			}
		})
	}
	if n := codeFiles(t, e.codeDir); n != 0 {
		t.Fatalf("rejected code was kept: %d files", n)
	}
	if len(sr.prepared) != 0 {
		t.Fatalf("nothing should be prepared: %+v", sr.prepared)
	}
}

func TestDeployMissingRuntimeImage(t *testing.T) {
	e, sr := newSnapEnv(t)
	sr.has = map[string]bool{"binary": true}
	key := e.tenant("alice")
	resp := e.deploy(key, "py", zipBytes(t, map[string]string{"handler.py": "x"}), `{"runtime":"python"}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "not available") {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
}

func TestRedeployAndDeleteForgetSnapshots(t *testing.T) {
	e, sr := newSnapEnv(t)
	key := e.tenant("alice")
	f := decode[apitypes.Function](t, e.deploy(key, "py", zipBytes(t, map[string]string{"handler.py": "v1"}), `{"runtime":"python"}`))
	tn, _ := e.reg.GetTenant("alice")
	id, _ := e.reg.GetFunction(tn.ID, f.Name)
	status(t, e.deploy(key, "py", zipBytes(t, map[string]string{"handler.py": "v2"}), `{"runtime":"python"}`), http.StatusOK)
	status(t, e.do("DELETE", "/api/v1/functions/py", key, nil, ""), http.StatusNoContent)
	sr.smu.Lock()
	defer sr.smu.Unlock()
	if len(sr.forgotten) != 2 || sr.forgotten[0] != id.ID || sr.forgotten[1] != id.ID {
		t.Fatalf("forgotten: %v (function %d)", sr.forgotten, id.ID)
	}
	if len(sr.prepared) != 2 || sr.prepared[1].CodeSHA256 == sr.prepared[0].CodeSHA256 {
		t.Fatalf("the redeploy should prepare the new code: %+v", sr.prepared)
	}
}

func TestInvokeRuntimeUnavailableIs503(t *testing.T) {
	e, _ := newSnapEnv(t)
	key := e.tenant("alice")
	status(t, e.deploy(key, "py", zipBytes(t, map[string]string{"handler.py": "x"}), `{"runtime":"python"}`), http.StatusCreated)
	e.runner.err = runner.ErrRuntimeUnavailable
	resp := e.do("POST", "/invoke/py", key, nil, "")
	status(t, resp, http.StatusServiceUnavailable)
}
