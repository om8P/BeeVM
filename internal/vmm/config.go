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
	RootfsPath     string // base rootfs image; each VM gets its own copy
	ExtraBootArgs  string // appended to DefaultBootArgs, e.g. "beevm.mode=idle"
	VCPUs          int64  // default 1
	MemMiB         int64  // default 128
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
	for name, p := range map[string]string{"FirecrackerBin": c.FirecrackerBin, "KernelPath": c.KernelPath, "RootfsPath": c.RootfsPath} {
		if p == "" {
			errs = append(errs, fmt.Errorf("%s is required", name))
		} else if _, err := os.Stat(p); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
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

// SocketPath is Firecracker's API socket.
func (c Config) SocketPath() string { return filepath.Join(c.Dir(), "firecracker.sock") }

// ConsolePath is where guest serial output and Firecracker's own messages are written.
func (c Config) ConsolePath() string { return filepath.Join(c.Dir(), "console.log") }

// VMRootfsPath is this VM's private copy of the root filesystem.
func (c Config) VMRootfsPath() string { return filepath.Join(c.Dir(), "rootfs.ext4") }
