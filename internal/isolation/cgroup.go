package isolation

// cgroups v2: the kernel's way to limit what a group of processes may use.
//
// /sys/fs/cgroup is a tree of folders; each folder is a group, and its files are its settings
// and counters. A limit on a folder applies to everything below it. BeeVM uses
//
//	/sys/fs/cgroup/beevm/t<tenant>/<vm-id>     one leaf per VM (the firecracker process)
//	/sys/fs/cgroup/beevm/build/<vm-id>         VMs that build snapshots
//
// The jailer creates the leaf, writes the limits into it and moves itself in before it becomes
// firecracker. We create the parents and switch on the controllers (cpu, memory, pids) for
// their children. cgroup v2 rule: a folder with children holds no processes itself.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Limits are the cgroup limits of one VM.
type Limits struct {
	MemoryMax   int64 // bytes: guest RAM plus Firecracker's own memory
	SwapOff     bool  // memory.swap.max=0 (no swapping a VM out)
	PidsMax     int   // threads/processes of the firecracker process (not the guest's!)
	CPUQuotaUS  int64 // CPU time per period, in microseconds
	CPUPeriodUS int64
}

// limitsFor computes a VM's limits. cpuPercent is per vCPU: 100 = each vCPU may use one whole
// host CPU, 50 = half of one.
func limitsFor(memMiB, vcpus int64, cpuPercent, overheadMiB, pidsMax int, swapOff bool) Limits {
	const period = 100_000 // 100 ms, the kernel default
	return Limits{
		MemoryMax:   (memMiB + int64(overheadMiB)) << 20,
		SwapOff:     swapOff,
		PidsMax:     pidsMax,
		CPUQuotaUS:  vcpus * int64(cpuPercent) * period / 100,
		CPUPeriodUS: period,
	}
}

// args are the limits as jailer --cgroup arguments ("file=value").
func (l Limits) args() []string {
	a := []string{
		fmt.Sprintf("memory.max=%d", l.MemoryMax),
		fmt.Sprintf("pids.max=%d", l.PidsMax),
		fmt.Sprintf("cpu.max=%d %d", l.CPUQuotaUS, l.CPUPeriodUS),
	}
	if l.SwapOff {
		a = append(a, "memory.swap.max=0")
	}
	return a
}

// cgroups manages BeeVM's part of the cgroup tree.
type cgroups struct {
	root string // /sys/fs/cgroup
	top  string // "beevm"
}

func (c cgroups) path(rel string) string { return filepath.Join(c.root, rel) }

// tenantParent is the parent cgroup (relative to root) of a tenant's VMs.
func (c cgroups) tenantParent(tenantID int64) string {
	if tenantID <= 0 {
		return filepath.Join(c.top, "build")
	}
	return filepath.Join(c.top, fmt.Sprintf("t%d", tenantID))
}

// ensure creates rel and its parents below root and lets their children use cpu, memory and
// pids. Safe to call again.
func (c cgroups) ensure(rel string) error {
	cur := c.root
	for _, part := range strings.Split(filepath.Clean(rel), string(filepath.Separator)) {
		next := filepath.Join(cur, part)
		if err := os.Mkdir(next, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("create cgroup %s: %w", next, err)
		}
		// The parent must hand the controllers down before the child can use them.
		if err := enableControllers(cur); err != nil {
			return err
		}
		cur = next
	}
	return enableControllers(cur)
}

func enableControllers(dir string) error {
	data, err := os.ReadFile(filepath.Join(dir, "cgroup.subtree_control"))
	if err != nil {
		return fmt.Errorf("cgroup %s: %w", dir, err)
	}
	have := " " + strings.TrimSpace(string(data)) + " "
	var want []string
	for _, c := range []string{"cpu", "memory", "pids"} {
		if !strings.Contains(have, " "+c+" ") {
			want = append(want, "+"+c)
		}
	}
	if len(want) == 0 {
		return nil
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.subtree_control"), []byte(strings.Join(want, " ")), 0); err != nil {
		return fmt.Errorf("enable %s in cgroup %s: %w", strings.Join(want, " "), dir, err)
	}
	return nil
}

// removeLeaf deletes a VM's cgroup. A cgroup can only be removed when no process is left in
// it; if one is, cgroup.kill ends them all first.
func (c cgroups) removeLeaf(rel string) error {
	dir := c.path(rel)
	err := syscall.Rmdir(dir)
	if err == nil || errors.Is(err, syscall.ENOENT) {
		return nil
	}
	if !errors.Is(err, syscall.EBUSY) {
		return fmt.Errorf("remove cgroup %s: %w", dir, err)
	}
	_ = os.WriteFile(filepath.Join(dir, "cgroup.kill"), []byte("1"), 0)
	for i := 0; i < 100; i++ {
		time.Sleep(10 * time.Millisecond)
		if err = syscall.Rmdir(dir); err == nil || errors.Is(err, syscall.ENOENT) {
			return nil
		}
	}
	return fmt.Errorf("remove cgroup %s: %w", dir, err)
}

// removeStaleLeaves deletes every VM cgroup below top (left over by a crashed server). It
// returns how many it removed.
func (c cgroups) removeStaleLeaves() (int, error) {
	parents, err := os.ReadDir(c.path(c.top))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, p := range parents {
		if !p.IsDir() {
			continue
		}
		leaves, _ := os.ReadDir(c.path(filepath.Join(c.top, p.Name())))
		for _, l := range leaves {
			if !l.IsDir() {
				continue
			}
			if err := c.removeLeaf(filepath.Join(c.top, p.Name(), l.Name())); err != nil {
				errs = append(errs, err)
			} else {
				n++
			}
		}
	}
	return n, errors.Join(errs...)
}

// readValue reads one cgroup file of a VM (for tests and reports).
func (c cgroups) readValue(rel, file string) (string, error) {
	data, err := os.ReadFile(filepath.Join(c.path(rel), file))
	return strings.TrimSpace(string(data)), err
}
