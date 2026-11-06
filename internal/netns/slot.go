package netns

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// Network namespaces belong to threads, not processes. Go moves goroutines between threads,
// so code that enters a namespace must pin its goroutine to one thread
// (runtime.LockOSThread). We do that in a fresh goroutine and never unlock: when it ends, Go
// throws the thread away, so no other code can ever run in the wrong namespace by accident.

// inThread runs fn on a throw-away OS thread and waits for it.
func inThread(fn func() error) error {
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread() // never unlocked: the thread dies with this goroutine
		done <- fn()
	}()
	return <-done
}

// inNS runs fn inside the namespace at path.
func inNS(path string, fn func() error) error {
	return inThread(func() error {
		ns, err := netns.GetFromPath(path)
		if err != nil {
			return fmt.Errorf("open netns %s: %w", path, err)
		}
		defer ns.Close()
		if err := netns.Set(ns); err != nil {
			return fmt.Errorf("enter netns %s: %w", path, err)
		}
		return fn()
	})
}

// Create builds slot s: its namespace, the TAP device for the guest (owned by uid/gid, the
// user the VM's firecracker runs as, which has no right to create devices itself), the veth
// pair to the host, addresses, routes and the namespace's NAT rules.
func Create(s Slot, uid, gid int) (err error) {
	if !s.Valid() {
		return fmt.Errorf("netns: slot %d out of range", int(s))
	}
	// 1. The namespace. NewNamed creates it and bind-mounts it to /run/netns/<name>, so it
	//    lives on without a process in it; it also moves the calling thread into it, which
	//    inThread makes harmless.
	if err := inThread(func() error {
		ns, err := netns.NewNamed(s.Name())
		if err != nil {
			return err
		}
		return ns.Close()
	}); err != nil {
		return fmt.Errorf("create netns %s: %w", s.Name(), err)
	}
	defer func() {
		if err != nil {
			_ = Delete(s)
		}
	}()
	nsh, err := netns.GetFromPath(s.Path())
	if err != nil {
		return err
	}
	defer nsh.Close()

	// 2. The veth pair: bv<n> stays in the host's namespace, veth0 goes into the VM's.
	veth := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: s.HostVeth()}, PeerName: VethName,
		PeerNamespace: netlink.NsFd(int(nsh))}
	if err := netlink.LinkAdd(veth); err != nil {
		return fmt.Errorf("create veth %s: %w", s.HostVeth(), err)
	}
	host, err := netlink.LinkByName(s.HostVeth())
	if err != nil {
		return err
	}
	if err := netlink.AddrAdd(host, addrOf(s.HostIP(), GuestPrefixLen)); err != nil {
		return fmt.Errorf("address %s: %w", s.HostVeth(), err)
	}
	if err := netlink.LinkSetUp(host); err != nil {
		return err
	}
	// Let the host route packets that arrive from this VM (to another VM). Only for this
	// interface: the host's global forwarding setting stays as it is.
	if err := writeSysctl(fmt.Sprintf("net/ipv4/conf/%s/forwarding", s.HostVeth()), "1"); err != nil {
		return err
	}

	// 3. Inside the namespace.
	if err := inNS(s.Path(), func() error { return setupInside(s, uid, gid) }); err != nil {
		return err
	}
	return nsRules(s, int(nsh))
}

// setupInside runs in the slot's namespace.
func setupInside(s Slot, uid, gid int) error {
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		return err
	}
	if err := netlink.LinkSetUp(lo); err != nil {
		return err
	}

	// The TAP device. Persistent (it outlives this program's file handle), owned by the VM's
	// user so its firecracker can attach to it. NO_PI: plain Ethernet frames, as Firecracker
	// expects.
	mac, _ := net.ParseMAC(TapMAC)
	tap := &netlink.Tuntap{LinkAttrs: netlink.LinkAttrs{Name: TapName, HardwareAddr: mac},
		Mode: netlink.TUNTAP_MODE_TAP, Flags: netlink.TUNTAP_NO_PI, Owner: uint32(uid), Group: uint32(gid)}
	if err := netlink.LinkAdd(tap); err != nil {
		return fmt.Errorf("create %s: %w", TapName, err)
	}
	for _, f := range tap.Fds {
		_ = f.Close()
	}
	tl, err := netlink.LinkByName(TapName)
	if err != nil {
		return err
	}
	if err := netlink.LinkSetHardwareAddr(tl, mac); err != nil {
		return err
	}
	if err := netlink.AddrAdd(tl, addrOf(netip.MustParseAddr(TapIP), GuestPrefixLen)); err != nil {
		return err
	}
	if err := netlink.LinkSetUp(tl); err != nil {
		return err
	}

	vl, err := netlink.LinkByName(VethName)
	if err != nil {
		return err
	}
	if err := netlink.AddrAdd(vl, addrOf(s.CloneIP(), GuestPrefixLen)); err != nil {
		return err
	}
	if err := netlink.LinkSetUp(vl); err != nil {
		return err
	}
	gw := net.IP(s.HostIP().AsSlice())
	if err := netlink.RouteAdd(&netlink.Route{LinkIndex: vl.Attrs().Index, Gw: gw}); err != nil {
		return fmt.Errorf("default route: %w", err)
	}
	// Inside its own namespace the slot is a small router between tap0 and veth0.
	return writeSysctl("net/ipv4/ip_forward", "1")
}

// Delete removes slot s. Deleting the namespace destroys its devices and with them the veth
// pair (also the host's end).
func Delete(s Slot) error {
	err := netns.DeleteNamed(s.Name())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete netns %s: %w", s.Name(), err)
	}
	// Normally gone with the namespace; remove it if a half-created slot left it.
	if l, lerr := netlink.LinkByName(s.HostVeth()); lerr == nil {
		_ = netlink.LinkDel(l)
	}
	return nil
}

// RemoveStale deletes every BeeVM namespace and host veth (left by a crashed server). It
// returns how many namespaces it removed.
func RemoveStale() (int, error) {
	entries, _ := os.ReadDir("/run/netns")
	n := 0
	var errs []error
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "beevm-") {
			continue
		}
		if err := netns.DeleteNamed(e.Name()); err != nil {
			errs = append(errs, err)
		} else {
			n++
		}
	}
	links, _ := netlink.LinkList()
	for _, l := range links {
		if name := l.Attrs().Name; strings.HasPrefix(name, "bv") && l.Type() == "veth" {
			_ = netlink.LinkDel(l)
		}
	}
	return n, errors.Join(errs...)
}

// Exists reports whether slot s's namespace exists.
func Exists(s Slot) bool {
	_, err := os.Stat(s.Path())
	return err == nil
}

func addrOf(a netip.Addr, bits int) *netlink.Addr {
	return &netlink.Addr{IPNet: &net.IPNet{IP: net.IP(a.AsSlice()), Mask: net.CIDRMask(bits, 32)}}
}

// writeSysctl sets /proc/sys/<key> for the namespace of the calling thread.
func writeSysctl(key, value string) error {
	if err := os.WriteFile(filepath.Join("/proc/sys", key), []byte(value), 0); err != nil {
		return fmt.Errorf("sysctl %s: %w", strings.ReplaceAll(key, "/", "."), err)
	}
	return nil
}

func readSysctl(key string) (string, error) {
	b, err := os.ReadFile(filepath.Join("/proc/sys", key))
	return strings.TrimSpace(string(b)), err
}

// HostForwarding turns the host's IP forwarding (net.ipv4.ip_forward, all interfaces) on or
// off and returns the old value ("0" or "1"). Only egress needs it: internet replies come in
// on the host's own network card and must be routed on to the VM.
func HostForwarding(on bool) (string, error) {
	old, err := readSysctl("net/ipv4/ip_forward")
	if err != nil {
		return "", err
	}
	v := "0"
	if on {
		v = "1"
	}
	return old, writeSysctl("net/ipv4/ip_forward", v)
}
