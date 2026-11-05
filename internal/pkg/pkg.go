// Package pkg handles function packages: zip files that hold a Python or Node.js function.
//
// The same rules are applied twice: by the API when a package is deployed (so a bad package is
// rejected with a clear message) and by the guest agent when it unpacks one (so a package that
// somehow got past the API still cannot write outside its folder).
package pkg

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/om8P/BeeVM/internal/agentproto"
)

// ErrInvalid marks a package or handler that breaks the rules; the message says which rule.
var ErrInvalid = errors.New("invalid package")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// ValidRuntime reports whether r is a runtime BeeVM knows.
func ValidRuntime(r string) bool {
	switch r {
	case agentproto.RuntimeBinary, agentproto.RuntimePython, agentproto.RuntimeNode:
		return true
	}
	return false
}

// IsPackaged reports whether functions of runtime r are zip packages (not one binary).
func IsPackaged(r string) bool {
	return r == agentproto.RuntimePython || r == agentproto.RuntimeNode
}

// DefaultHandler is the handler used when none is given: handler.py's handler() for Python,
// index.js's exported handler for Node.js, nothing for binaries.
func DefaultHandler(runtime string) string {
	switch runtime {
	case agentproto.RuntimePython:
		return "handler.handler"
	case agentproto.RuntimeNode:
		return "index.handler"
	}
	return ""
}

var (
	pyIdent   = `[A-Za-z_][A-Za-z0-9_]*`
	pyHandler = regexp.MustCompile(`^` + pyIdent + `(\.` + pyIdent + `)*\.` + pyIdent + `$`)
	// Node: a file path without extension (letters, digits, _ - / .) and an exported name.
	nodeHandler = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_./-]*\.[A-Za-z_$][A-Za-z0-9_$]*$`)
)

// ValidateHandler checks the handler's syntax for the runtime ("" is fine: the default is used).
func ValidateHandler(runtime, handler string) error {
	if handler == "" {
		return nil
	}
	switch runtime {
	case agentproto.RuntimePython:
		if !pyHandler.MatchString(handler) {
			return invalid("handler %q must look like module.function (for example handler.handler)", handler)
		}
	case agentproto.RuntimeNode:
		// The pattern already forbids a leading "/" or "."; ".." anywhere would leave the package.
		if !nodeHandler.MatchString(handler) || strings.Contains(handler, "..") {
			return invalid("handler %q must look like file.function (for example index.handler)", handler)
		}
	default:
		return invalid("runtime %q has no handler", runtime)
	}
	return nil
}

// HandlerFiles lists the files, one of which must exist in the package for the handler.
func HandlerFiles(runtime, handler string) []string {
	if handler == "" {
		handler = DefaultHandler(runtime)
	}
	module := handler[:max(strings.LastIndex(handler, "."), 0)]
	switch runtime {
	case agentproto.RuntimePython:
		p := strings.ReplaceAll(module, ".", "/")
		return []string{p + ".py", p + "/__init__.py"}
	case agentproto.RuntimeNode:
		return []string{module + ".js", module + ".mjs", module + ".cjs"}
	}
	return nil
}

// entry is one checked zip entry.
type entry struct {
	file *zip.File
	name string // cleaned, slash-separated, relative
	dir  bool
}

// checkEntries applies the package rules to every entry, before anything is written:
//   - at most MaxPackageEntries entries,
//   - no absolute paths, no "..", no backslashes ("zip slip": a name like ../../sbin/x would
//     otherwise be written outside the target folder),
//   - only regular files and folders (no symlinks, devices, ...),
//   - declared unpacked size at most MaxUnpackedBytes (Unpack also counts the real bytes,
//     because a "zip bomb" can lie about its sizes).
func checkEntries(zr *zip.Reader) ([]entry, error) {
	if len(zr.File) == 0 {
		return nil, invalid("the zip file is empty")
	}
	if len(zr.File) > agentproto.MaxPackageEntries {
		return nil, invalid("%d files, at most %d allowed", len(zr.File), agentproto.MaxPackageEntries)
	}
	var total uint64
	out := make([]entry, 0, len(zr.File))
	for _, f := range zr.File {
		name := f.Name
		if name == "" || strings.Contains(name, `\`) || strings.HasPrefix(name, "/") || strings.ContainsRune(name, 0) {
			return nil, invalid("bad file name %q", name)
		}
		clean := path.Clean(name)
		if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
			return nil, invalid("file name %q points outside the package", name)
		}
		mode := f.Mode()
		switch {
		case mode.IsDir():
		case mode.IsRegular():
			total += f.UncompressedSize64
		default:
			return nil, invalid("%q is not a regular file or folder (symlinks are not allowed)", name)
		}
		if total > agentproto.MaxUnpackedBytes {
			return nil, invalid("unpacked size over %d MiB", agentproto.MaxUnpackedBytes>>20)
		}
		out = append(out, entry{file: f, name: clean, dir: mode.IsDir()})
	}
	return out, nil
}

// Check opens the zip at zipPath, applies the package rules and checks that the handler's
// file is in it. An error wrapping ErrInvalid means the package is bad.
func Check(zipPath, runtime, handler string) error {
	if !IsPackaged(runtime) {
		return invalid("runtime %q does not use packages", runtime)
	}
	if err := ValidateHandler(runtime, handler); err != nil {
		return err
	}
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return invalid("not a zip file (%v)", err)
	}
	defer zr.Close()
	entries, err := checkEntries(&zr.Reader)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, e := range entries {
		if !e.dir {
			have[e.name] = true
		}
	}
	want := HandlerFiles(runtime, handler)
	for _, f := range want {
		if have[f] {
			return nil
		}
	}
	if handler == "" {
		handler = DefaultHandler(runtime)
	}
	return invalid("handler %s needs %s in the zip (at the top level, not inside a folder)", handler, strings.Join(want, " or "))
}

// Unpack checks the zip at zipPath and writes its files into dir (created if missing).
// It returns the number of bytes written.
func Unpack(zipPath, dir string) (int64, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return 0, invalid("not a zip file (%v)", err)
	}
	defer zr.Close()
	entries, err := checkEntries(&zr.Reader)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	var written int64
	for _, e := range entries {
		target := filepath.Join(dir, filepath.FromSlash(e.name))
		if e.dir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return written, err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return written, err
		}
		perm := fs.FileMode(0o644)
		if e.file.Mode()&0o111 != 0 {
			perm = 0o755
		}
		n, err := writeEntry(e.file, target, perm, agentproto.MaxUnpackedBytes-written)
		written += n
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

func writeEntry(f *zip.File, target string, perm fs.FileMode, room int64) (int64, error) {
	rc, err := f.Open()
	if err != nil {
		return 0, invalid("%s: %v", f.Name, err)
	}
	defer rc.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return 0, err
	}
	// Copy at most room+1 bytes: one byte more than allowed proves the zip lied about sizes.
	n, err := io.Copy(out, io.LimitReader(rc, room+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return n, invalid("%s: %v", f.Name, err)
	}
	if n > room {
		return n, invalid("unpacked size over %d MiB", agentproto.MaxUnpackedBytes>>20)
	}
	return n, nil
}

// skipDirs are folders Zip leaves out: version control and Python's bytecode cache (the guest
// compiles what it needs; old .pyc files from another Python version are useless).
var skipDirs = map[string]bool{".git": true, "__pycache__": true}

// Zip writes a zip package of src to w. src is a folder (its contents become the top level of
// the zip) or a single file (stored under its own name).
func Zip(src string, w io.Writer) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(w)
	if !info.IsDir() {
		if err := addFile(zw, src, info.Name(), info); err != nil {
			return err
		}
		return zw.Close()
	}
	count := 0
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == src {
			return nil
		}
		if d.IsDir() && skipDirs[d.Name()] {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return nil // folders are created from the file paths
		case !info.Mode().IsRegular():
			return invalid("%s is not a regular file (symlinks are not allowed)", name)
		}
		count++
		if count > agentproto.MaxPackageEntries {
			return invalid("more than %d files", agentproto.MaxPackageEntries)
		}
		return addFile(zw, p, name, info)
	})
	if err != nil {
		return err
	}
	if count == 0 {
		return invalid("%s has no files", src)
	}
	return zw.Close()
}

func addFile(zw *zip.Writer, src, name string, info fs.FileInfo) error {
	hdr, err := zip.FileInfoHeader(info)
	if err != nil {
		return err
	}
	hdr.Name = name
	hdr.Method = zip.Deflate
	w, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

// Detect guesses the runtime of a deploy path:
//   - a folder or zip with handler.py at the top -> python; index.js/.mjs/.cjs -> node,
//   - a .py file -> python; a .js/.mjs/.cjs file -> node,
//   - a file starting with the ELF magic bytes -> binary.
func Detect(p string) (string, error) {
	info, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return detectNames(func(n string) bool {
			_, err := os.Stat(filepath.Join(p, n))
			return err == nil
		}, p)
	}
	switch strings.ToLower(filepath.Ext(p)) {
	case ".py":
		return agentproto.RuntimePython, nil
	case ".js", ".mjs", ".cjs":
		return agentproto.RuntimeNode, nil
	}
	if zr, err := zip.OpenReader(p); err == nil {
		defer zr.Close()
		names := map[string]bool{}
		for _, f := range zr.File {
			names[path.Clean(f.Name)] = true
		}
		return detectNames(func(n string) bool { return names[n] }, p)
	}
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	magic := make([]byte, 4)
	if _, err := io.ReadFull(f, magic); err == nil && string(magic) == "\x7fELF" {
		return agentproto.RuntimeBinary, nil
	}
	return "", fmt.Errorf("cannot tell the runtime of %s: give --runtime binary|python|node", p)
}

func detectNames(exists func(string) bool, p string) (string, error) {
	py := exists("handler.py")
	node := exists("index.js") || exists("index.mjs") || exists("index.cjs")
	switch {
	case py && node:
		return "", fmt.Errorf("%s has both handler.py and index.js: give --runtime python|node", p)
	case py:
		return agentproto.RuntimePython, nil
	case node:
		return agentproto.RuntimeNode, nil
	}
	return "", fmt.Errorf("cannot tell the runtime of %s (no handler.py or index.js at the top): give --runtime and --handler", p)
}
