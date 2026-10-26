package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// buildStatic compiles a tiny Go program as a static Linux binary.
func buildStatic(t *testing.T, static bool) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	os.WriteFile(src, []byte("package main\nfunc main() {}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module tiny\ngo 1.21\n"), 0o644)
	out := filepath.Join(dir, "tiny")
	cmd := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", out, ".")
	cmd.Dir = dir
	cgo := "0"
	if !static {
		cgo = "1"
	}
	cmd.Env = append(os.Environ(), "CGO_ENABLED="+cgo, "GOOS=linux", "GOARCH=amd64")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot build the test binary: %v\n%s", err, b)
	}
	return out
}

func TestCheckBinaryAcceptsAStaticBinary(t *testing.T) {
	warning, err := checkBinary(buildStatic(t, true))
	if err != nil || warning != "" {
		t.Errorf("static binary: warning %q, err %v", warning, err)
	}
}

func TestCheckBinaryRejectsNonExecutables(t *testing.T) {
	dir := t.TempDir()
	text := filepath.Join(dir, "script.sh")
	os.WriteFile(text, []byte("#!/bin/sh\necho hi\n"), 0o755)
	if _, err := checkBinary(text); err == nil || !strings.Contains(err.Error(), "not a Linux executable") {
		t.Errorf("script: %v", err)
	}
	if _, err := checkBinary(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing file must be an error")
	}
	empty := filepath.Join(dir, "empty")
	os.WriteFile(empty, nil, 0o644)
	if _, err := checkBinary(empty); err == nil {
		t.Error("an empty file must be an error")
	}
}

func TestCheckBinaryRejectsOtherArchitectures(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	os.WriteFile(src, []byte("package main\nfunc main() {}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module tiny\ngo 1.21\n"), 0o644)
	out := filepath.Join(dir, "tiny-arm")
	cmd := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", out, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=arm64")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot cross-compile: %v\n%s", err, b)
	}
	if _, err := checkBinary(out); err == nil || !strings.Contains(err.Error(), "x86-64") {
		t.Errorf("arm64 binary: %v", err)
	}
}

func TestHumanSize(t *testing.T) {
	for n, want := range map[int64]string{12: "12 B", 2048: "2.0 KB", 3 << 20: "3.0 MB"} {
		if got := humanSize(n); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestReadPayloadSources(t *testing.T) {
	if b, _ := readPayload(`{"a":1}`, ""); string(b) != `{"a":1}` {
		t.Errorf("flag: %q", b)
	}
	f := filepath.Join(t.TempDir(), "p.json")
	os.WriteFile(f, []byte(`{"b":2}`), 0o644)
	if b, _ := readPayload("", f); string(b) != `{"b":2}` {
		t.Errorf("file: %q", b)
	}
	if _, err := readPayload("x", f); err == nil {
		t.Error("both flags together must be refused")
	}
}

func TestMissingKeyIsExitCode2(t *testing.T) {
	t.Setenv("BEEVM_API_KEY", "")
	s := &settings{}
	_, err := s.client()
	ee, ok := err.(*exitError)
	if !ok || ee.code != 2 || !strings.Contains(ee.msg, "BEEVM_API_KEY") {
		t.Errorf("got %#v", err)
	}
}

func TestSettingsPrecedence(t *testing.T) {
	t.Setenv("BEEVM_API_KEY", "from-env")
	t.Setenv("BEEVM_URL", "http://env:1")
	if firstNonEmpty("flag", "env") != "flag" || firstNonEmpty("", "env") != "env" || firstNonEmpty("", "") != "" {
		t.Error("firstNonEmpty is wrong")
	}
	if _, err := (&settings{}).client(); err != nil {
		t.Errorf("a key from the environment should be enough: %v", err)
	}
}
