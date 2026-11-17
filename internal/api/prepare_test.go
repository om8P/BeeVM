package api

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareSnapshotsAsksForEveryDeployedFunction(t *testing.T) {
	e, sr := newSnapEnv(t)
	alice, bob := e.tenant("alice"), e.tenant("bob")
	e.deploy(alice, "py", zipBytes(t, map[string]string{"handler.py": "def handler(e): return e"}), `{"runtime":"python"}`).Body.Close()
	e.deploy(alice, "bin", elf("a"), "").Body.Close()
	e.deploy(bob, "lost", elf("b"), "").Body.Close()
	sr.smu.Lock()
	sr.prepared = nil // forget the builds the deploys started
	sr.smu.Unlock()

	// bob's code is gone from this node and there is no artifact store: skipped with a warning.
	lost := decode[struct {
		CodeSHA256 string `json:"code_sha256"`
	}](t, e.do("GET", "/api/v1/functions/lost", bob, nil, ""))
	if err := os.Remove(filepath.Join(e.codeDir, lost.CodeSHA256)); err != nil {
		t.Fatal(err)
	}

	srv := New(Deps{Registry: e.reg, Code: e.code, Runner: sr, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	n, err := srv.PrepareSnapshots(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("checked %d functions, %v", n, err)
	}
	sr.smu.Lock()
	defer sr.smu.Unlock()
	if len(sr.prepared) != 2 {
		t.Fatalf("prepared: %+v", sr.prepared)
	}
	for _, r := range sr.prepared {
		if r.FunctionID == 0 || r.CodePath == "" || r.TenantID == 0 {
			t.Errorf("incomplete request: %+v", r)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := srv.PrepareSnapshots(ctx); err == nil {
		t.Error("a cancelled context should stop it")
	}
}

func TestPrepareSnapshotsWithoutPreparer(t *testing.T) {
	e := newEnv(t) // the plain fakeRunner cannot build snapshots
	srv := New(Deps{Registry: e.reg, Code: e.code, Runner: e.runner, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if n, err := srv.PrepareSnapshots(context.Background()); n != 0 || err != nil {
		t.Errorf("got %d, %v", n, err)
	}
}
