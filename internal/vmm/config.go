// Package vmm starts, stops and cleans up Firecracker MicroVMs.
//
// It is a thin layer over firecracker-go-sdk. It never reads environment variables:
// the caller passes every path in Config.
package vmm

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultBootArgs are the kernel arguments every BeeVM guest gets.
//
//	console=ttyS0  kernel messages go to the serial port (we capture it in console.log)
//	reboot=k       a guest reboot becomes "exit" for Firecracker
//	panic=1        reboot 1 s after a kernel crash instead of hanging
//	pci=off        Firecracker has no PCI bus
//	init=...       run our script as PID 1
const DefaultBootArgs = "console=ttyS0 reboot=k panic=1 pci=off init=/sbin/beevm-init"

// Config describes one MicroVM. Zero values for ID, VCPUs and MemMiB get defaults.
type Config struct {
	ID             string // short unique name; random if empty
	BaseDir        string // parent of the per-VM folder, e.g. ~/.beevm/run/vms
	FirecrackerBin string // path to the firecracker binary
	KernelPath     string // path to vmlinux
	RootfsPath     string // base rootfs image; each VM gets its own copy unless SharedRootfs
	SharedRootfs   bool   // all VMs use RootfsPath directly, read-only (no copy; needed for snapshots)
	ExtraBootArgs  string // appended to DefaultBootArgs, e.g. "beevm.mode=idle"
	VCPUs          int64  // default 1
	MemMiB         int64  // default 128
	VsockCID       uint32 // the guest's vsock address; default 3 (0-2 are reserved, 2 is the host)

	// Measuring and tuning (restored VMs only; used by the benchmark):
	FirecrackerMetrics bool  // Firecracker writes metrics.json in the VM folder (see VM.FirecrackerLoadTime)
	CPUs               []int // host CPUs the firecracker process may run on; empty = any

	// Isolation (milestone 9). All empty = no isolation: firecracker runs as this process's
	// user, in the host's network, without limits.
	TenantID int64     // whose VM this is (0 = nobody's, e.g. a snapshot build)
	Isolator Isolator  // when set, New and Restore ask it to fill in Jail and Net
	Jail     *Jail     // run firecracker through the jailer (chroot, own UID, cgroup, netns)
	Net      *NetIface // give the guest a network card
}

// Isolator sets up the isolation of one VM: it fills in cfg.Jail (and cfg.Net) and returns a
// function that undoes its setup. New and Restore call Prepare; VM.Cleanup calls release once
// the firecracker process is gone. The vmm package only knows this interface, so it never
// decides UIDs, cgroups or addresses itself.
type Isolator interface {
	Prepare(cfg *Config) (release func(), err error)
	// Name says how VMs are isolated, e.g. "jailer-net-1". Snapshots taken with one isolator
	// do not fit another (the paths inside them differ), so it is part of a snapshot's key.
	Name() string
}

// applyDefaults fills empty fields. It returns a copy; the caller's Config is untouched.
func (c Config) applyDefaults() (Config, error) {
	if c.ID == "" {
		b := make([]byte, 4)
		if _, err := rand.Read(b); err != nil {
			return c, fmt.Errorf("generate vm id: %w", err)
		}
		c.ID = hex.EncodeToString(b)
	}
	if c.VCPUs == 0 {
		c.VCPUs = 1
	}
	if c.MemMiB == 0 {
		c.MemMiB = 128
	}
	if c.VsockCID == 0 {
		c.VsockCID = 3
	}
	return c, nil
}

// Validate checks the config and that the files it names exist.
func (c Config) Validate() error {
	var errs []error
	if c.ID == "" || strings.ContainsAny(c.ID, `/\ `) || c.ID == "." || c.ID == ".." {
		errs = append(errs, fmt.Errorf("invalid ID %q (must be non-empty, no slashes or spaces)", c.ID))
	}
	if c.BaseDir == "" {
		errs = append(errs, errors.New("BaseDir is required"))
	}
	if c.VCPUs < 1 || c.VCPUs > 32 {
		errs = append(errs, fmt.Errorf("VCPUs must be 1..32, got %d", c.VCPUs))
	}
	if c.MemMiB < 64 {
		errs = append(errs, fmt.Errorf("MemMiB must be at least 64, got %d", c.MemMiB))
	}
	if c.VsockCID < 3 {
		errs = append(errs, fmt.Errorf("VsockCID must be at least 3, got %d", c.VsockCID))
	}
	for name, p := range map[string]string{"FirecrackerBin": c.FirecrackerBin, "KernelPath": c.KernelPath, "RootfsPath": c.RootfsPath} {
		if p == "" {
			errs = append(errs, fmt.Errorf("%s is required", name))
		} else if _, err := os.Stat(p); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	if c.Jail != nil {
		errs = append(errs, c.Jail.validate(c))
	}
	return errors.Join(errs...)
}

// BootArgs returns the full kernel command line.
func (c Config) BootArgs() string {
	if c.ExtraBootArgs == "" {
		return DefaultBootArgs
	}
	return DefaultBootArgs + " " + c.ExtraBootArgs
}

// Dir is this VM's private folder.
func (c Config) Dir() string { return filepath.Join(c.BaseDir, c.ID) }

// fcDir is the folder Firecracker works in: the VM folder, or with the jailer the jail's root
// (Firecracker's "/").
func (c Config) fcDir() string {
	if c.Jail != nil {
		return c.JailRoot()
	}
	return c.Dir()
}

// SocketPath is Firecracker's API socket (as the host sees it).
func (c Config) SocketPath() string { return filepath.Join(c.fcDir(), "firecracker.sock") }

// VsockPath is the Unix socket through which the host reaches the guest's vsock ports.
func (c Config) VsockPath() string { return filepath.Join(c.fcDir(), VsockRelPath) }

// VsockRelPath is the vsock socket's name as Firecracker is told it: relative to the VM folder,
// which is Firecracker's working directory. A snapshot stores this path, so a VM restored in
// another folder creates its socket in its own folder instead of the original VM's.
const VsockRelPath = "v.sock"

// MetricsPath is where Firecracker writes its metrics when FirecrackerMetrics is set.
func (c Config) MetricsPath() string { return filepath.Join(c.fcDir(), "metrics.json") }

// ConsolePath is where guest serial output and Firecracker's own messages are written.
func (c Config) ConsolePath() string { return filepath.Join(c.Dir(), "console.log") }

// VMRootfsPath is the root filesystem this VM uses: the shared read-only image, or its
// private copy.
func (c Config) VMRootfsPath() string {
	if c.SharedRootfs {
		return c.RootfsPath
	}
	return filepath.Join(c.Dir(), "rootfs.ext4")
}
