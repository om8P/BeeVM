//go:build kvm

package snapshot

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/om8P/BeeVM/internal/vmm"
)

func realBase(t *testing.T) vmm.Config {
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
	for _, p := range []string{"/dev/kvm", base.FirecrackerBin, base.KernelPath, base.RootfsPath} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("skipping, %v", err)
		}
	}
	return base
}

func TestBuildRealSnapshotAndRestore(t *testing.T) {
	m, err := New(t.TempDir(), realBase(t), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, err := m.Ensure(ctx, 128, 1)
	if err != nil {
		t.Fatal(err)
	}
	if s.MemBytes != 128<<20 {
		t.Errorf("memory file is %d bytes, want 128 MiB", s.MemBytes)
	}
	t.Logf("snapshot %s built in %d ms", s.Key, s.BuildMS)

	cfg := m.base
	cfg.BaseDir = t.TempDir()
	vm, tm, err := vmm.Restore(ctx, cfg, s.Files)
	if err != nil {
		t.Fatal(err)
	}
	defer vm.Cleanup(false)
	info, err := vm.WaitAndResume(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("restored: spawn %v, load %v, agent ready after %v (%d attempts)", tm.Spawn, tm.Load, info.Ready, info.Attempts)
	if list, _ := m.List(); len(list) != 1 || !list[0].Current {
		t.Errorf("list: %+v", list)
	}
}
