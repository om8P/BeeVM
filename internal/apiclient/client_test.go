package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/om8P/BeeVM/internal/api"
	"github.com/om8P/BeeVM/internal/apitypes"
	"github.com/om8P/BeeVM/internal/codestore"
	"github.com/om8P/BeeVM/internal/registry"
	"github.com/om8P/BeeVM/internal/runner"
)

type fakeRunner struct {
	result runner.Result
	last   runner.Request
}

func (f *fakeRunner) Run(_ context.Context, req runner.Request) (runner.Result, error) {
	f.last = req
	return f.result, nil
}

// setup runs the real API server (with a fake runner) and returns a client with a valid key.
func setup(t *testing.T) (*Client, *fakeRunner, string) {
	t.Helper()
	reg, err := registry.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	code, _ := codestore.New(t.TempDir())
	fr := &fakeRunner{result: runner.Result{Output: json.RawMessage(`{"hi":1}`), VMID: "vm7", BootMS: 11, FunctionMS: 22, TotalMS: 33}}
	ts := httptest.NewServer(api.New(api.Deps{
		Registry: reg, Code: code, Runner: fr, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}).Handler())
	t.Cleanup(ts.Close)

	tn, _ := reg.CreateTenant("alice")
	key, _, _ := reg.CreateKey(tn.ID)
	return New(ts.URL, key), fr, ts.URL
}

func elf(n int) []byte { return append([]byte("\x7fELF"), bytes.Repeat([]byte("x"), n)...) }

func TestDeployListGetDelete(t *testing.T) {
	c, _, _ := setup(t)
	ctx := context.Background()

	// 3 MB: bigger than any internal buffer, so it really streams.
	f, created, err := c.Deploy(ctx, "hello", bytes.NewReader(elf(3<<20)), apitypes.FunctionConfig{MemoryMiB: 256, TimeoutMS: 2000})
	if err != nil || !created {
		t.Fatalf("deploy: %+v created=%v err=%v", f, created, err)
	}
	if f.Name != "hello" || f.MemoryMiB != 256 || f.TimeoutMS != 2000 || f.CodeSize != int64(3<<20)+4 {
		t.Errorf("function: %+v", f)
	}

	f2, created, err := c.Deploy(ctx, "hello", bytes.NewReader(elf(10)), apitypes.FunctionConfig{})
	if err != nil || created || f2.MemoryMiB != 128 {
		t.Errorf("redeploy: %+v created=%v err=%v", f2, created, err)
	}

	list, err := c.List(ctx)
	if err != nil || len(list) != 1 || list[0].Name != "hello" {
		t.Errorf("list: %+v %v", list, err)
	}
	got, err := c.Get(ctx, "hello")
	if err != nil || got.CodeSHA256 != f2.CodeSHA256 {
		t.Errorf("get: %+v %v", got, err)
	}
	if err := c.Delete(ctx, "hello"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(ctx, "hello"); !IsNotFound(err) {
		t.Errorf("want not found, got %v", err)
	}
	if err := c.Delete(ctx, "hello"); !IsNotFound(err) {
		t.Errorf("delete twice: want not found, got %v", err)
	}
}

func TestDeployErrorsAreReported(t *testing.T) {
	c, _, _ := setup(t)
	_, _, err := c.Deploy(context.Background(), "f", strings.NewReader("not a binary"), apitypes.FunctionConfig{})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 400 || !strings.Contains(ae.Message, "ELF") {
		t.Errorf("want a 400 about ELF, got %v", err)
	}
	_, _, err = c.Deploy(context.Background(), "f", bytes.NewReader(elf(1)), apitypes.FunctionConfig{MemoryMiB: 1})
	if !errors.As(err, &ae) || ae.Status != 400 {
		t.Errorf("bad config: %v", err)
	}
}

func TestDeployStopsWhenTheCodeReaderFails(t *testing.T) {
	c, _, _ := setup(t)
	_, _, err := c.Deploy(context.Background(), "f", &failingReader{}, apitypes.FunctionConfig{})
	if err == nil {
		t.Error("a failing reader must fail the deploy (and not hang)")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("disk on fire") }

func TestInvoke(t *testing.T) {
	c, fr, _ := setup(t)
	ctx := context.Background()
	c.Deploy(ctx, "hello", bytes.NewReader(elf(5)), apitypes.FunctionConfig{})

	res, err := c.Invoke(ctx, "hello", []byte(`{"name":"Om"}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Output) != "{\"hi\":1}" && strings.TrimSpace(string(res.Output)) != `{"hi":1}` {
		t.Errorf("output %s", res.Output)
	}
	if res.VMID != "vm7" || res.BootMS != 11 || res.FunctionMS != 22 || res.TotalMS != 33 {
		t.Errorf("result: %+v", res)
	}
	if string(fr.last.Payload) != `{"name":"Om"}` {
		t.Errorf("payload %q", fr.last.Payload)
	}
}

func TestInvokeFunctionFailureCarriesExitCodeAndLog(t *testing.T) {
	c, fr, _ := setup(t)
	ctx := context.Background()
	c.Deploy(ctx, "f", bytes.NewReader(elf(5)), apitypes.FunctionConfig{})
	fr.result = runner.Result{FunctionError: "function exited with code 3", ExitCode: 3, Stderr: "boom"}

	_, err := c.Invoke(ctx, "f", nil)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 502 || ae.ExitCode == nil || *ae.ExitCode != 3 || ae.Stderr != "boom" {
		t.Errorf("got %#v", err)
	}
}

func TestWrongKeyIs401AndUnknownFunctionIs404(t *testing.T) {
	c, _, url := setup(t)
	bad := New(url, "bvm_wrong")
	var ae *APIError
	if _, err := bad.List(context.Background()); !errors.As(err, &ae) || ae.Status != 401 {
		t.Errorf("want 401, got %v", err)
	}
	if _, err := c.Invoke(context.Background(), "nope", nil); !IsNotFound(err) {
		t.Errorf("want 404, got %v", err)
	}
}

func TestConnectionRefusedIsAnError(t *testing.T) {
	c := New("http://127.0.0.1:1", "k")
	if _, err := c.List(context.Background()); err == nil {
		t.Error("expected an error")
	}
}
