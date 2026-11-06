package main

import (
	"flag"
	"fmt"
	"log/slog"

	"github.com/om8P/BeeVM/internal/isolation"
	"github.com/om8P/BeeVM/internal/vmm"
)

// isolationFlags are the --isolation settings shared by serve, invoke, bench and snapshot.
type isolationFlags struct {
	mode       *string
	cpuPercent *int
	uidBase    *int
	egress     *string
	prealloc   *int
}

func addIsolationFlags(fs *flag.FlagSet) isolationFlags {
	return isolationFlags{
		mode: fs.String("isolation", "none", "none (VMs run as you, no limits) or jailer (jailer + cgroups per VM; needs sudo)"),
		cpuPercent: fs.Int("cpu-limit", isolation.DefaultCPUPercent,
			"jailer: CPU each vCPU may use, in percent of one host CPU (cgroup cpu.max)"),
		uidBase: fs.Int("uid-base", isolation.DefaultUIDBase, "jailer: VM n runs as UID/GID uid-base+n"),
		egress: fs.String("egress", "none", "jailer: none (VMs reach only their tenant's VMs) or nat (also the internet; "+
			"turns on the host's IP forwarding while running)"),
		prealloc: fs.Int("net-prealloc", -1, "jailer: VM networks to create at start-up (-1 = enough for --max-vms)"),
	}
}

// open sets up isolation and adds it to base. It returns nil for --isolation none. vms is how
// many VMs may run at once (for --net-prealloc -1).
func (f isolationFlags) open(home string, base *vmm.Config, logger *slog.Logger, vms int) (*isolation.Manager, error) {
	switch *f.mode {
	case "none", "":
		return nil, nil
	case "jailer":
	default:
		return nil, fmt.Errorf("--isolation must be none or jailer, not %q", *f.mode)
	}
	if logger == nil {
		logger = slog.Default()
	}
	if *f.egress != "none" && *f.egress != "nat" {
		return nil, fmt.Errorf("--egress must be none or nat, not %q", *f.egress)
	}
	prealloc := *f.prealloc
	if prealloc < 0 {
		prealloc = vms + 1 // +1: a snapshot build next to a full pool
	}
	iso, err := isolation.New(isolation.Config{
		Home: home, CPUPercent: *f.cpuPercent, UIDBase: *f.uidBase, Logger: logger,
		Network: true, Egress: *f.egress == "nat", NetPrealloc: prealloc,
	})
	if err != nil {
		return nil, err
	}
	base.Isolator = iso
	return iso, nil
}

// closeIsolation is a nil-safe Close for deferring.
func closeIsolation(iso *isolation.Manager) {
	if iso != nil {
		_ = iso.Close()
	}
}
