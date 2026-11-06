// Package netns gives every MicroVM its own network (milestone 9).
//
// The idea is Firecracker's "network for clones" pattern. A snapshot remembers the guest's
// network settings (IP address, MAC address, the gateway's MAC in its ARP cache), so every VM
// restored from it believes it has the same address. That only works if each VM lives in its
// own network namespace, a private copy of the host's network stack:
//
//	guest eth0 172.16.0.2 ──tap0 172.16.0.1──[ namespace beevm-<n> ]──veth0 10.200.x.(4n+2)
//	                                                                       │  (veth pair)
//	host (root namespace) ─────────────────────────────────────── bv<n> 10.200.x.(4n+1)
//
// Inside the namespace, NAT translates the guest's address to the slot's own "clone address"
// (10.200.x.(4n+2)) and back, so outside every VM has a different address. In the root
// namespace, nftables decides who may talk to whom: only VMs of the same tenant, never the
// host (see nft.go).
//
// A slot (number n) is one such namespace with its devices and addresses. Slots do not belong
// to a tenant; a tenant is attached to a slot only while one of its VMs uses it. That lets
// slots be created ahead of time and reused, which keeps their setup (several ms) off the
// path of a call.
package netns

import (
	"fmt"
	"net/netip"
)

// What every guest sees. The same in every slot, so snapshots fit everywhere.
const (
	GuestIP  = "172.16.0.2"
	TapIP    = "172.16.0.1" // the guest's gateway
	TapName  = "tap0"
	TapMAC   = "06:00:ac:10:00:01" // fixed: restored guests remember it in their ARP cache
	GuestMAC = "06:00:ac:10:00:02"
	VethName = "veth0" // the namespace's end of the veth pair

	// Guest-side prefix length (a /30 holds exactly gateway + guest).
	GuestPrefixLen = 30
)

// ClonePrefix holds every slot's addresses: four per slot (a /30), so 16384 slots.
var ClonePrefix = netip.MustParsePrefix("10.200.0.0/16")

// MaxSlots is how many slots fit into ClonePrefix.
const MaxSlots = 1 << (32 - 16 - 2)

// Slot is one VM network.
type Slot int

// Name is the network namespace's name (it appears in `ip netns list`).
func (s Slot) Name() string { return fmt.Sprintf("beevm-%d", int(s)) }

// Path is the namespace's file, which the jailer joins (--netns).
func (s Slot) Path() string { return "/run/netns/" + s.Name() }

// HostVeth is the host's end of the veth pair. Interface names are at most 15 characters;
// "bv16383" is 7.
func (s Slot) HostVeth() string { return fmt.Sprintf("bv%d", int(s)) }

func (s Slot) addr(offset int) netip.Addr {
	b := ClonePrefix.Addr().As4()
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	v += uint32(int(s)*4 + offset)
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// HostIP is the host end's address (the namespace's gateway).
func (s Slot) HostIP() netip.Addr { return s.addr(1) }

// CloneIP is the address other VMs see for this slot's VM.
func (s Slot) CloneIP() netip.Addr { return s.addr(2) }

// Valid reports whether the slot fits the address range.
func (s Slot) Valid() bool { return s >= 0 && int(s) < MaxSlots }
