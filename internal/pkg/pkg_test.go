package pkg

import (
	"archive/zip"
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/om8P/BeeVM/internal/agentproto"
)

type zfile struct {
	name string
	body string
	mode fs.FileMode // 0 = regular 0644
}

// writeZip builds a zip file in a temp folder, entry by entry (so tests can create evil ones).
func writeZip(t *testing.T, files ...zfile) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range files {
		hdr := &zip.FileHeader{Name: f.name, Method: zip.Deflate}
		mode := f.mode
		if mode == 0 {
			mode = 0o644
		}
		hdr.SetMode(mode)
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "pkg.zip")
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func wantInvalid(t *testing.T, err error, contains string) {
	t.Helper()
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
	if !strings.Contains(err.Error(), contains) {
		t.Fatalf("error %q should mention %q", err, contains)
	}
}

func TestCheckGoodPackages(t *testing.T) {
	py := writeZip(t, zfile{name: "handler.py", body: "def handler(e): return e"})
	if err := Check(py, "python", ""); err != nil {
		t.Fatal(err)
	}
	pyPkg := writeZip(t, zfile{name: "app/", mode: fs.ModeDir | 0o755}, zfile{name: "app/main.py", body: "x"})
	if err := Check(pyPkg, "python", "app.main.run"); err != nil {
		t.Fatal(err)
	}
	pyInit := writeZip(t, zfile{name: "svc/__init__.py", body: "x"})
	if err := Check(pyInit, "python", "svc.handle"); err != nil {
		t.Fatal(err)
	}
	node := writeZip(t, zfile{name: "index.mjs", body: "export const handler = () => 1"})
	if err := Check(node, "node", ""); err != nil {
		t.Fatal(err)
	}
	nodeSub := writeZip(t, zfile{name: "lib/app.js", body: "x"}, zfile{name: "node_modules/a/index.js", body: "x"})
	if err := Check(nodeSub, "node", "lib/app.main"); err != nil {
		t.Fatal(err)
	}
}

func TestCheckRejects(t *testing.T) {
	cases := []struct {
		name     string
		files    []zfile
		runtime  string
		handler  string
		contains string
	}{
		{"zip slip", []zfile{{name: "handler.py"}, {name: "../../sbin/beevm-agent"}}, "python", "", "outside"},
		{"zip slip inside", []zfile{{name: "handler.py"}, {name: "a/../../x"}}, "python", "", "outside"},
		{"absolute", []zfile{{name: "/etc/passwd"}}, "python", "", "bad file name"},
		{"backslash", []zfile{{name: `..\x.py`}}, "python", "", "bad file name"},
		{"symlink", []zfile{{name: "handler.py"}, {name: "link", body: "/etc/shadow", mode: fs.ModeSymlink | 0o777}}, "python", "", "symlinks"},
		{"missing handler", []zfile{{name: "main.py"}}, "python", "", "handler.py"},
		{"handler in folder", []zfile{{name: "src/handler.py"}}, "python", "", "top level"},
		{"bad python handler", []zfile{{name: "handler.py"}}, "python", "handler", "module.function"},
		{"python path handler", []zfile{{name: "handler.py"}}, "python", "../x.f", "module.function"},
		{"node dotdot", []zfile{{name: "index.js"}}, "node", "../index.handler", "file.function"},
		{"node absolute", []zfile{{name: "index.js"}}, "node", "/index.handler", "file.function"},
		{"node no dot", []zfile{{name: "index.js"}}, "node", "index", "file.function"},
		{"binary", []zfile{{name: "x"}}, "binary", "", "does not use packages"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantInvalid(t, Check(writeZip(t, c.files...), c.runtime, c.handler), c.contains)
		})
	}
}

func TestCheckNotAZip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.zip")
	os.WriteFile(p, []byte("\x7fELF not a zip"), 0o644)
	wantInvalid(t, Check(p, "python", ""), "not a zip")
	wantInvalid(t, Check(writeZip(t), "python", ""), "empty")
}

func TestTooManyEntries(t *testing.T) {
	files := make([]zfile, agentproto.MaxPackageEntries+1)
	for i := range files {
		files[i] = zfile{name: "f" + strconv.Itoa(i)}
	}
	wantInvalid(t, Check(writeZip(t, files...), "python", ""), "at most")
}

func TestUnpack(t *testing.T) {
	p := writeZip(t,
		zfile{name: "handler.py", body: "print(1)"},
		zfile{name: "lib/", mode: fs.ModeDir | 0o755},
		zfile{name: "lib/tool.sh", body: "#!/bin/sh", mode: 0o755},
		zfile{name: "deep/a/b.txt", body: "b"},
	)
	dir := filepath.Join(t.TempDir(), "code")
	n, err := Unpack(p, dir)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len("print(1)")+len("#!/bin/sh")+1) {
		t.Fatalf("written %d", n)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "deep/a/b.txt")); string(b) != "b" {
		t.Fatalf("deep file: %q", b)
	}
	info, err := os.Stat(filepath.Join(dir, "lib/tool.sh"))
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("exec bit lost: %v %v", info.Mode(), err)
	}
	info, _ = os.Stat(filepath.Join(dir, "handler.py"))
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("handler.py mode %v", info.Mode())
	}
}

func TestUnpackRejectsBeforeWriting(t *testing.T) {
	p := writeZip(t, zfile{name: "a.py", body: "ok"}, zfile{name: "../escape", body: "bad"})
	parent := t.TempDir()
	dir := filepath.Join(parent, "code")
	if _, err := Unpack(p, dir); !errors.Is(err, ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(parent, "escape")); err == nil {
		t.Fatal("file written outside the folder")
	}
	if _, err := os.Stat(filepath.Join(dir, "a.py")); err == nil {
		t.Fatal("nothing should be written when a rule fails")
	}
}

// A zip bomb: the header claims a tiny size, the data is bigger than the limit.
func TestUnpackCountsRealBytes(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	hdr := &zip.FileHeader{Name: "big.bin", Method: zip.Deflate}
	w, _ := zw.CreateHeader(hdr)
	chunk := make([]byte, 1<<20)
	for i := 0; i <= agentproto.MaxUnpackedBytes>>20; i++ {
		w.Write(chunk)
	}
	zw.Close()
	data := buf.Bytes()
	// Patch the declared uncompressed size (central directory) to 1 byte. The zip reader
	// still reads the real data; Unpack must notice by counting.
	cd := bytes.LastIndex(data, []byte("PK\x01\x02"))
	data[cd+24], data[cd+25], data[cd+26], data[cd+27] = 1, 0, 0, 0
	p := filepath.Join(t.TempDir(), "bomb.zip")
	os.WriteFile(p, data, 0o644)
	_, err := Unpack(p, filepath.Join(t.TempDir(), "code"))
	if err == nil {
		t.Fatal("bomb unpacked")
	}
}

func TestZipFolderAndFile(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "handler.py"), []byte("def handler(e): return e"), 0o644)
	os.MkdirAll(filepath.Join(src, "__pycache__"), 0o755)
	os.WriteFile(filepath.Join(src, "__pycache__", "x.pyc"), []byte("old"), 0o644)
	os.MkdirAll(filepath.Join(src, "lib"), 0o755)
	os.WriteFile(filepath.Join(src, "lib", "util.py"), []byte("x"), 0o644)

	out := filepath.Join(t.TempDir(), "out.zip")
	f, _ := os.Create(out)
	if err := Zip(src, f); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := Check(out, "python", ""); err != nil {
		t.Fatal(err)
	}
	zr, _ := zip.OpenReader(out)
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	zr.Close()
	if strings.Join(names, ",") != "handler.py,lib/util.py" {
		t.Fatalf("names %v", names)
	}

	single := filepath.Join(t.TempDir(), "single.zip")
	f, _ = os.Create(single)
	if err := Zip(filepath.Join(src, "handler.py"), f); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := Check(single, "python", ""); err != nil {
		t.Fatal(err)
	}
}

func TestZipRejectsSymlink(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "index.js"), []byte("x"), 0o644)
	if err := os.Symlink("/etc/passwd", filepath.Join(src, "link")); err != nil {
		t.Skip("symlinks not supported here:", err)
	}
	var buf bytes.Buffer
	wantInvalid(t, Zip(src, &buf), "symlinks")
}

func TestDetect(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
		return p
	}
	cases := map[string]string{
		write("a/handler.py", "x"):  "",
		write("b/index.js", "x"):    "",
		write("c.py", "x"):          "python",
		write("d.mjs", "x"):         "node",
		write("e.bin", "\x7fELFxx"): "binary",
	}
	cases[filepath.Join(dir, "a")] = "python"
	cases[filepath.Join(dir, "b")] = "node"
	cases[writeZip(t, zfile{name: "index.cjs"})] = "node"
	for p, want := range cases {
		if want == "" {
			continue
		}
		got, err := Detect(p)
		if err != nil || got != want {
			t.Errorf("Detect(%s) = %q, %v; want %q", p, got, err, want)
		}
	}
	if _, err := Detect(write("f.txt", "hello")); err == nil {
		t.Error("text file should not be detected")
	}
	write("both/handler.py", "x")
	write("both/index.js", "x")
	if _, err := Detect(filepath.Join(dir, "both")); err == nil {
		t.Error("both runtimes should be an error")
	}
}

func TestDefaultsAndValidRuntime(t *testing.T) {
	if DefaultHandler("python") != "handler.handler" || DefaultHandler("node") != "index.handler" || DefaultHandler("binary") != "" {
		t.Fatal("default handlers")
	}
	for _, r := range []string{"binary", "python", "node"} {
		if !ValidRuntime(r) {
			t.Fatal(r)
		}
	}
	if ValidRuntime("ruby") || IsPackaged("binary") || !IsPackaged("node") {
		t.Fatal("runtime checks")
	}
	if got := HandlerFiles("python", "a.b.c"); got[0] != "a/b.py" {
		t.Fatal(got)
	}
}
