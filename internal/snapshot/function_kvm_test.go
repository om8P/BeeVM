//go:build kvm

package snapshot

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/om8P/BeeVM/internal/vmm"
)

// Real VMs: go test -tags kvm -v -run Function ./internal/snapshot/

func realManager(t *testing.T) *Manager {
	t.Helper()
	home := os.Getenv("BEEVM_HOME")
	if home == "" {
		h, _ := os.UserHomeDir()
		home = filepath.Join(h, ".beevm")
	}
	base := vmm.Config{
		FirecrackerBin: filepath.Join(home, "bin", "firecracker"),
		KernelPath:     filepath.Join(home, "images", "vmlinux"),
		RootfsPath:     filepath.Join(home, "images", "rootfs.ext4"),
	}
	py := filepath.Join(home, "images", "rootfs-python.ext4")
	for _, p := range []string{"/dev/kvm", base.FirecrackerBin, base.KernelPath, base.RootfsPath, py} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("skipping, %v (run `make setup` and `make images`)", err)
		}
	}
	m, err := New(filepath.Join(t.TempDir(), "snapshots"), base, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	m.SetRootfs("python", py)
	return m
}

func TestRealFunctionSnapshotPython(t *testing.T) {
	m := realManager(t)
	zipPath := filepath.Join(t.TempDir(), "fn.zip")
	f, _ := os.Create(zipPath)
	zw := zip.NewWriter(f)
	w, _ := zw.Create("handler.py")
	w.Write([]byte("print('imported')\ndef handler(e):\n    return {'ok': e}\n"))
	zw.Close()
	f.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	spec := FunctionSpec{Runtime: "python", Handler: "handler.handler", TenantID: 1, FunctionID: 1,
		CodeSHA256: "test", CodePath: zipPath, MemMiB: 128, VCPUs: 1}
	s, err := m.EnsureFunction(ctx, spec)
	if err != nil {
		t.Fatalf("EnsureFunction: %v", err)
	}
	t.Logf("function snapshot %s built in %d ms (memory file %d MiB)", s.Key, s.BuildMS, s.MemBytes>>20)

	// A VM restored from it answers at once: the code is already imported.
	cfg := m.vmConfig(s.Inputs)
	cfg.BaseDir = t.TempDir()
	vm, _, err := vmm.Restore(ctx, cfg, s.Files)
	if err != nil {
		t.Fatal(err)
	}
	defer vm.Cleanup(false)
	info, err := vm.WaitAndResume(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out, err := vm.Agent().Invoke(ctx, []byte(`"hi"`), 5*time.Second)
	if err != nil || out.Error != "" || string(out.Output) != `{"ok":"hi"}` {
		t.Fatalf("invoke: %+v %v", out, err)
	}
	t.Logf("ready after %v, first call %d ms", info.Ready, out.DurationMS)
}
