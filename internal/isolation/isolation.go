// Package isolation keeps tenants' MicroVMs apart on the host (milestone 9).
//
// A MicroVM already gives every function its own kernel. This package adds the layers around
// the Firecracker process itself, so that even a guest that broke out into it gains little:
//
//   - the jailer: each Firecracker runs in its own chroot as its own unprivileged UID, with
//     Firecracker's seccomp filter on (see vmm/jail.go),
//   - cgroups v2: memory, CPU and process limits per VM, grouped per tenant,
//   - (with networking) its own network namespace, and nftables rules that only let a
//     tenant's VMs reach each other.
//
// The Manager implements vmm.Isolator: vmm.New and vmm.Restore call Prepare for every VM, and
// VM.Cleanup calls the returned release function. Everything here needs root, so it is only
// used by `beevmd serve --isolation jailer` (run with sudo).
package isolation

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/om8P/BeeVM/internal/netns"
	"github.com/om8P/BeeVM/internal/vmm"
)

// Defaults.
const (
	DefaultUIDBase        = 900000 // far from normal users and from /etc/subuid ranges (100000-165535)
	DefaultSlots          = 1024
	DefaultCPUPercent     = 100
	DefaultMemOverheadMiB = 64 // Firecracker's own memory on top of the guest's (measured: see docs)
	DefaultPidsMax        = 32 // firecracker has ~3 + vCPU threads; the guest's processes do not count
)

// ErrNotRoot means the process is not root; the jailer, cgroups and network namespaces need it.
var ErrNotRoot = errors.New("isolation needs root: run with sudo (e.g. sudo ./bin/beevmd serve --isolation jailer)")

// Config configures a Manager. Only Home is required.
type Config struct {
	Home           string // BEEVM_HOME; jails go to Home/jail (same filesystem as images and snapshots)
	JailerBin      string // default Home/bin/jailer
	UIDBase        int    // VM n runs as UID/GID UIDBase+n
	Slots          int    // most isolated VMs at once (snapshot builds included)
	CPUPercent     int    // cpu.max per vCPU in percent of one host CPU
	MemOverheadMiB int    // memory.max = guest memory + this
	PidsMax        int
	CgroupRoot     string // default /sys/fs/cgroup
	LockPath       string // default /run/beevm/isolation.lock
	Logger         *slog.Logger

	// Network gives every VM its own network namespace and a network card; nftables lets only
	// a tenant's own VMs reach each other. Egress also lets VMs reach the internet (NAT); it
	// turns on the host's IP forwarding while the server runs. NetPrealloc networks are made
	// at start-up so the first calls do not wait for them.
	Network     bool
	Egress      bool
	NetPrealloc int
}

func (c *Config) applyDefaults() {
	if c.JailerBin == "" {
		c.JailerBin = filepath.Join(c.Home, "bin", "jailer")
	}
	if c.UIDBase == 0 {
		c.UIDBase = DefaultUIDBase
	}
	if c.Slots == 0 {
		c.Slots = DefaultSlots
	}
	if c.CPUPercent == 0 {
		c.CPUPercent = DefaultCPUPercent
	}
	if c.MemOverheadMiB == 0 {
		c.MemOverheadMiB = DefaultMemOverheadMiB
	}
	if c.PidsMax == 0 {
		c.PidsMax = DefaultPidsMax
	}
	if c.CgroupRoot == "" {
		c.CgroupRoot = "/sys/fs/cgroup"
	}
	if c.LockPath == "" {
		c.LockPath = "/run/beevm/isolation.lock"
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
}

// Manager isolates VMs. One per server; New takes a lock so only one runs on a host.
type Manager struct {
	cfg     Config
	owner   vmm.Owner // owner of Home: gets the files root creates there
	slots   *slots
	cg      cgroups
	swapOff bool // memory.swap.max exists (swap accounting is on)
	lock    *os.File

	fw         *netns.Firewall // nil without Network
	oldForward string          // the host's ip_forward before Egress changed it
}

// GuestNetBootArg tells the guest's init to configure eth0 (see guest/rootfs/init).
const GuestNetBootArg = "beevm.net=1"

// New checks that isolation can work here, takes the host-wide lock, and removes what a
// crashed server left behind (cgroups, jails).
func New(cfg Config) (*Manager, error) {
	cfg.applyDefaults()
	if cfg.Home == "" {
		return nil, errors.New("isolation: Home is required")
	}
	if os.Geteuid() != 0 {
		return nil, ErrNotRoot
	}
	if cfg.CPUPercent < 1 || cfg.CPUPercent > 100 {
		return nil, fmt.Errorf("isolation: CPU limit must be 1..100 %% per vCPU, got %d", cfg.CPUPercent)
	}
	if _, err := os.Stat(cfg.JailerBin); err != nil {
		return nil, fmt.Errorf("isolation: jailer not found (run: make setup): %w", err)
	}
	owner, err := ownerOf(cfg.Home)
	if err != nil {
		return nil, fmt.Errorf("isolation: %w", err)
	}
	m := &Manager{cfg: cfg, owner: owner, slots: newSlots(cfg.Slots), cg: cgroups{root: cfg.CgroupRoot, top: "beevm"}}

	if err := m.takeLock(); err != nil {
		return nil, err
	}
	if err := m.removeStale(); err != nil {
		m.unlock()
		return nil, err
	}
	if err := m.cg.ensure(m.cg.top); err != nil {
		m.unlock()
		return nil, fmt.Errorf("isolation: cgroups v2 with cpu, memory and pids is needed: %w", err)
	}
	if _, err := os.Stat(filepath.Join(m.cg.path(m.cg.top), "memory.swap.max")); err == nil {
		m.swapOff = true
	}
	if err := os.MkdirAll(m.jailBase(), 0o755); err != nil {
		m.unlock()
		return nil, err
	}
	if cfg.Network {
		if err := m.startNetwork(); err != nil {
			m.stopNetwork()
			m.unlock()
			return nil, err
		}
	}
	return m, nil
}

// startNetwork removes networks left by a crashed server, installs the host firewall and
// prepares the first slots' networks.
func (m *Manager) startNetwork() error {
	if m.cfg.Slots > netns.MaxSlots {
		m.cfg.Slots = netns.MaxSlots
		m.slots = newSlots(m.cfg.Slots)
	}
	if n, err := netns.RemoveStale(); err != nil {
		return fmt.Errorf("isolation: remove old networks: %w", err)
	} else if n > 0 {
		m.cfg.Logger.Warn("removed VM networks left by a previous run", "count", n)
	}
	fw, err := netns.NewFirewall(m.cfg.Egress)
	if err != nil {
		return fmt.Errorf("isolation: nftables: %w", err)
	}
	m.fw = fw
	if m.cfg.Egress {
		old, err := netns.HostForwarding(true)
		if err != nil {
			return err
		}
		m.oldForward = old
		m.cfg.Logger.Warn("egress on: the host's IP forwarding is on while the server runs", "was", old)
	}
	for i := 0; i < m.cfg.NetPrealloc && i < m.cfg.Slots; i++ {
		if err := netns.Create(netns.Slot(i), m.uid(i), m.uid(i)); err != nil {
			return fmt.Errorf("isolation: create network %d: %w", i, err)
		}
		m.slots.markReady(i)
	}
	return nil
}

// stopNetwork deletes every slot network and the host firewall, and restores forwarding.
func (m *Manager) stopNetwork() {
	for _, n := range m.slots.readyList() {
		if err := netns.Delete(netns.Slot(n)); err != nil {
			m.cfg.Logger.Warn("could not delete a VM network", "slot", n, "error", err)
		}
	}
	if m.fw != nil {
		_ = m.fw.Close()
		m.fw = nil
	}
	if m.oldForward != "" {
		_, _ = netns.HostForwarding(m.oldForward == "1")
		m.oldForward = ""
	}
}

// takeLock makes sure only one isolated server runs on this host: they would fight over the
// same cgroups, UIDs and network names.
func (m *Manager) takeLock() error {
	if err := os.MkdirAll(filepath.Dir(m.cfg.LockPath), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(m.cfg.LockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return errors.New("isolation: another isolated beevmd is already running on this host")
	}
	m.lock = f
	return nil
}

func (m *Manager) unlock() {
	if m.lock != nil {
		_ = m.lock.Close() // closing releases the flock
		m.lock = nil
	}
}

// removeStale deletes cgroups and jails of VMs from an earlier run. The lock guarantees no
// other isolated server owns them.
func (m *Manager) removeStale() error {
	n, err := m.cg.removeStaleLeaves()
	if err != nil {
		return fmt.Errorf("isolation: remove old cgroups: %w", err)
	}
	if n > 0 {
		m.cfg.Logger.Warn("removed cgroups of VMs left by a previous run", "count", n)
	}
	if err := os.RemoveAll(filepath.Join(m.jailBase(), "firecracker")); err != nil {
		return fmt.Errorf("isolation: remove old jails: %w", err)
	}
	return nil
}

// jailBase is the jailer's --chroot-base-dir.
func (m *Manager) jailBase() string { return filepath.Join(m.cfg.Home, "jail") }

// Name implements vmm.Isolator. It changes when what a snapshot depends on changes (paths in
// the jail, a network card).
func (m *Manager) Name() string {
	if m.cfg.Network {
		return "jailer-net-1"
	}
	return "jailer-1"
}

// Firewall is the host's nftables table (nil without networking).
func (m *Manager) Firewall() *netns.Firewall { return m.fw }

// Owner is who owns BeeVM's folder.
func (m *Manager) Owner() vmm.Owner { return m.owner }

// Limits returns the cgroup limits a VM of this size gets.
func (m *Manager) Limits(memMiB, vcpus int64) Limits {
	return limitsFor(memMiB, vcpus, m.cfg.CPUPercent, m.cfg.MemOverheadMiB, m.cfg.PidsMax, m.swapOff)
}

// UID is the UID of slot n.
func (m *Manager) uid(slot int) int { return m.cfg.UIDBase + slot }

// Prepare implements vmm.Isolator. It gives the VM a slot (its UID), a cgroup parent for its
// tenant, and the jailer settings. release frees all of it again.
func (m *Manager) Prepare(cfg *vmm.Config) (func(), error) {
	slot, err := m.slots.get()
	if err != nil {
		return nil, err
	}
	parent := m.cg.tenantParent(cfg.TenantID)
	if err := m.cg.ensure(parent); err != nil {
		m.slots.put(slot)
		return nil, err
	}
	owner := m.owner
	cfg.Jail = &vmm.Jail{
		JailerBin:    m.cfg.JailerBin,
		ChrootBase:   m.jailBase(),
		UID:          m.uid(slot),
		GID:          m.uid(slot),
		ParentCgroup: parent,
		Cgroups:      m.Limits(cfg.MemMiB, cfg.VCPUs).args(),
		FileOwner:    &owner,
	}
	leaf := filepath.Join(parent, cfg.ID)
	tenant := cfg.TenantID
	if m.fw != nil {
		if err := m.prepareNetwork(cfg, slot); err != nil {
			m.slots.put(slot)
			return nil, err
		}
	}
	release := func() {
		if m.fw != nil {
			if err := m.fw.Detach(netns.Slot(slot), tenant); err != nil {
				m.cfg.Logger.Warn("could not detach a VM's network from its tenant", "slot", slot, "error", err)
			}
		}
		if err := m.cg.removeLeaf(leaf); err != nil {
			m.cfg.Logger.Warn("could not remove a VM's cgroup", "cgroup", leaf, "error", err)
		}
		m.slots.put(slot)
	}
	return release, nil
}

// prepareNetwork gives the VM slot's network (creating it the first time) and lets it reach
// its tenant's other VMs.
func (m *Manager) prepareNetwork(cfg *vmm.Config, slot int) error {
	s := netns.Slot(slot)
	if !m.slots.isReady(slot) {
		if err := netns.Create(s, m.uid(slot), m.uid(slot)); err != nil {
			return err
		}
		m.slots.markReady(slot)
	}
	if err := m.fw.Attach(s, cfg.TenantID); err != nil {
		return fmt.Errorf("nft attach: %w", err)
	}
	cfg.Jail.NetNS = s.Path()
	cfg.Net = &vmm.NetIface{HostDev: netns.TapName, GuestMAC: netns.GuestMAC, Addr: s.CloneIP().String()}
	cfg.ExtraBootArgs = strings.TrimSpace(cfg.ExtraBootArgs + " " + GuestNetBootArg)
	return nil
}

// CgroupDir is the folder of a VM's cgroup (for tests and reports).
func (m *Manager) CgroupDir(cfg vmm.Config) string {
	return filepath.Join(m.cg.path(m.cg.tenantParent(cfg.TenantID)), cfg.ID)
}

// InUse is how many slots are taken.
func (m *Manager) InUse() int { return m.slots.inUse() }

// ChownBack gives the files root created in BeeVM's folder back to its owner (jails excluded).
func (m *Manager) ChownBack() (int, error) {
	return ChownTree(m.cfg.Home, m.owner, m.jailBase())
}

// Close gives files back to their owner and releases the host-wide lock. Call it after every
// VM is gone.
func (m *Manager) Close() error {
	_, err := m.ChownBack()
	if left := m.InUse(); left > 0 {
		m.cfg.Logger.Warn("isolation closed while VMs still hold slots", "count", left)
	}
	m.stopNetwork()
	m.unlock()
	return err
}
