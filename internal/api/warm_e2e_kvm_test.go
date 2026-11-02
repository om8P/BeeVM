//go:build kvm

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/om8P/BeeVM/internal/apiclient"
	"github.com/om8P/BeeVM/internal/apitypes"
	"github.com/om8P/BeeVM/internal/pool"
	"github.com/om8P/BeeVM/internal/runner"
	"github.com/om8P/BeeVM/internal/snapshot"
	"github.com/om8P/BeeVM/internal/vmm"
)

// End to end with the warm pool: go test -tags kvm -v -run E2EWarm ./internal/api/

// warmServer starts the API with a WarmRunner (snapshot built first, as serve does).
func warmServer(t *testing.T, maxVMs int, keepAlive time.Duration) (*apiclient.Client, *runner.WarmRunner, string) {
	t.Helper()
	var w *runner.WarmRunner
	var snaps *snapshot.Manager
	c, _, vmBase := e2eServerWith(t, func(base vmm.Config) runner.Runner {
		m, err := snapshot.New(t.TempDir(), base, t.Logf)
		if err != nil {
			t.Fatal(err)
		}
		snaps = m
		w = runner.NewWarmRunner(base, m, pool.Config{MaxVMs: maxVMs, KeepAlive: keepAlive, QueueWait: 60 * time.Second},
			slog.New(slog.NewTextHandler(io.Discard, nil)))
		return w
	})
	t.Cleanup(func() { _ = w.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := snaps.Ensure(ctx, 128, 1); err != nil {
		t.Fatal(err)
	}
	return c, w, vmBase
}

func deployFile(t *testing.T, c *apiclient.Client, name, path string, cfg apitypes.FunctionConfig) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, _, err := c.Deploy(context.Background(), name, f, cfg); err != nil {
		t.Fatal(err)
	}
}

func waitVMs(t *testing.T, c *apiclient.Client, want int, within time.Duration) []apitypes.VM {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		vms, err := c.VMs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(vms) == want {
			return vms
		}
		if time.Now().After(deadline) {
			t.Fatalf("want %d VMs, have %+v", want, vms)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestE2EWarmRepeatCallsReuseTheVM(t *testing.T) {
	c, w, vmBase := warmServer(t, 4, 700*time.Millisecond)
	ctx := context.Background()
	deployFile(t, c, "hello", buildFn(t, "hello-go-binary"), apitypes.FunctionConfig{})

	first, err := c.Invoke(ctx, "hello", []byte(`{"name":"one"}`))
	if err != nil || first.StartKind != "restore" {
		t.Fatalf("first call: %+v, %v", first, err)
	}
	var totals []int64
	for i := 0; i < 5; i++ {
		res, err := c.Invoke(ctx, "hello", []byte(`{"name":"again"}`))
		if err != nil {
			t.Fatal(err)
		}
		if res.StartKind != "warm" || res.VMID != first.VMID || !strings.Contains(string(res.Output), "Hello, again!") {
			t.Fatalf("call %d: %s start %q vm %s (first vm %s)", i, res.Output, res.StartKind, res.VMID, first.VMID)
		}
		totals = append(totals, res.TotalMS)
	}
	t.Logf("restore call: server total %d ms (ready %d, upload %d); warm calls: %v ms", first.TotalMS, first.BootMS, first.UploadMS, totals)
	for _, ms := range totals {
		if ms >= first.TotalMS {
			t.Errorf("a warm call (%d ms) was not faster than the restore (%d ms)", ms, first.TotalMS)
		}
	}
	vms := waitVMs(t, c, 1, time.Second)
	if vms[0].State != "idle" || vms[0].Function != "hello" || vms[0].Calls != 6 {
		t.Errorf("vm list %+v", vms)
	}

	// After the keep-alive the VM is reaped: no process, no folder.
	waitVMs(t, c, 0, 3*time.Second)
	waitNoVMLeft(t, vmBase)

	// Prewarm: the next call is warm straight away.
	res, err := c.Prewarm(ctx, "hello", 2)
	if err != nil || res.Started != 2 {
		t.Fatalf("prewarm: %+v, %v", res, err)
	}
	if r, _ := c.Invoke(ctx, "hello", nil); r.StartKind != "warm" {
		t.Errorf("call after prewarm: %s", r.StartKind)
	}
	// Redeploy drains: the next call gets a fresh VM with the new code.
	deployFile(t, c, "hello", buildFn(t, "testfn"), apitypes.FunctionConfig{})
	r, err := c.Invoke(ctx, "hello", []byte(`{"mode":"echo"}`))
	if err != nil || r.StartKind != "restore" || !strings.Contains(string(r.Output), "echo") {
		t.Errorf("after redeploy: %+v, %v", r, err)
	}
	// Delete drains too.
	if err := c.Delete(ctx, "hello"); err != nil {
		t.Fatal(err)
	}
	waitVMs(t, c, 0, 3*time.Second)
	_ = w.Close()
	waitNoVMLeft(t, vmBase)
}

// Two functions with the same code never share a VM: a file one leaves in /tmp is invisible
// to the other, while the same function sees it again on its warm VM.
func TestE2EWarmFunctionsDoNotShareVMs(t *testing.T) {
	c, w, vmBase := warmServer(t, 4, time.Minute)
	ctx := context.Background()
	code := buildFn(t, "testfn")
	deployFile(t, c, "a", code, apitypes.FunctionConfig{})
	deployFile(t, c, "b", code, apitypes.FunctionConfig{})

	if _, err := c.Invoke(ctx, "a", []byte(`{"mode":"writefile","path":"/tmp/secret"}`)); err != nil {
		t.Fatal(err)
	}
	same, err := c.Invoke(ctx, "a", []byte(`{"mode":"readfile","path":"/tmp/secret"}`))
	if err != nil || same.StartKind != "warm" || strings.Contains(string(same.Output), `"error"`) {
		t.Errorf("a should find its own file on its warm VM: %s (%s), %v", same.Output, same.StartKind, err)
	}
	other, err := c.Invoke(ctx, "b", []byte(`{"mode":"readfile","path":"/tmp/secret"}`))
	if err != nil || other.StartKind == "warm" || !strings.Contains(string(other.Output), `"error"`) {
		t.Errorf("b must not see a's file: %s (%s), %v", other.Output, other.StartKind, err)
	}
	_ = w.Close()
	waitNoVMLeft(t, vmBase)
}

// A function that times out leaves a VM that is not trusted again; a function that fails
// normally keeps its VM.
func TestE2EWarmTimeoutRecyclesFailureKeeps(t *testing.T) {
	c, w, vmBase := warmServer(t, 2, time.Minute)
	ctx := context.Background()
	deployFile(t, c, "t", buildFn(t, "testfn"), apitypes.FunctionConfig{TimeoutMS: 1000})

	first, err := c.Invoke(ctx, "t", []byte(`{"mode":"echo"}`))
	if err != nil {
		t.Fatal(err)
	}
	var ae *apiclient.APIError
	if _, err := c.Invoke(ctx, "t", []byte(`{"mode":"fail"}`)); !errors.As(err, &ae) || ae.Status != 502 {
		t.Fatalf("fail: %v", err)
	}
	if r, _ := c.Invoke(ctx, "t", []byte(`{"mode":"echo"}`)); r.StartKind != "warm" || r.VMID != first.VMID {
		t.Errorf("after a normal failure the VM should stay: %+v", r)
	}
	if _, err := c.Invoke(ctx, "t", []byte(`{"mode":"sleep","ms":3000}`)); !errors.As(err, &ae) || ae.Status != 502 {
		t.Fatalf("timeout: %v", err)
	}
	r, err := c.Invoke(ctx, "t", []byte(`{"mode":"echo"}`))
	if err != nil || r.StartKind == "warm" || r.VMID == first.VMID {
		t.Errorf("after a timeout the VM must be replaced: %+v, %v", r, err)
	}
	_ = w.Close()
	waitNoVMLeft(t, vmBase)
}

func TestE2EWarmParallelCallsWithTwoSlots(t *testing.T) {
	c, w, vmBase := warmServer(t, 2, time.Minute)
	ctx := context.Background()
	deployFile(t, c, "hello", buildFn(t, "hello-go-binary"), apitypes.FunctionConfig{})

	var wg sync.WaitGroup
	var mu sync.Mutex
	kinds := map[string]int{}
	vms := map[string]bool{}
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := c.Invoke(ctx, "hello", []byte(`{"name":"p"}`))
			if err != nil {
				t.Errorf("parallel call: %v", err)
				return
			}
			var out map[string]string
			if json.Unmarshal(res.Output, &out) != nil || out["greeting"] != "Hello, p!" {
				t.Errorf("output %s", res.Output)
			}
			mu.Lock()
			kinds[res.StartKind]++
			vms[res.VMID] = true
			mu.Unlock()
		}()
	}
	wg.Wait()
	t.Logf("start kinds %v, VMs used %d", kinds, len(vms))
	if len(vms) > 2 {
		t.Errorf("%d VMs used with 2 slots", len(vms))
	}
	if kinds["warm"] < 8 {
		t.Errorf("most calls should be warm: %v", kinds)
	}
	_ = w.Close()
	waitNoVMLeft(t, vmBase)
}

// waitNoVMLeft is noVMLeft after giving background destroys a moment.
func waitNoVMLeft(t *testing.T, vmBase string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		entries, _ := os.ReadDir(vmBase)
		if len(entries) == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	noVMLeft(t, vmBase)
}
