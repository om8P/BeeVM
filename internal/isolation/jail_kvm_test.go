//go:build kvm

package isolation

// Tests that need root and KVM: run them with `make test-isolation` (they skip otherwise).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/om8P/BeeVM/internal/snapshot"
	"github.com/om8P/BeeVM/internal/vmm"
)

// beevmHome is the real BEEVM_HOME (images must be on the same filesystem as the jails).
func beevmHome() string {
	if h := os.Getenv("BEEVM_HOME"); h != "" {
		return h
	}
	if su := os.Getenv("SUDO_USER"); su != "" {
		return filepath.Join("/home", su, ".beevm")
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".beevm")
}

// requireRoot skips unless the test runs as root with KVM, the jailer and the images.
func requireRoot(t *testing.T) string {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root: run make test-isolation")
	}
	home := beevmHome()
	for _, p := range []string{"/dev/kvm", filepath.Join(home, "bin", "jailer"), filepath.Join(home, "images", "vmlinux"),
		filepath.Join(home, "images", "rootfs.ext4")} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("skipping, %v", err)
		}
	}
	return home
}

// testManager is a Manager on the real BEEVM_HOME with a test lock file.
func testManager(t *testing.T, cpuPercent int) (*Manager, string) {
	t.Helper()
	home := requireRoot(t)
	m, err := New(Config{Home: home, CPUPercent: cpuPercent, LockPath: filepath.Join(t.TempDir(), "lock")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m, home
}

// workDir is a scratch folder below BEEVM_HOME (hard links need the same filesystem).
func workDir(t *testing.T, home string) string {
	t.Helper()
	d, err := os.MkdirTemp(filepath.Join(home, "run"), "isotest-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func baseConfig(home, dir string, iso vmm.Isolator) vmm.Config {
	return vmm.Config{
		BaseDir:        filepath.Join(dir, "vms"),
		FirecrackerBin: filepath.Join(home, "bin", "firecracker"),
		KernelPath:     filepath.Join(home, "images", "vmlinux"),
		RootfsPath:     filepath.Join(home, "images", "rootfs.ext4"),
		SharedRootfs:   true,
		ExtraBootArgs:  snapshot.BootArgs,
		Isolator:       iso,
		TenantID:       7,
	}
}

// procStatus reads fields of /proc/<pid>/task/<tid>/status for every thread.
func procStatus(t *testing.T, pid int) []map[string]string {
	t.Helper()
	tasks, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]string
	for _, task := range tasks {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%s/status", pid, task.Name()))
		if err != nil {
			continue
		}
		m := map[string]string{"tid": task.Name()}
		for _, line := range strings.Split(string(b), "\n") {
			k, v, ok := strings.Cut(line, ":")
			if ok {
				m[k] = strings.TrimSpace(v)
			}
		}
		out = append(out, m)
	}
	return out
}

// checkJailed checks what the jailer promises about a running firecracker process.
func checkJailed(t *testing.T, vm *vmm.VM) {
	t.Helper()
	cfg := vm.Config()
	pid := vm.PID()
	uid := strconv.Itoa(cfg.Jail.UID)
	for _, st := range procStatus(t, pid) {
		if f := strings.Fields(st["Uid"]); len(f) == 0 || f[0] != uid || f[1] != uid {
			t.Errorf("thread %s Uid %q, want %s", st["tid"], st["Uid"], uid)
		}
		if st["Seccomp"] != "2" {
			t.Errorf("thread %s Seccomp %q, want 2 (filter on)", st["tid"], st["Seccomp"])
		}
		if st["NoNewPrivs"] != "1" {
			t.Errorf("thread %s NoNewPrivs %q, want 1", st["tid"], st["NoNewPrivs"])
		}
		if st["CapEff"] != "0000000000000000" {
			t.Errorf("thread %s CapEff %q, want no capabilities", st["tid"], st["CapEff"])
		}
	}
	// The process's "/" is the jail: look at it from outside through /proc/<pid>/root.
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/root", pid))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	for _, n := range names {
		if n == "home" || n == "etc" || n == "usr" {
			t.Errorf("jail contains host folder %s: %v", n, names)
		}
	}
	cg, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil || !strings.Contains(string(cg), "/beevm/t7/"+cfg.ID) {
		t.Errorf("cgroup %q, want beevm/t7/%s", cg, cfg.ID)
	}
	t.Logf("pid %d uid %s, jail / = %v", pid, uid, names)
}

func callEcho(t *testing.T, vm *vmm.VM, testfn string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := vm.Agent().UploadFunctionFile(ctx, testfn); err != nil {
		t.Fatal(err)
	}
	out, err := vm.Agent().Invoke(ctx, json.RawMessage(`{"mode":"echo","x":42}`), 5*time.Second)
	if err != nil || out.Error != "" || !strings.Contains(string(out.Output), "42") {
		t.Fatalf("invoke: %v %+v", err, out)
	}
}

func testfnPath(t *testing.T) string {
	t.Helper()
	p, _ := filepath.Abs("../../bin/functions/testfn")
	if _, err := os.Stat(p); err != nil {
		t.Skipf("skipping, %v (run: make examples)", err)
	}
	return p
}

func TestJailedBootSnapshotRestore(t *testing.T) {
	m, home := testManager(t, 100)
	dir := workDir(t, home)
	testfn := testfnPath(t)

	// 1. Boot in a jail, call a function, check the process.
	cfg := baseConfig(home, dir, m)
	vm, err := vmm.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vm.Cleanup(false) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := vm.Start(ctx); err != nil {
		b, _ := os.ReadFile(vm.ConsolePath())
		t.Fatalf("start: %v\n%s", err, b)
	}
	if _, err := vm.WaitForAgent(ctx); err != nil {
		b, _ := os.ReadFile(vm.ConsolePath())
		t.Fatalf("agent: %v\n%s", err, b)
	}
	checkJailed(t, vm)
	callEcho(t, vm, testfn)

	// cgroup limits as configured.
	cgDir := m.CgroupDir(vm.Config())
	for file, want := range map[string]string{"memory.max": strconv.Itoa(192 << 20), "pids.max": "32", "cpu.max": "100000 100000"} {
		if got, _ := os.ReadFile(filepath.Join(cgDir, file)); strings.TrimSpace(string(got)) != want {
			t.Errorf("%s = %q, want %q", file, got, want)
		}
	}
	peak, _ := os.ReadFile(filepath.Join(cgDir, "memory.peak"))
	t.Logf("memory.peak after boot and one call: %s bytes", strings.TrimSpace(string(peak)))

	// 2. Snapshot it (Firecracker writes inside the jail, we move the files out).
	snapDir := filepath.Join(dir, "snap")
	_ = os.MkdirAll(snapDir, 0o755)
	vm.Agent().Close()
	if err := vm.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	files := vmm.SnapshotFiles{StatePath: filepath.Join(snapDir, "vmstate"), MemPath: filepath.Join(snapDir, "mem")}
	if err := vm.CreateSnapshot(ctx, files.MemPath, files.StatePath); err != nil {
		t.Fatal(err)
	}
	owner := m.Owner()
	for _, p := range []string{files.MemPath, files.StatePath} {
		o, err := ownerOf(p)
		if err != nil || o != owner {
			t.Errorf("%s owner %+v, want %+v (%v)", p, o, owner, err)
		}
	}
	jailDir := filepath.Dir(vm.Config().JailRoot())
	_ = vm.Cleanup(false)
	if _, err := os.Stat(jailDir); !os.IsNotExist(err) {
		t.Errorf("jail %s not removed", jailDir)
	}

	// 3. Two restores from the same files at once, each in its own jail with its own UID.
	var wg sync.WaitGroup
	uids := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rvm, tm, err := vmm.Restore(ctx, baseConfig(home, dir, m), files)
			if err != nil {
				t.Errorf("restore: %v", err)
				return
			}
			defer rvm.Cleanup(false)
			if _, err := rvm.WaitAndResume(ctx); err != nil {
				b, _ := os.ReadFile(rvm.ConsolePath())
				t.Errorf("resume: %v\n%s", err, b)
				return
			}
			checkJailed(t, rvm)
			callEcho(t, rvm, testfn)
			uids <- rvm.Config().Jail.UID
			t.Logf("restored %s: spawn %v load %v", rvm.ID(), tm.Spawn, tm.Load)
		}()
	}
	wg.Wait()
	close(uids)
	a, b := <-uids, <-uids
	if a == b {
		t.Errorf("two VMs share UID %d", a)
	}
	if n := m.InUse(); n != 0 {
		t.Errorf("%d slots still in use after cleanup", n)
	}
	assertNoLeftovers(t, m)
}

// TestJailedSnapshotManager builds a real runtime snapshot through the Snapshot Manager in a
// jail, and restores from it.
func TestJailedSnapshotManager(t *testing.T) {
	m, home := testManager(t, 100)
	dir := workDir(t, home)
	snaps, err := snapshot.New(filepath.Join(dir, "snapshots"), baseConfig(home, dir, m), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	s, err := snaps.Ensure(ctx, 128, 1)
	if err != nil {
		t.Fatal(err)
	}
	if s.Inputs.Isolation != m.Name() {
		t.Errorf("snapshot isolation %q, want %q", s.Inputs.Isolation, m.Name())
	}
	vm, _, err := vmm.Restore(ctx, baseConfig(home, dir, m), s.Files)
	if err != nil {
		t.Fatal(err)
	}
	defer vm.Cleanup(false)
	if _, err := vm.WaitAndResume(ctx); err != nil {
		t.Fatal(err)
	}
	callEcho(t, vm, testfnPath(t))
}

// assertNoLeftovers checks that no jail and no VM cgroup is left.
func assertNoLeftovers(t *testing.T, m *Manager) {
	t.Helper()
	if entries, _ := os.ReadDir(filepath.Join(m.jailBase(), "firecracker")); len(entries) > 0 {
		t.Errorf("%d jail folders left", len(entries))
	}
	tenants, _ := os.ReadDir(m.cg.path(m.cg.top))
	for _, tn := range tenants {
		if !tn.IsDir() {
			continue
		}
		leaves, _ := os.ReadDir(m.cg.path(filepath.Join(m.cg.top, tn.Name())))
		for _, l := range leaves {
			if l.IsDir() {
				t.Errorf("cgroup %s/%s left", tn.Name(), l.Name())
			}
		}
	}
}
