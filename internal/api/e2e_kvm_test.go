//go:build kvm

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/om8P/BeeVM/internal/apiclient"
	"github.com/om8P/BeeVM/internal/apitypes"
	"github.com/om8P/BeeVM/internal/codestore"
	"github.com/om8P/BeeVM/internal/registry"
	"github.com/om8P/BeeVM/internal/runner"
	"github.com/om8P/BeeVM/internal/vmm"
)

// End to end: a real API server, the real ColdRunner and real Firecracker VMs, driven by the real
// client library. Run with: go test -tags kvm -v -run E2E ./internal/api/

func buildFn(t *testing.T, name string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), name)
	cmd := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", out, "../../examples/functions/"+name)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", name, err, b)
	}
	return out
}

func e2eServer(t *testing.T, maxVMs int, queueWait time.Duration) (*apiclient.Client, string, string) {
	t.Helper()
	home := os.Getenv("BEEVM_HOME")
	if home == "" {
		h, _ := os.UserHomeDir()
		home = filepath.Join(h, ".beevm")
	}
	vmBase := t.TempDir()
	base := vmm.Config{
		BaseDir:        vmBase,
		FirecrackerBin: filepath.Join(home, "bin", "firecracker"),
		KernelPath:     filepath.Join(home, "images", "vmlinux"),
		RootfsPath:     filepath.Join(home, "images", "rootfs.ext4"),
	}
	for _, p := range []string{"/dev/kvm", base.FirecrackerBin, base.KernelPath, base.RootfsPath} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("skipping, %v (run `make setup`?)", err)
		}
	}
	reg, err := registry.Open(filepath.Join(t.TempDir(), "e2e.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	code, _ := codestore.New(t.TempDir())
	ts := httptest.NewServer(New(Deps{
		Registry: reg, Code: code, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Runner: runner.NewColdRunner(base, maxVMs, queueWait),
	}).Handler())
	t.Cleanup(ts.Close)

	tn, _ := reg.CreateTenant("alice")
	key, _, _ := reg.CreateKey(tn.ID)
	return apiclient.New(ts.URL, key), ts.URL, vmBase
}

func noVMLeft(t *testing.T, vmBase string) {
	t.Helper()
	if entries, _ := os.ReadDir(vmBase); len(entries) != 0 {
		t.Errorf("VM folders left behind: %v", entries)
	}
	if out, _ := exec.Command("pgrep", "-f", vmBase).Output(); strings.TrimSpace(string(out)) != "" {
		t.Errorf("firecracker still running for %s: %s", vmBase, out)
	}
}

func TestE2EDeployAndInvoke(t *testing.T) {
	c, url, vmBase := e2eServer(t, 2, 5*time.Second)
	ctx := context.Background()

	// Without a key: 401, before any VM is started.
	var ae *apiclient.APIError
	if _, err := apiclient.New(url, "").Invoke(ctx, "hello", nil); !errors.As(err, &ae) || ae.Status != 401 {
		t.Errorf("no key: want 401, got %v", err)
	}

	hello, err := os.Open(buildFn(t, "hello-go-binary"))
	if err != nil {
		t.Fatal(err)
	}
	defer hello.Close()
	if _, created, err := c.Deploy(ctx, "hello", hello, apitypes.FunctionConfig{}); err != nil || !created {
		t.Fatalf("deploy: created=%v err=%v", created, err)
	}

	start := time.Now()
	res, err := c.Invoke(ctx, "hello", []byte(`{"name":"Om"}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	t.Logf("invoke over HTTP took %v (boot %d ms, function %d ms, server total %d ms)", time.Since(start), res.BootMS, res.FunctionMS, res.TotalMS)
	var out map[string]string
	if err := json.Unmarshal(res.Output, &out); err != nil || out["greeting"] != "Hello, Om!" || out["runtime"] != "binary" {
		t.Errorf("output %s (%v)", res.Output, err)
	}
	if res.VMID == "" || res.BootMS <= 0 {
		t.Errorf("timings missing: %+v", res)
	}

	// A different tenant cannot call it.
	other := apiclient.New(url, "bvm_notakey")
	if _, err := other.Invoke(ctx, "hello", nil); !errors.As(err, &ae) || ae.Status != 401 {
		t.Errorf("wrong key: %v", err)
	}
	noVMLeft(t, vmBase)
}

func TestE2EFailingAndSlowFunctions(t *testing.T) {
	c, _, vmBase := e2eServer(t, 2, 5*time.Second)
	ctx := context.Background()
	fn, _ := os.Open(buildFn(t, "testfn"))
	defer fn.Close()
	if _, _, err := c.Deploy(ctx, "t", fn, apitypes.FunctionConfig{TimeoutMS: 800}); err != nil {
		t.Fatal(err)
	}

	var ae *apiclient.APIError
	_, err := c.Invoke(ctx, "t", []byte(`{"mode":"fail"}`))
	if !errors.As(err, &ae) || ae.Status != 502 || ae.ExitCode == nil || *ae.ExitCode != 3 || !strings.Contains(ae.Stderr, "boom") {
		t.Errorf("fail: %#v", err)
	}
	_, err = c.Invoke(ctx, "t", []byte(`{"mode":"sleep","ms":20000}`))
	if !errors.As(err, &ae) || ae.Status != 502 || !strings.Contains(ae.Message, "timeout") {
		t.Errorf("timeout: %#v", err)
	}
	// The service keeps working after failures.
	if res, err := c.Invoke(ctx, "t", []byte(`{"mode":"echo","n":1}`)); err != nil || !bytes.Contains(res.Output, []byte(`"n":1`)) {
		t.Errorf("after failures: %s %v", res.Output, err)
	}
	noVMLeft(t, vmBase)
}

func TestE2EConcurrentCallsAreLimitedNotLost(t *testing.T) {
	// Two VM slots, a long wait for a free slot: five parallel calls must all succeed, never more
	// than two at once.
	c, _, vmBase := e2eServer(t, 2, 60*time.Second)
	ctx := context.Background()
	fn, _ := os.Open(buildFn(t, "hello-go-binary"))
	defer fn.Close()
	if _, _, err := c.Deploy(ctx, "hello", fn, apitypes.FunctionConfig{}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 5)
	start := time.Now()
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := c.Invoke(ctx, "hello", []byte(`{"name":"x"}`)); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("a parallel call failed: %v", err)
	}
	t.Logf("5 calls through 2 slots took %v", time.Since(start))
	noVMLeft(t, vmBase)
}

func TestE2EBusyServerAnswers503(t *testing.T) {
	// One slot and almost no waiting: a second call while the first runs gets 503.
	c, _, vmBase := e2eServer(t, 1, 100*time.Millisecond)
	ctx := context.Background()
	fn, _ := os.Open(buildFn(t, "testfn"))
	defer fn.Close()
	if _, _, err := c.Deploy(ctx, "t", fn, apitypes.FunctionConfig{}); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { _, err := c.Invoke(ctx, "t", []byte(`{"mode":"sleep","ms":1500}`)); done <- err }()
	time.Sleep(400 * time.Millisecond)

	var ae *apiclient.APIError
	_, err := c.Invoke(ctx, "t", []byte(`{"mode":"echo"}`))
	if !errors.As(err, &ae) || ae.Status != 503 {
		t.Errorf("want 503, got %v", err)
	}
	if err := <-done; err != nil {
		t.Errorf("first call: %v", err)
	}
	noVMLeft(t, vmBase)
}
