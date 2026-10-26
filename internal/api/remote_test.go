package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/om8P/BeeVM/internal/apitypes"
	"github.com/om8P/BeeVM/internal/store"
)

// newRemoteEnv is newEnv with a fake artifact store attached.
func newRemoteEnv(t *testing.T) (*env, *store.Mem) {
	t.Helper()
	e := newEnv(t)
	m := store.NewMem()
	e.code.WithRemote(m)
	return e, m
}

// deployOK deploys, checks the status and returns the stored function.
func (e *env) deployOK(key, name string, code []byte, want int) apitypes.Function {
	e.t.Helper()
	resp := e.deploy(key, name, code, "")
	if resp.StatusCode != want {
		e.t.Fatalf("deploy %s: status %d, want %d", name, resp.StatusCode, want)
	}
	return decode[apitypes.Function](e.t, resp)
}

// wipeCache empties the node's local code folder, like a fresh node would have.
func wipeCache(t *testing.T, dir string) {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "*"))
	for _, f := range files {
		if err := os.Remove(f); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDeployUploadsToArtifactStore(t *testing.T) {
	e, m := newRemoteEnv(t)
	key := e.tenant("acme")
	fn := e.deployOK(key, "hello", elf("v1"), 201)
	if !m.Has("functions/" + fn.CodeSHA256) {
		t.Fatal("code was not uploaded")
	}
}

func TestInvokeFetchesMissingCodeThenHitsCache(t *testing.T) {
	e, m := newRemoteEnv(t)
	key := e.tenant("acme")
	status(t, e.deploy(key, "hello", elf("v1"), ""), 201)
	wipeCache(t, e.codeDir)

	r1 := e.do("POST", "/invoke/hello", key, nil, "")
	status(t, r1, 200)
	if got := r1.Header.Get(apitypes.HeaderCodeCache); got != "miss" {
		t.Errorf("first call cache = %q, want miss", got)
	}
	if string(e.runner.lastCode) != string(elf("v1")) {
		t.Error("function ran with the wrong code")
	}

	r2 := e.do("POST", "/invoke/hello", key, nil, "")
	status(t, r2, 200)
	if got := r2.Header.Get(apitypes.HeaderCodeCache); got != "hit" {
		t.Errorf("second call cache = %q, want hit", got)
	}
	if got := r2.Header.Get(apitypes.HeaderFetchMS); got != "0" {
		t.Errorf("fetch ms on a hit = %q, want 0", got)
	}
	if m.Gets != 1 {
		t.Errorf("%d downloads, want 1", m.Gets)
	}
}

func TestDeployIdenticalCodeUploadsOnce(t *testing.T) {
	e, m := newRemoteEnv(t)
	key := e.tenant("acme")
	status(t, e.deploy(key, "a", elf("same"), ""), 201)
	status(t, e.deploy(key, "b", elf("same"), ""), 201)
	if m.Puts != 1 {
		t.Errorf("%d uploads, want 1", m.Puts)
	}
}

func TestDeployArtifactStoreDownIs503AndNothingRegistered(t *testing.T) {
	e, m := newRemoteEnv(t)
	key := e.tenant("acme")
	m.Fail = errors.New("connection refused")
	resp := e.deploy(key, "hello", elf("v1"), "")
	status(t, resp, 503)
	if resp.Header.Get("Retry-After") == "" {
		t.Error("want Retry-After")
	}
	status(t, e.do("GET", "/api/v1/functions/hello", key, nil, ""), 404)
	if left, _ := filepath.Glob(filepath.Join(e.codeDir, "*")); len(left) != 0 {
		t.Errorf("local copy left behind: %v", left)
	}
}

func TestInvokeCachedCodeWorksWhileArtifactStoreIsDown(t *testing.T) {
	e, m := newRemoteEnv(t)
	key := e.tenant("acme")
	status(t, e.deploy(key, "hello", elf("v1"), ""), 201)
	m.Fail = errors.New("down")
	status(t, e.do("POST", "/invoke/hello", key, nil, ""), 200)
}

func TestInvokeCodeMissingEverywhereIs500(t *testing.T) {
	e, m := newRemoteEnv(t)
	key := e.tenant("acme")
	fn := e.deployOK(key, "hello", elf("v1"), 201)
	wipeCache(t, e.codeDir)
	_ = m.Delete(context.Background(), "functions/"+fn.CodeSHA256)
	status(t, e.do("POST", "/invoke/hello", key, nil, ""), 500)
}

func TestDeleteFunctionRemovesRemoteObject(t *testing.T) {
	e, m := newRemoteEnv(t)
	key := e.tenant("acme")
	fn := e.deployOK(key, "hello", elf("v1"), 201)
	status(t, e.do("DELETE", "/api/v1/functions/hello", key, nil, ""), 204)
	if m.Has("functions/" + fn.CodeSHA256) {
		t.Error("object still in the artifact store")
	}
}

func TestRedeployRemovesOldRemoteObjectOnlyWhenUnused(t *testing.T) {
	e, m := newRemoteEnv(t)
	key := e.tenant("acme")
	old := e.deployOK(key, "a", elf("v1"), 201)
	status(t, e.deploy(key, "b", elf("v1"), ""), 201) // b shares v1
	status(t, e.deploy(key, "a", elf("v2"), ""), 200) // a moves to v2
	if !m.Has("functions/" + old.CodeSHA256) {
		t.Error("v1 deleted while b still uses it")
	}
	status(t, e.do("DELETE", "/api/v1/functions/b", key, nil, ""), 204)
	if m.Has("functions/" + old.CodeSHA256) {
		t.Error("v1 should be gone once nobody uses it")
	}
}
