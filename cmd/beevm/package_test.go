package main

import (
	"archive/zip"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func zipNames(t *testing.T, r io.Reader) []string {
	t.Helper()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	return names
}

func TestPackageCode(t *testing.T) {
	dir := t.TempDir()
	app := filepath.Join(dir, "app.py")
	os.WriteFile(app, []byte("def handler(e): return e"), 0o644)

	// One file: zipped under its own name, handler defaults to <name>.handler.
	r, handler, err := packageCode(app, "")
	if err != nil || handler != "app.handler" {
		t.Fatalf("single file: %q %v", handler, err)
	}
	if names := zipNames(t, r); len(names) != 1 || names[0] != "app.py" {
		t.Fatalf("names %v", names)
	}
	// An explicit handler wins.
	if _, handler, _ = packageCode(app, "app.main"); handler != "app.main" {
		t.Fatalf("explicit handler: %q", handler)
	}

	// A folder: zipped, handler left to the server's default.
	fn := filepath.Join(dir, "fn")
	os.MkdirAll(filepath.Join(fn, "lib"), 0o755)
	os.WriteFile(filepath.Join(fn, "index.js"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(fn, "lib", "a.js"), []byte("x"), 0o644)
	r, handler, err = packageCode(fn, "")
	if err != nil || handler != "" {
		t.Fatalf("folder: %q %v", handler, err)
	}
	if names := zipNames(t, r); len(names) != 2 {
		t.Fatalf("names %v", names)
	}

	// A .zip is sent as it is.
	zipPath := filepath.Join(dir, "p.zip")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("handler.py")
	w.Write([]byte("x"))
	zw.Close()
	os.WriteFile(zipPath, buf.Bytes(), 0o644)
	r, _, err = packageCode(zipPath, "")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	r.(io.Closer).Close()
	if !bytes.Equal(got, buf.Bytes()) {
		t.Fatal("zip was changed")
	}
}
