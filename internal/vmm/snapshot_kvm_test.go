//go:build kvm

package vmm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// makeSnapshot boots a base VM (shared read-only rootfs, agent ready), lets it settle,
// snapshots it and throws the VM away. It returns the snapshot files.
func makeSnapshot(t *testing.T) SnapshotFiles {
	t.Helper()
	cfg := kvmConfig(t)
	cfg.SharedRootfs = true
	cfg.ExtraBootArgs = "beevm.mode=agent"
	vm, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer vm.Cleanup(false)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := vm.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := vm.WaitForAgent(ctx); err != nil {
		b, _ := os.ReadFile(vm.ConsolePath())
		t.Fatalf("agent: %v\n%s", err, b)
	}
	// A snapshot taken right after the agent starts makes the first reply after every restore
	// slow (~850 ms on this machine); letting the guest settle for 0.5 s or more avoids it.
	time.Sleep(time.Second)
	vm.Agent().Close() // open vsock connections do not survive a snapshot
	dir := t.TempDir()
	snap := SnapshotFiles{StatePath: filepath.Join(dir, "vmstate"), MemPath: filepath.Join(dir, "mem")}
	if err := vm.Pause(ctx); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := vm.CreateSnapshot(ctx, snap.MemPath, snap.StatePath); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return snap
}

// restored restores a VM from snap, waits for its agent, tells it it was resumed and uploads fn.
func restored(t *testing.T, snap SnapshotFiles, fn string) (*VM, RestoreTimings, time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	vm, tm, err := Restore(ctx, kvmConfig(t), snap)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	t.Cleanup(func() { _ = vm.Cleanup(false) })
	info, err := vm.WaitAndResume(ctx)
	if err != nil {
		b, _ := os.ReadFile(vm.ConsolePath())
		t.Fatalf("agent after restore: %v\n%s", err, b)
	}
	ready := info.Ready
	if _, err := vm.Agent().UploadFunctionFile(ctx, fn); err != nil {
		t.Fatalf("upload: %v", err)
	}
	return vm, tm, ready
}

// call runs the uploaded function once and decodes its JSON output.
func call(t *testing.T, vm *VM, payload string) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := vm.Agent().Invoke(ctx, json.RawMessage(payload), 5*time.Second)
	if err != nil {
		t.Fatalf("invoke %s: %v", payload, err)
	}
	if res.Error != "" {
		t.Fatalf("invoke %s: function error %s (%s)", payload, res.Error, res.Stderr)
	}
	var out map[string]any
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatalf("output %s: %v", res.Output, err)
	}
	return out
}

func TestSnapshotRestoreRunsFunction(t *testing.T) {
	snap := makeSnapshot(t)
	hello := buildFunction(t, "hello-go-binary")
	for i := 0; i < 10; i++ {
		vm, tm, ready := restored(t, snap, hello)
		if out := call(t, vm, `{"name":"snap"}`); out["greeting"] != "Hello, snap!" {
			t.Fatalf("restore %d got %v", i, out)
		}
		t.Logf("restore %d: spawn %v, load %v, agent ready %v after spawn", i, tm.Spawn, tm.Load, ready)
		if ready > 2*time.Second {
			t.Errorf("restore %d: agent took %v to answer", i, ready)
		}
		_ = vm.Cleanup(false)
	}
}

func TestRestoreWithMetricsAndPinning(t *testing.T) {
	snap := makeSnapshot(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg := kvmConfig(t)
	cfg.FirecrackerMetrics, cfg.CPUs = true, []int{0}
	vm, tm, err := Restore(ctx, cfg, snap)
	if err != nil {
		t.Fatal(err)
	}
	defer vm.Cleanup(false)
	if _, err := vm.WaitAndResume(ctx); err != nil {
		t.Fatal(err)
	}
	fc, err := vm.FirecrackerLoadTime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("load: ours %v, firecracker's %v", tm.Load, fc)
	if fc <= 0 || fc > tm.Load {
		t.Errorf("firecracker's load time %v should be positive and within ours (%v)", fc, tm.Load)
	}
	// Every thread of the process (incl. the vCPU thread created during the load) is pinned.
	tasks, _ := filepath.Glob(fmt.Sprintf("/proc/%d/task/*/status", vm.PID()))
	if len(tasks) < 2 {
		t.Fatalf("found %d firecracker threads", len(tasks))
	}
	for _, p := range tasks {
		b, _ := os.ReadFile(p)
		if !strings.Contains(string(b), "Cpus_allowed_list:\t0\n") {
			t.Errorf("%s is not pinned to CPU 0", p)
		}
	}
}

func TestParallelRestoresShareOneSnapshot(t *testing.T) {
	snap := makeSnapshot(t)
	testfn := buildFunction(t, "testfn")
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			vm, _, ready := restored(t, snap, testfn)
			out := call(t, vm, fmt.Sprintf(`{"mode":"echo","n":%d}`, i))
			echo, _ := out["echo"].(map[string]any)
			if echo == nil || echo["n"] != float64(i) {
				t.Errorf("call %d got %v", i, out)
			}
			t.Logf("parallel restore %d: agent ready after %v", i, ready)
		}(i)
	}
	wg.Wait()
}

func TestRestoredGuestIsFreshAndReadOnly(t *testing.T) {
	snap := makeSnapshot(t)
	testfn := buildFunction(t, "testfn")
	time.Sleep(2 * time.Second) // the snapshot's clock is now at least 2 s old
	a, _, _ := restored(t, snap, testfn)
	b, _, _ := restored(t, snap, testfn)

	// Clock: Resume set it to the host's time.
	guest := call(t, a, `{"mode":"time"}`)["unix_ms"].(float64)
	if diff := float64(time.Now().UnixMilli()) - guest; diff < -1000 || diff > 1000 {
		t.Errorf("guest clock is %.0f ms off the host after resume", diff)
	}
	// Random numbers: two copies of the same snapshot must not produce the same bytes.
	ra, rb := call(t, a, `{"mode":"random"}`)["hex"], call(t, b, `{"mode":"random"}`)["hex"]
	if ra == rb {
		t.Errorf("both restored VMs produced the same random bytes %v", ra)
	}
	// Disk: the shared root is read-only; /tmp is RAM and writable.
	if out := call(t, a, `{"mode":"writefile","path":"/hello.txt"}`); !strings.Contains(fmt.Sprint(out["error"]), "read-only") {
		t.Errorf("write to / should fail with read-only, got %v", out)
	}
	if out := call(t, a, `{"mode":"writefile","path":"/tmp/hello.txt"}`); out["ok"] != true {
		t.Errorf("write to /tmp should work, got %v", out)
	}
	// What one VM writes to /tmp, the other does not see.
	if out := call(t, b, `{"mode":"readfile","path":"/tmp/hello.txt"}`); out["error"] == nil {
		t.Errorf("VM b sees VM a's /tmp file: %v", out)
	}
}
