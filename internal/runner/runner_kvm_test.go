//go:build kvm

package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/om8P/BeeVM/internal/vmm"
)

// These tests boot real VMs: go test -tags kvm -v ./internal/runner/

func baseConfig(t *testing.T) vmm.Config {
	t.Helper()
	home := os.Getenv("BEEVM_HOME")
	if home == "" {
		h, _ := os.UserHomeDir()
		home = filepath.Join(h, ".beevm")
	}
	cfg := vmm.Config{
		BaseDir:        t.TempDir(),
		FirecrackerBin: filepath.Join(home, "bin", "firecracker"),
		KernelPath:     filepath.Join(home, "images", "vmlinux"),
		RootfsPath:     filepath.Join(home, "images", "rootfs.ext4"),
	}
	for _, p := range []string{"/dev/kvm", cfg.FirecrackerBin, cfg.KernelPath, cfg.RootfsPath} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("skipping, %v (run `make setup`?)", err)
		}
	}
	return cfg
}

func buildFunction(t *testing.T, name string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), name)
	cmd := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", out, "../../examples/functions/"+name)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", name, err, b)
	}
	return out
}

func leftovers(t *testing.T, cfg vmm.Config) {
	t.Helper()
	entries, _ := os.ReadDir(cfg.BaseDir)
	if len(entries) != 0 {
		t.Errorf("VM folders left behind: %v", entries)
	}
}

func TestColdRunnerRunsAFunction(t *testing.T) {
	cfg := baseConfig(t)
	r := NewColdRunner(cfg, 2, time.Second)
	code := buildFunction(t, "hello-go-binary")

	res, err := r.Run(context.Background(), Request{
		CodePath: code, Payload: json.RawMessage(`{"name":"Om"}`), MemoryMiB: 128, VCPUs: 1, TimeoutMS: 5000,
	})
	if err != nil || res.FunctionError != "" {
		t.Fatalf("Run: %+v, %v", res, err)
	}
	var out map[string]string
	if err := json.Unmarshal(res.Output, &out); err != nil || out["greeting"] != "Hello, Om!" {
		t.Errorf("output %s (%v)", res.Output, err)
	}
	if res.VMID == "" || res.BootMS <= 0 || res.TotalMS < res.BootMS {
		t.Errorf("timings: %+v", res)
	}
	if !strings.Contains(res.Stderr, "greeting Om") {
		t.Errorf("stderr %q", res.Stderr)
	}
	t.Logf("boot %d ms, function %d ms, total %d ms", res.BootMS, res.FunctionMS, res.TotalMS)
	leftovers(t, cfg)
}

func TestColdRunnerReportsFunctionFailure(t *testing.T) {
	cfg := baseConfig(t)
	r := NewColdRunner(cfg, 1, time.Second)
	res, err := r.Run(context.Background(), Request{
		CodePath: buildFunction(t, "testfn"), Payload: json.RawMessage(`{"mode":"fail"}`), MemoryMiB: 128, VCPUs: 1, TimeoutMS: 5000,
	})
	if err != nil {
		t.Fatalf("a failing function is not a runner error: %v", err)
	}
	if res.ExitCode != 3 || !strings.Contains(res.FunctionError, "code 3") || !strings.Contains(res.Stderr, "boom") {
		t.Errorf("result: %+v", res)
	}
	leftovers(t, cfg)
}

func TestColdRunnerBusyThenFree(t *testing.T) {
	cfg := baseConfig(t)
	r := NewColdRunner(cfg, 1, 200*time.Millisecond)
	code := buildFunction(t, "testfn")
	slow := Request{CodePath: code, Payload: json.RawMessage(`{"mode":"sleep","ms":1500}`), MemoryMiB: 128, VCPUs: 1, TimeoutMS: 5000}

	var wg sync.WaitGroup
	wg.Add(1)
	var firstErr error
	go func() { defer wg.Done(); _, firstErr = r.Run(context.Background(), slow) }()
	time.Sleep(300 * time.Millisecond) // let the first call take the only slot

	if _, err := r.Run(context.Background(), slow); !errors.Is(err, ErrBusy) {
		t.Errorf("second call: want ErrBusy, got %v", err)
	}
	wg.Wait()
	if firstErr != nil {
		t.Fatalf("first call: %v", firstErr)
	}
	// The slot is free again.
	res, err := r.Run(context.Background(), Request{CodePath: code, Payload: json.RawMessage(`{"mode":"echo"}`), MemoryMiB: 128, VCPUs: 1, TimeoutMS: 5000})
	if err != nil || res.FunctionError != "" {
		t.Errorf("after the first finished: %+v, %v", res, err)
	}
	leftovers(t, cfg)
}

func TestColdRunnerCancelKillsTheVM(t *testing.T) {
	cfg := baseConfig(t)
	r := NewColdRunner(cfg, 1, time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond) // shorter than a boot
	defer cancel()
	_, err := r.Run(ctx, Request{CodePath: buildFunction(t, "hello-go-binary"), Payload: json.RawMessage(`{}`), MemoryMiB: 128, VCPUs: 1, TimeoutMS: 5000})
	if err == nil {
		t.Fatal("expected an error")
	}
	leftovers(t, cfg)
	if out, _ := exec.Command("pgrep", "-f", cfg.BaseDir).Output(); len(strings.TrimSpace(string(out))) != 0 {
		t.Errorf("firecracker still running for %s: %s", cfg.BaseDir, out)
	}
}
