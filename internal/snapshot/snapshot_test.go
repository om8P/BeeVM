package snapshot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/om8P/BeeVM/internal/vmm"
)

// newTestManager returns a Manager with fake images, a fake Firecracker version and a fake
// build that just writes the two snapshot files. It counts builds.
func newTestManager(t *testing.T) (*Manager, *atomic.Int32) {
	t.Helper()
	img := t.TempDir()
	base := vmm.Config{
		FirecrackerBin: filepath.Join(img, "firecracker"),
		KernelPath:     filepath.Join(img, "vmlinux"),
		RootfsPath:     filepath.Join(img, "rootfs.ext4"),
	}
	for _, p := range []string{base.FirecrackerBin, base.KernelPath, base.RootfsPath} {
		if err := os.WriteFile(p, []byte("content of "+filepath.Base(p)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m, err := New(filepath.Join(t.TempDir(), "snapshots"), base, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.fcVersion = func() (string, error) { return "Firecracker v1.17.0", nil }
	builds := &atomic.Int32{}
	m.build = func(ctx context.Context, cfg vmm.Config, dir string) error {
		builds.Add(1)
		if !cfg.SharedRootfs || cfg.ExtraBootArgs != BootArgs {
			return errors.New("snapshot VMs must use the shared rootfs and agent mode")
		}
		time.Sleep(20 * time.Millisecond) // make parallel callers overlap
		_ = os.WriteFile(filepath.Join(dir, "vmstate"), []byte("state"), 0o644)
		return os.WriteFile(filepath.Join(dir, "mem"), make([]byte, cfg.MemMiB), 0o644)
	}
	return m, builds
}

func TestKeyChangesWithEveryInput(t *testing.T) {
	base := Inputs{FirecrackerVersion: "v1", KernelSHA256: "k", RootfsSHA256: "r", RootfsPath: "/r", BootArgs: "a", MemMiB: 128, VCPUs: 1}
	seen := map[string]string{Key(base): "base"}
	variants := map[string]Inputs{}
	for name, change := range map[string]func(*Inputs){
		"firecracker": func(i *Inputs) { i.FirecrackerVersion = "v2" },
		"kernel":      func(i *Inputs) { i.KernelSHA256 = "k2" },
		"rootfs":      func(i *Inputs) { i.RootfsSHA256 = "r2" },
		"rootfs path": func(i *Inputs) { i.RootfsPath = "/other" },
		"boot args":   func(i *Inputs) { i.BootArgs = "b" },
		"memory":      func(i *Inputs) { i.MemMiB = 256 },
		"vcpus":       func(i *Inputs) { i.VCPUs = 2 },
		"build":       func(i *Inputs) { i.BuildVersion++ },
	} {
		in := base
		change(&in)
		variants[name] = in
	}
	for name, in := range variants {
		k := Key(in)
		if prev, dup := seen[k]; dup {
			t.Errorf("%s gives the same key as %s", name, prev)
		}
		seen[k] = name
	}
	if Key(base) != Key(base) {
		t.Error("key is not stable")
	}
}

func TestEnsureBuildsOnceThenReuses(t *testing.T) {
	m, builds := newTestManager(t)
	ctx := context.Background()
	s1, err := m.Ensure(ctx, 128, 1)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := m.Ensure(ctx, 128, 1)
	if err != nil {
		t.Fatal(err)
	}
	if builds.Load() != 1 || s1.Key != s2.Key {
		t.Fatalf("builds=%d keys %s %s", builds.Load(), s1.Key, s2.Key)
	}
	if s1.MemBytes != 128 || s1.Inputs.MemMiB != 128 {
		t.Errorf("meta %+v", s1.Meta)
	}
	for _, p := range []string{s1.Files.MemPath, s1.Files.StatePath} {
		if _, err := os.Stat(p); err != nil {
			t.Error(err)
		}
	}
	// A different size is a different snapshot.
	s3, _ := m.Ensure(ctx, 256, 1)
	if s3.Key == s1.Key || builds.Load() != 2 {
		t.Errorf("256 MiB snapshot: key %s, builds %d", s3.Key, builds.Load())
	}
}

func TestParallelEnsureSharesOneBuild(t *testing.T) {
	m, builds := newTestManager(t)
	var wg sync.WaitGroup
	keys := make([]string, 10)
	for i := range keys {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := m.Ensure(context.Background(), 128, 1)
			if err != nil {
				t.Error(err)
			}
			keys[i] = s.Key
		}(i)
	}
	wg.Wait()
	if builds.Load() != 1 {
		t.Errorf("%d builds for 10 parallel calls, want 1", builds.Load())
	}
	for _, k := range keys {
		if k != keys[0] {
			t.Fatalf("different keys %v", keys)
		}
	}
}

func TestFailedBuildLeavesNothing(t *testing.T) {
	m, _ := newTestManager(t)
	m.build = func(ctx context.Context, cfg vmm.Config, dir string) error {
		_ = os.WriteFile(filepath.Join(dir, "mem"), []byte("half"), 0o644)
		return errors.New("vm did not start")
	}
	if _, err := m.Ensure(context.Background(), 128, 1); err == nil {
		t.Fatal("want an error")
	}
	entries, _ := os.ReadDir(m.Dir())
	for _, e := range entries {
		if e.Name() != "build" {
			t.Errorf("left behind: %s", e.Name())
		}
	}
	if list, _ := m.List(); len(list) != 0 {
		t.Errorf("listed %d snapshots after a failed build", len(list))
	}
}

func TestChangedRootfsMeansNewSnapshotAndOldOneIsStale(t *testing.T) {
	m, builds := newTestManager(t)
	ctx := context.Background()
	old, _ := m.Ensure(ctx, 128, 1)

	// "make images" rebuilt the rootfs: new content (and a new modification time).
	later := time.Now().Add(time.Minute)
	_ = os.WriteFile(m.base.RootfsPath, []byte("new rootfs"), 0o644)
	_ = os.Chtimes(m.base.RootfsPath, later, later)

	cur, _ := m.Ensure(ctx, 128, 1)
	if cur.Key == old.Key || builds.Load() != 2 {
		t.Fatalf("rootfs change did not lead to a new snapshot (builds %d)", builds.Load())
	}
	list, err := m.List()
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %v %d", err, len(list))
	}
	for _, s := range list {
		if s.Current != (s.Key == cur.Key) {
			t.Errorf("snapshot %s current=%v", s.Key, s.Current)
		}
	}
	n, err := m.RemoveStale()
	if err != nil || n != 1 {
		t.Fatalf("RemoveStale: %d, %v", n, err)
	}
	if list, _ := m.List(); len(list) != 1 || list[0].Key != cur.Key {
		t.Errorf("after RemoveStale: %+v", list)
	}
}

func TestIgnoresUnfinishedFolders(t *testing.T) {
	m, _ := newTestManager(t)
	_ = os.MkdirAll(filepath.Join(m.Dir(), "tmp-1234"), 0o755)
	_ = os.WriteFile(filepath.Join(m.Dir(), "tmp-1234", "mem"), []byte("x"), 0o644)
	// A folder named like a key but without meta.json (for example copied by hand, half-way).
	_ = os.MkdirAll(filepath.Join(m.Dir(), "abc"), 0o755)
	if list, _ := m.List(); len(list) != 0 {
		t.Errorf("listed %d", len(list))
	}
}

func TestRemove(t *testing.T) {
	m, _ := newTestManager(t)
	s, _ := m.Ensure(context.Background(), 128, 1)
	if err := m.Remove(s.Key); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(s.Key); !errors.Is(err, ErrNotFound) {
		t.Errorf("second remove: %v", err)
	}
	for _, bad := range []string{"", "../x", "a/b", "."} {
		if err := m.Remove(bad); err == nil {
			t.Errorf("Remove(%q) should fail", bad)
		}
	}
}

func TestEnsureSurvivesCallerCancel(t *testing.T) {
	m, builds := newTestManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the caller is already gone; the shared build must still finish
	if _, err := m.Ensure(ctx, 128, 1); err != nil {
		t.Fatalf("Ensure with a cancelled caller: %v", err)
	}
	if builds.Load() != 1 {
		t.Errorf("builds %d", builds.Load())
	}
}

func TestInputsCarryBuildVersion(t *testing.T) {
	m, _ := newTestManager(t)
	in, err := m.Inputs(128, 1)
	if err != nil {
		t.Fatal(err)
	}
	if in.BuildVersion != BuildVersion || BuildVersion < 2 {
		t.Errorf("build version %d, want %d (>= 2)", in.BuildVersion, BuildVersion)
	}
	old := in
	old.BuildVersion = 1
	if Key(old) == Key(in) {
		t.Error("a snapshot made the old way must get a different key")
	}
}

func TestSetSettle(t *testing.T) {
	m, _ := newTestManager(t)
	if m.settle != DefaultSettle {
		t.Errorf("default settle %v", m.settle)
	}
	m.SetSettle(2 * time.Second)
	if m.settle != 2*time.Second {
		t.Errorf("settle %v", m.settle)
	}
}
