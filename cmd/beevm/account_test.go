package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/om8P/BeeVM/internal/api"
	"github.com/om8P/BeeVM/internal/apiclient"
	"github.com/om8P/BeeVM/internal/apitypes"
	"github.com/om8P/BeeVM/internal/codestore"
	"github.com/om8P/BeeVM/internal/registry"
	"github.com/om8P/BeeVM/internal/runner"
)

type noRunner struct{}

func (noRunner) Run(context.Context, runner.Request) (runner.Result, error) {
	return runner.Result{Output: json.RawMessage(`{}`)}, nil
}

// testServer starts an in-process API server with tenant "alice" and returns its URL and key.
// The CLI's environment is cleared and its config file moved into a temporary folder.
func testServer(t *testing.T) (url, key string) {
	t.Helper()
	reg, err := registry.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	code, _ := codestore.New(t.TempDir())
	ts := httptest.NewServer(api.New(api.Deps{
		Registry: reg, Code: code, Runner: noRunner{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxUploadBytes: 1 << 20, MaxPayloadBytes: 1 << 10,
	}).Handler())
	t.Cleanup(ts.Close)
	tn, _ := reg.CreateTenant("alice")
	key, _, _ = reg.CreateKey(tn.ID)

	t.Setenv("BEEVM_API_KEY", "")
	t.Setenv("BEEVM_URL", "")
	t.Setenv("BEEVM_CONFIG", filepath.Join(t.TempDir(), "cfg", "config.json"))
	return ts.URL, key
}

// runCLI runs the beevm command line with args and returns what it printed on stdout.
func runCLI(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	r, w, _ := os.Pipe()
	oldOut, oldIn := os.Stdout, os.Stdin
	os.Stdout = w
	if stdin != "" {
		in, _ := os.CreateTemp(t.TempDir(), "stdin")
		in.WriteString(stdin)
		in.Seek(0, 0)
		os.Stdin = in
	}
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	root := newRoot()
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	w.Close()
	os.Stdout, os.Stdin = oldOut, oldIn
	return <-done, err
}

func TestLoginSavesKeyAndLaterCommandsUseIt(t *testing.T) {
	url, key := testServer(t)

	if _, err := runCLI(t, "", "ls"); err == nil || !strings.Contains(err.Error(), "beevm login") {
		t.Fatalf("without a key: %v", err)
	}
	if _, err := runCLI(t, "bvm_wrong\n", "login", "--url", url); err == nil {
		t.Fatal("a wrong key must not be saved")
	}
	if _, err := os.Stat(os.Getenv("BEEVM_CONFIG")); err == nil {
		t.Fatal("config written for a wrong key")
	}

	out, err := runCLI(t, key+"\n", "login", "--url", url)
	if err != nil || !strings.Contains(out, "as tenant alice") {
		t.Fatalf("login: %q %v", out, err)
	}
	st, err := os.Stat(os.Getenv("BEEVM_CONFIG"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("config file: %v %v", st, err)
	}

	// No flags, no environment: the saved URL and key are used.
	out, err = runCLI(t, "", "whoami")
	if err != nil || !strings.Contains(out, "tenant alice on "+url) {
		t.Errorf("whoami: %q %v", out, err)
	}
	// The environment wins over the saved key.
	t.Setenv("BEEVM_API_KEY", "bvm_wrong")
	if _, err := runCLI(t, "", "whoami"); err == nil {
		t.Error("BEEVM_API_KEY should override the saved key")
	}
	t.Setenv("BEEVM_API_KEY", "")

	if out, err = runCLI(t, "", "logout"); err != nil || !strings.Contains(out, "removed") {
		t.Errorf("logout: %q %v", out, err)
	}
	if _, err := runCLI(t, "", "whoami"); err == nil {
		t.Error("after logout there is no key")
	}
}

func TestGetAndJSONOutput(t *testing.T) {
	url, key := testServer(t)
	c := apiclient.New(url, key)
	if _, _, err := c.Deploy(context.Background(), "hello", bytes.NewReader([]byte("\x7fELFxx")), apitypes.FunctionConfig{MemoryMiB: 256}); err != nil {
		t.Fatal(err)
	}
	flags := []string{"--url", url, "--key", key}

	out, err := runCLI(t, "", append([]string{"get", "hello"}, flags...)...)
	if err != nil || !strings.Contains(out, "memory:") || !strings.Contains(out, "256 MiB") {
		t.Errorf("get: %q %v", out, err)
	}
	out, err = runCLI(t, "", append([]string{"get", "hello", "--json"}, flags...)...)
	var f apitypes.Function
	if err != nil || json.Unmarshal([]byte(out), &f) != nil || f.Name != "hello" || f.MemoryMiB != 256 {
		t.Errorf("get --json: %q %v", out, err)
	}
	out, err = runCLI(t, "", append([]string{"ls", "--json"}, flags...)...)
	var fns []apitypes.Function
	if err != nil || json.Unmarshal([]byte(out), &fns) != nil || len(fns) != 1 {
		t.Errorf("ls --json: %q %v", out, err)
	}
	out, err = runCLI(t, "", append([]string{"vms", "--json"}, flags...)...)
	var vms []apitypes.VM
	if err != nil || json.Unmarshal([]byte(out), &vms) != nil {
		t.Errorf("vms --json: %q %v", out, err)
	}
	if _, err := runCLI(t, "", append([]string{"get", "missing"}, flags...)...); !apiclient.IsNotFound(err) {
		t.Errorf("get missing: %v", err)
	}
}

func TestInitWritesStarterFunctions(t *testing.T) {
	base := t.TempDir()
	for _, rt := range []string{"python", "node", "binary"} {
		dir := filepath.Join(base, "My_Func "+rt)
		out, err := runCLI(t, "", "init", rt, dir)
		if err != nil {
			t.Fatalf("%s: %v", rt, err)
		}
		if !strings.Contains(out, "beevm deploy my-func-"+rt) {
			t.Errorf("%s: next steps %q", rt, out)
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != len(initTemplates[rt]) {
			t.Errorf("%s: %d files", rt, len(entries))
		}
		// A second init into the same folder must not overwrite anything.
		if _, err := runCLI(t, "", "init", rt, dir); err == nil || !strings.Contains(err.Error(), "not empty") {
			t.Errorf("%s: second init: %v", rt, err)
		}
	}
	if _, err := runCLI(t, "", "init", "ruby", filepath.Join(base, "r")); err == nil {
		t.Error("unknown runtime accepted")
	}
	gomod, _ := os.ReadFile(filepath.Join(base, "My_Func binary", "go.mod"))
	if !strings.HasPrefix(string(gomod), "module my-func-binary\n") {
		t.Errorf("go.mod: %q", gomod)
	}
}

func TestFunctionName(t *testing.T) {
	for in, want := range map[string]string{
		"hello":                 "hello",
		"/tmp/My App":           "my-app",
		"--x--":                 "x",
		"___":                   "my-function",
		strings.Repeat("a", 70): strings.Repeat("a", 63),
	} {
		if got := functionName(in); got != want || !registry.ValidName(got) {
			t.Errorf("functionName(%q) = %q, want %q", in, got, want)
		}
	}
}
