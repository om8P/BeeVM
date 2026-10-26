package registry

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func open(t *testing.T) *Registry {
	t.Helper()
	r, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func sha(c byte) string { return strings.Repeat(string(c), 64) }

func TestNames(t *testing.T) {
	for _, ok := range []string{"a", "hello", "my-func-2", "0abc"} {
		if !ValidName(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "-x", "Hello", "a_b", "a b", "a/b", "..", strings.Repeat("a", 64)} {
		if ValidName(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestTenants(t *testing.T) {
	r := open(t)
	a, err := r.CreateTenant("alice")
	if err != nil || a.ID == 0 {
		t.Fatalf("create: %+v, %v", a, err)
	}
	if _, err := r.CreateTenant("alice"); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate: want ErrExists, got %v", err)
	}
	if _, err := r.CreateTenant("Bad Name"); !errors.Is(err, ErrInvalidName) {
		t.Errorf("want ErrInvalidName, got %v", err)
	}
	r.CreateTenant("bob")
	list, _ := r.ListTenants()
	if len(list) != 2 || list[0].Name != "alice" || list[1].Name != "bob" {
		t.Errorf("list: %+v", list)
	}
	if _, err := r.GetTenant("nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestKeys(t *testing.T) {
	r := open(t)
	alice, _ := r.CreateTenant("alice")
	bob, _ := r.CreateTenant("bob")

	keyA, infoA, err := r.CreateKey(alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(keyA, "bvm_") || len(keyA) != 47 {
		t.Errorf("key shape: %q (%d chars)", keyA, len(keyA))
	}
	if infoA.Prefix != keyA[:12] {
		t.Errorf("prefix %q", infoA.Prefix)
	}
	keyB, _, _ := r.CreateKey(bob.ID)
	if keyA == keyB {
		t.Fatal("keys must be unique")
	}

	got, err := r.Authenticate(keyA)
	if err != nil || got.Name != "alice" {
		t.Errorf("auth alice: %+v, %v", got, err)
	}
	got, _ = r.Authenticate(keyB)
	if got.Name != "bob" {
		t.Errorf("auth bob: %+v", got)
	}
	for _, bad := range []string{"", "bvm_nope", keyA + "x", strings.ToUpper(keyA)} {
		if _, err := r.Authenticate(bad); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Authenticate(%q): want ErrInvalidKey, got %v", bad, err)
		}
	}
}

func TestKeyIsNotStoredInPlainText(t *testing.T) {
	r := open(t)
	a, _ := r.CreateTenant("alice")
	key, _, _ := r.CreateKey(a.ID)
	var stored string
	if err := r.db.QueryRow("SELECT key_hash FROM api_keys").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == key || strings.Contains(stored, key[4:]) || len(stored) != 64 {
		t.Errorf("stored value %q looks like the plain key", stored)
	}
}

func TestRevokeKey(t *testing.T) {
	r := open(t)
	a, _ := r.CreateTenant("alice")
	key, info, _ := r.CreateKey(a.ID)
	key2, _, _ := r.CreateKey(a.ID)

	if err := r.RevokeKey("bvm"); err == nil {
		t.Error("a too-short prefix must be refused")
	}
	if err := r.RevokeKey(info.Prefix); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Authenticate(key); !errors.Is(err, ErrInvalidKey) {
		t.Error("revoked key must stop working")
	}
	if _, err := r.Authenticate(key2); err != nil {
		t.Error("other keys must keep working")
	}
	if err := r.RevokeKey(info.Prefix); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoking twice: want ErrNotFound, got %v", err)
	}
	keys, _ := r.ListKeys("alice")
	if len(keys) != 2 || !keys[0].Revoked || keys[1].Revoked {
		t.Errorf("list: %+v", keys)
	}
	if err := r.RevokeKey("bvm_%%%%%%%%"); !errors.Is(err, ErrNotFound) {
		t.Errorf("wildcards in the prefix must not match anything, got %v", err)
	}
}

func TestFunctions(t *testing.T) {
	r := open(t)
	a, _ := r.CreateTenant("alice")

	f, created, old, err := r.PutFunction(a.ID, "hello", Spec{CodeSHA256: sha('a'), CodeSize: 10})
	if err != nil || !created || old != "" {
		t.Fatalf("create: %+v %v %q %v", f, created, old, err)
	}
	if f.MemoryMiB != 128 || f.VCPUs != 1 || f.TimeoutMS != 10000 || f.Runtime != "binary" {
		t.Errorf("defaults not applied: %+v", f)
	}

	f2, created, old, err := r.PutFunction(a.ID, "hello", Spec{CodeSHA256: sha('b'), CodeSize: 20, MemoryMiB: 256})
	if err != nil || created || old != sha('a') {
		t.Fatalf("replace: %+v %v %q %v", f2, created, old, err)
	}
	if f2.ID != f.ID || f2.CodeSHA256 != sha('b') || f2.MemoryMiB != 256 {
		t.Errorf("replace result: %+v", f2)
	}

	r.PutFunction(a.ID, "alpha", Spec{CodeSHA256: sha('c'), CodeSize: 1})
	list, _ := r.ListFunctions(a.ID)
	if len(list) != 2 || list[0].Name != "alpha" || list[1].Name != "hello" {
		t.Errorf("list order: %+v", list)
	}

	sh, err := r.DeleteFunction(a.ID, "hello")
	if err != nil || sh != sha('b') {
		t.Errorf("delete: %q, %v", sh, err)
	}
	if _, err := r.GetFunction(a.ID, "hello"); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
	if _, err := r.DeleteFunction(a.ID, "hello"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete twice: want ErrNotFound, got %v", err)
	}
}

func TestFunctionValidation(t *testing.T) {
	r := open(t)
	a, _ := r.CreateTenant("alice")
	good := Spec{CodeSHA256: sha('a'), CodeSize: 1}
	if _, _, _, err := r.PutFunction(a.ID, "Bad_Name", good); !errors.Is(err, ErrInvalidName) {
		t.Errorf("name: %v", err)
	}
	cases := map[string]func(*Spec){
		"memory too small": func(s *Spec) { s.MemoryMiB = 32 },
		"memory too big":   func(s *Spec) { s.MemoryMiB = 4096 },
		"too many vcpus":   func(s *Spec) { s.VCPUs = 16 },
		"timeout too big":  func(s *Spec) { s.TimeoutMS = 600000 },
		"negative timeout": func(s *Spec) { s.TimeoutMS = -5 },
		"missing hash":     func(s *Spec) { s.CodeSHA256 = "" },
	}
	for name, mutate := range cases {
		s := good
		mutate(&s)
		if _, _, _, err := r.PutFunction(a.ID, "f", s); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestFunctionsAreSeparatedByTenant(t *testing.T) {
	r := open(t)
	a, _ := r.CreateTenant("alice")
	b, _ := r.CreateTenant("bob")
	r.PutFunction(a.ID, "hello", Spec{CodeSHA256: sha('a'), CodeSize: 1})
	r.PutFunction(b.ID, "hello", Spec{CodeSHA256: sha('b'), CodeSize: 2}) // same name is fine

	fa, _ := r.GetFunction(a.ID, "hello")
	fb, _ := r.GetFunction(b.ID, "hello")
	if fa.CodeSHA256 != sha('a') || fb.CodeSHA256 != sha('b') {
		t.Errorf("tenants see each other's code: %+v %+v", fa, fb)
	}
	if _, err := r.DeleteFunction(b.ID, "hello"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetFunction(a.ID, "hello"); err != nil {
		t.Error("deleting bob's function must not touch alice's")
	}
	if l, _ := r.ListFunctions(b.ID); len(l) != 0 {
		t.Errorf("bob should have none: %+v", l)
	}
}

func TestCodeInUse(t *testing.T) {
	r := open(t)
	a, _ := r.CreateTenant("alice")
	b, _ := r.CreateTenant("bob")
	r.PutFunction(a.ID, "f", Spec{CodeSHA256: sha('a'), CodeSize: 1})
	r.PutFunction(b.ID, "g", Spec{CodeSHA256: sha('a'), CodeSize: 1}) // same bytes
	if in, _ := r.CodeInUse(sha('a')); !in {
		t.Error("should be in use")
	}
	r.DeleteFunction(a.ID, "f")
	if in, _ := r.CodeInUse(sha('a')); !in {
		t.Error("bob still uses it")
	}
	r.DeleteFunction(b.ID, "g")
	if in, _ := r.CodeInUse(sha('a')); in {
		t.Error("nobody uses it now")
	}
}

func TestDeletingTenantRemovesItsThings(t *testing.T) {
	r := open(t)
	a, _ := r.CreateTenant("alice")
	key, _, _ := r.CreateKey(a.ID)
	r.PutFunction(a.ID, "f", Spec{CodeSHA256: sha('a'), CodeSize: 1})
	if _, err := r.db.Exec("DELETE FROM tenants WHERE id = ?", a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Authenticate(key); !errors.Is(err, ErrInvalidKey) {
		t.Error("key should be gone with its tenant (foreign keys must be on)")
	}
	if in, _ := r.CodeInUse(sha('a')); in {
		t.Error("function should be gone with its tenant")
	}
}

func TestMigrationsAreSafeToRepeat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "again.db")
	r1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := r1.CreateTenant("alice")
	r1.Close()

	r2, err := Open(path) // second open: schema is already current
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer r2.Close()
	if got, err := r2.GetTenant("alice"); err != nil || got.ID != a.ID {
		t.Errorf("data lost on reopen: %+v %v", got, err)
	}
	var v int
	r2.db.QueryRow("PRAGMA user_version").Scan(&v)
	if v != len(migrations) {
		t.Errorf("schema version %d, want %d", v, len(migrations))
	}
}

func TestTwoHandlesShareOneFile(t *testing.T) {
	// `beevmd tenant add` runs while `beevmd serve` has the database open.
	path := filepath.Join(t.TempDir(), "shared.db")
	server, _ := Open(path)
	defer server.Close()
	admin, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()

	tn, _ := admin.CreateTenant("alice")
	key, _, err := admin.CreateKey(tn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := server.Authenticate(key); err != nil || got.Name != "alice" {
		t.Errorf("server must see the key the admin created: %+v %v", got, err)
	}
}
