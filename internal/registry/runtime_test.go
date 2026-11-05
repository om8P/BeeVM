package registry

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

func TestRuntimesAndHandlers(t *testing.T) {
	r := open(t)
	a, _ := r.CreateTenant("alice")

	py, _, _, err := r.PutFunction(a.ID, "py", Spec{Runtime: "python", CodeSHA256: sha('a'), CodeSize: 1})
	if err != nil || py.Runtime != "python" || py.Handler != "handler.handler" {
		t.Fatalf("python default handler: %+v %v", py, err)
	}
	node, _, _, err := r.PutFunction(a.ID, "js", Spec{Runtime: "node", Handler: "lib/app.main", CodeSHA256: sha('b'), CodeSize: 1})
	if err != nil || node.Handler != "lib/app.main" {
		t.Fatalf("node handler: %+v %v", node, err)
	}
	got, _ := r.GetFunction(a.ID, "js")
	if got.Runtime != "node" || got.Handler != "lib/app.main" {
		t.Fatalf("read back: %+v", got)
	}

	// A redeploy may change the runtime (and the handler goes with it).
	bin, created, _, err := r.PutFunction(a.ID, "py", Spec{CodeSHA256: sha('c'), CodeSize: 1})
	if err != nil || created || bin.Runtime != "binary" || bin.Handler != "" {
		t.Fatalf("redeploy as binary: %+v %v", bin, err)
	}

	bad := map[string]Spec{
		"unknown runtime":    {Runtime: "ruby", CodeSHA256: sha('a')},
		"binary handler":     {Runtime: "binary", Handler: "x.y", CodeSHA256: sha('a')},
		"bad python handler": {Runtime: "python", Handler: "handler", CodeSHA256: sha('a')},
		"node escape":        {Runtime: "node", Handler: "../x.y", CodeSHA256: sha('a')},
	}
	for name, s := range bad {
		if _, _, _, err := r.PutFunction(a.ID, "f", s); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("%s: want ErrInvalidSpec, got %v", name, err)
		}
	}
}

// A database written by the M4-M7 code (schema version 1) is upgraded in place, and its
// functions stay binaries.
func TestUpgradeFromVersion1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(migrations[0]); err != nil {
		t.Fatal(err)
	}
	db.Exec("PRAGMA user_version = 1")
	db.Exec(`INSERT INTO tenants(id, name, created_at) VALUES(1, 'alice', 0)`)
	_, err = db.Exec(fmt.Sprintf(`INSERT INTO functions(tenant_id, name, code_sha256, code_size, memory_mib, vcpus, timeout_ms, created_at, updated_at)
		VALUES(1, 'old', '%s', 5, 128, 1, 10000, 0, 0)`, sha('a')))
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	f, err := r.GetFunction(1, "old")
	if err != nil || f.Runtime != "binary" || f.Handler != "" || f.CodeSHA256 != sha('a') {
		t.Fatalf("after upgrade: %+v %v", f, err)
	}
	var v int
	r.db.QueryRow("PRAGMA user_version").Scan(&v)
	if v != 2 {
		t.Fatalf("schema version %d", v)
	}
}

func TestFunctionCodes(t *testing.T) {
	r := open(t)
	a, _ := r.CreateTenant("alice")
	b, _ := r.CreateTenant("bob")
	f1, _, _, _ := r.PutFunction(a.ID, "one", Spec{CodeSHA256: sha('a'), CodeSize: 1})
	f2, _, _, _ := r.PutFunction(b.ID, "two", Spec{Runtime: "python", CodeSHA256: sha('b'), CodeSize: 1})
	codes, err := r.FunctionCodes()
	if err != nil || len(codes) != 2 || codes[f1.ID] != sha('a') || codes[f2.ID] != sha('b') {
		t.Fatalf("codes %v %v", codes, err)
	}
}
