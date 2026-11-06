package isolation

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSlotsFIFO(t *testing.T) {
	s := newSlots(3)
	a, _ := s.get()
	b, _ := s.get()
	c, _ := s.get()
	if a != 0 || b != 1 || c != 2 {
		t.Fatalf("got %d %d %d", a, b, c)
	}
	if _, err := s.get(); err != ErrNoSlot {
		t.Fatalf("want ErrNoSlot, got %v", err)
	}
	s.put(b)
	s.put(a)
	s.put(a) // twice: ignored
	if n, _ := s.get(); n != b {
		t.Errorf("freed slots must come back first in, first out: got %d, want %d", n, b)
	}
	if n, _ := s.get(); n != a {
		t.Errorf("got %d, want %d", n, a)
	}
	if s.inUse() != 3 {
		t.Errorf("in use %d", s.inUse())
	}
}

func TestSlotsTake(t *testing.T) {
	s := newSlots(4)
	if !s.take(2) || s.take(2) {
		t.Fatal("take must reserve a free slot exactly once")
	}
	var got []int
	for {
		n, err := s.get()
		if err != nil {
			break
		}
		got = append(got, n)
	}
	if !slices.Equal(got, []int{0, 1, 3}) {
		t.Errorf("got %v", got)
	}
}

func TestSlotsPreferReadyNetworks(t *testing.T) {
	s := newSlots(4)
	s.markReady(2)
	if n, _ := s.get(); n != 2 {
		t.Errorf("got %d, want the slot whose network exists (2)", n)
	}
	if n, _ := s.get(); n != 0 {
		t.Errorf("then the first free one: got %d", n)
	}
	s.put(2)
	if n, _ := s.get(); n != 2 {
		t.Errorf("a freed ready slot comes back first: got %d", n)
	}
}

func TestLimits(t *testing.T) {
	l := limitsFor(128, 2, 50, 64, 32, true)
	if l.MemoryMax != 192<<20 {
		t.Errorf("memory.max %d, want 192 MiB", l.MemoryMax)
	}
	if l.CPUQuotaUS != 100_000 || l.CPUPeriodUS != 100_000 {
		t.Errorf("2 vCPUs at 50%% = one host CPU: got %d/%d", l.CPUQuotaUS, l.CPUPeriodUS)
	}
	want := []string{"memory.max=201326592", "pids.max=32", "cpu.max=100000 100000", "memory.swap.max=0"}
	if !slices.Equal(l.args(), want) {
		t.Errorf("args %q, want %q", l.args(), want)
	}
	if slices.Contains(limitsFor(128, 1, 100, 64, 32, false).args(), "memory.swap.max=0") {
		t.Error("swap limit without swap accounting")
	}
}

// fakeCgroupRoot makes a folder that looks enough like /sys/fs/cgroup for ensure.
func fakeCgroupRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "cgroup.subtree_control"), []byte("cpu memory pids\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestCgroupEnsure(t *testing.T) {
	root := fakeCgroupRoot(t)
	// A real cgroupfs creates the control files in a new folder; a temp folder does not, so
	// create them as the kernel would before ensure reads them.
	for _, d := range []string{"beevm", "beevm/t7"} {
		_ = os.MkdirAll(filepath.Join(root, d), 0o755)
		_ = os.WriteFile(filepath.Join(root, d, "cgroup.subtree_control"), nil, 0o644)
	}
	cg := cgroups{root: root, top: "beevm"}
	if err := cg.ensure(cg.tenantParent(7)); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"beevm", "beevm/t7"} {
		b, _ := os.ReadFile(filepath.Join(root, d, "cgroup.subtree_control"))
		if strings.TrimSpace(string(b)) != "+cpu +memory +pids" {
			t.Errorf("%s: controllers %q", d, b)
		}
	}
	if cg.tenantParent(0) != "beevm/build" {
		t.Errorf("tenant 0 = snapshot builds: %s", cg.tenantParent(0))
	}
}

func TestNewNeedsRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root")
	}
	if _, err := New(Config{Home: t.TempDir()}); err != ErrNotRoot {
		t.Errorf("want ErrNotRoot, got %v", err)
	}
}

func TestChownTreeNoChangeForOwnFiles(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "a", "b", "f"), nil, 0o644)
	owner, err := ownerOf(dir)
	if err != nil {
		t.Fatal(err)
	}
	n, err := ChownTree(dir, owner, filepath.Join(dir, "a", "b"))
	if err != nil || n != 0 {
		t.Errorf("changed %d, err %v; files already belong to the owner", n, err)
	}
}
