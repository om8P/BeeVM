//go:build kvm

package vmm

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// These tests boot real MicroVMs. Run them with:  go test -tags kvm -v ./internal/vmm/
// They use the images from `make setup` in $BEEVM_HOME (default ~/.beevm).

func kvmConfig(t *testing.T) Config {
	t.Helper()
	home := os.Getenv("BEEVM_HOME")
	if home == "" {
		h, _ := os.UserHomeDir()
		home = filepath.Join(h, ".beevm")
	}
	cfg := Config{
		BaseDir:        t.TempDir(),
		FirecrackerBin: filepath.Join(home, "bin", "firecracker"),
		KernelPath:     filepath.Join(home, "images", "vmlinux"),
		RootfsPath:     filepath.Join(home, "images", "rootfs.ext4"),
		ExtraBootArgs:  "beevm.mode=idle",
	}
	for _, p := range []string{"/dev/kvm", cfg.FirecrackerBin, cfg.KernelPath, cfg.RootfsPath} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("skipping, %v (run `make setup`?)", err)
		}
	}
	return cfg
}

func bootIdle(t *testing.T) *VM {
	t.Helper()
	vm, err := New(kvmConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vm.Cleanup(false) })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := vm.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	took, err := vm.WaitForConsole(ctx, "BEEVM_READY")
	if err != nil {
		b, _ := os.ReadFile(vm.ConsolePath())
		t.Fatalf("WaitForConsole: %v\n--- console ---\n%s", err, b)
	}
	t.Logf("VM %s (pid %d) ready after %v", vm.ID(), vm.PID(), took)
	return vm
}

func processAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func TestBootAndGracefulStop(t *testing.T) {
	vm := bootIdle(t)
	pid := vm.PID()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	graceful, err := vm.Stop(ctx)
	if err != nil || !graceful {
		t.Fatalf("Stop: graceful=%v err=%v", graceful, err)
	}
	t.Logf("graceful stop took %v", time.Since(start))

	if err := vm.Cleanup(false); err != nil {
		t.Fatal(err)
	}
	if processAlive(pid) {
		t.Errorf("firecracker pid %d still running", pid)
	}
	if _, err := os.Stat(vm.Dir()); !os.IsNotExist(err) {
		t.Error("vm folder was not removed")
	}
	// Stop and Kill after the VM is gone must be harmless.
	if _, err := vm.Stop(ctx); err != nil {
		t.Errorf("second Stop: %v", err)
	}
	vm.Kill()
}

func TestBootAndKill(t *testing.T) {
	vm := bootIdle(t)
	pid := vm.PID()

	vm.Kill()
	select {
	case <-vm.Exited():
	case <-time.After(5 * time.Second):
		t.Fatal("process did not exit after Kill")
	}
	if err := vm.Cleanup(false); err != nil {
		t.Fatal(err)
	}
	if processAlive(pid) {
		t.Errorf("firecracker pid %d still running", pid)
	}
	if _, err := os.Stat(vm.Dir()); !os.IsNotExist(err) {
		t.Error("vm folder was not removed")
	}
}

func TestStartWithCancelledContextLeavesNothingRunning(t *testing.T) {
	vm, err := New(kvmConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	if err := vm.Start(ctx); err == nil {
		t.Error("Start with a cancelled context should fail")
	}
	pid := vm.PID()
	if err := vm.Cleanup(false); err != nil {
		t.Fatal(err)
	}
	if pid != 0 && processAlive(pid) {
		t.Errorf("firecracker pid %d still running", pid)
	}
}
