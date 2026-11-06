package netns

// nftables rules. nftables is the Linux kernel's packet filter (the successor of iptables).
// Rules live in tables; a chain hooked into a point of the packet's path ("forward" = being
// routed through this machine, "input" = addressed to this machine) runs its rules in order.
// We talk to the kernel directly over netlink (github.com/google/nftables); the `nft` command
// is not needed, but if it is installed, `sudo nft list ruleset` shows what we made.
//
// Inside each slot's namespace (table "ip beevm-vm"):
//
//	prerouting  iif veth0 ip daddr <clone>    dnat to 172.16.0.2     (traffic to the VM)
//	postrouting oif veth0 ip saddr 172.16.0.2 snat to <clone>        (traffic from the VM)
//	forward     iif tap0 ip saddr != 172.16.0.2 drop                 (no faked sender address)
//	input       iif tap0 drop                                        (the VM may not talk to the namespace itself)
//
// In the host's namespace (table "ip beevm"):
//
//	forward  iifname "bv*" jump vmfwd           packets from a VM are judged in vmfwd
//	         [egress] oifname "bv*" ct state established,related accept   (internet replies)
//	         oifname "bv*" drop                 nothing else is routed to a VM
//	vmfwd    ip saddr @t<A> ip daddr @t<A> accept    one rule per tenant: its VMs reach each other
//	         [egress] oifname != "bv*" accept   to the internet
//	         drop                               everything else: other tenants' VMs
//	input    iifname "bv*" drop                 VMs never reach the host itself
//	[egress] postrouting ip saddr 10.200.0.0/16 oifname != "bv*" masquerade
//
// The per-tenant sets hold the clone addresses of that tenant's running VMs. Every packet is
// checked against them (no "established" shortcut between VMs), so when a slot is reused by
// another tenant, an old connection cannot leak through.

import (
	"fmt"
	"net/netip"
	"sync"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

const (
	hostTable = "beevm"
	vmTable   = "beevm-vm"
)

// --- small expression helpers -------------------------------------------------------------

// ifname pads an interface name to IFNAMSIZ for an exact match.
func ifname(n string) []byte {
	b := make([]byte, unix.IFNAMSIZ)
	copy(b, n)
	return b
}

func metaIf(key expr.MetaKey) expr.Any { return &expr.Meta{Key: key, Register: 1} }

// ifIs matches the interface exactly; ifPrefix matches names starting with p ("bv*").
func ifIs(key expr.MetaKey, name string) []expr.Any {
	return []expr.Any{metaIf(key), &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ifname(name)}}
}

func ifPrefix(key expr.MetaKey, p string, eq bool) []expr.Any {
	op := expr.CmpOpEq
	if !eq {
		op = expr.CmpOpNeq
	}
	return []expr.Any{metaIf(key), &expr.Cmp{Op: op, Register: 1, Data: []byte(p)}}
}

// ip4 loads the IPv4 source (src=true) or destination address into register 1.
func ip4(src bool) expr.Any {
	off := uint32(16)
	if src {
		off = 12
	}
	return &expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: off, Len: 4}
}

func addrIs(src bool, a netip.Addr, eq bool) []expr.Any {
	op := expr.CmpOpEq
	if !eq {
		op = expr.CmpOpNeq
	}
	b := a.As4()
	return []expr.Any{ip4(src), &expr.Cmp{Op: op, Register: 1, Data: b[:]}}
}

func inSet(src bool, set *nftables.Set) []expr.Any {
	return []expr.Any{ip4(src), &expr.Lookup{SourceRegister: 1, SetName: set.Name, SetID: set.ID}}
}

func verdict(k expr.VerdictKind) expr.Any { return &expr.Verdict{Kind: k} }

func jump(chain string) expr.Any { return &expr.Verdict{Kind: expr.VerdictJump, Chain: chain} }

func cat(parts ...[]expr.Any) []expr.Any {
	var out []expr.Any
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// --- inside a slot's namespace ------------------------------------------------------------

// nsRules installs a slot's NAT and filter rules inside its namespace (nsFd).
func nsRules(s Slot, nsFd int) error {
	c, err := nftables.New(nftables.WithNetNSFd(nsFd))
	if err != nil {
		return err
	}
	guest := netip.MustParseAddr(GuestIP)
	clone := s.CloneIP()
	t := c.AddTable(&nftables.Table{Family: nftables.TableFamilyIPv4, Name: vmTable})
	accept := nftables.ChainPolicyAccept
	pre := c.AddChain(&nftables.Chain{Name: "pre", Table: t, Type: nftables.ChainTypeNAT,
		Hooknum: nftables.ChainHookPrerouting, Priority: nftables.ChainPriorityNATDest, Policy: &accept})
	post := c.AddChain(&nftables.Chain{Name: "post", Table: t, Type: nftables.ChainTypeNAT,
		Hooknum: nftables.ChainHookPostrouting, Priority: nftables.ChainPriorityNATSource, Policy: &accept})
	fwd := c.AddChain(&nftables.Chain{Name: "fwd", Table: t, Type: nftables.ChainTypeFilter,
		Hooknum: nftables.ChainHookForward, Priority: nftables.ChainPriorityFilter, Policy: &accept})
	in := c.AddChain(&nftables.Chain{Name: "in", Table: t, Type: nftables.ChainTypeFilter,
		Hooknum: nftables.ChainHookInput, Priority: nftables.ChainPriorityFilter, Policy: &accept})

	g, cl := guest.As4(), clone.As4()
	// to the VM: clone address -> guest address
	c.AddRule(&nftables.Rule{Table: t, Chain: pre, Exprs: cat(ifIs(expr.MetaKeyIIFNAME, VethName), addrIs(false, clone, true),
		[]expr.Any{&expr.Immediate{Register: 1, Data: g[:]},
			&expr.NAT{Type: expr.NATTypeDestNAT, Family: unix.NFPROTO_IPV4, RegAddrMin: 1}})})
	// from the VM: guest address -> clone address
	c.AddRule(&nftables.Rule{Table: t, Chain: post, Exprs: cat(ifIs(expr.MetaKeyOIFNAME, VethName), addrIs(true, guest, true),
		[]expr.Any{&expr.Immediate{Register: 1, Data: cl[:]},
			&expr.NAT{Type: expr.NATTypeSourceNAT, Family: unix.NFPROTO_IPV4, RegAddrMin: 1}})})
	// no faked sender addresses
	c.AddRule(&nftables.Rule{Table: t, Chain: fwd, Exprs: cat(ifIs(expr.MetaKeyIIFNAME, TapName), addrIs(true, guest, false),
		[]expr.Any{verdict(expr.VerdictDrop)})})
	// the VM may not talk to its namespace (172.16.0.1, the clone address) itself
	c.AddRule(&nftables.Rule{Table: t, Chain: in, Exprs: cat(ifIs(expr.MetaKeyIIFNAME, TapName),
		[]expr.Any{verdict(expr.VerdictDrop)})})
	if err := c.Flush(); err != nil {
		return fmt.Errorf("nft rules in %s: %w", s.Name(), err)
	}
	return nil
}

// --- the host's namespace -----------------------------------------------------------------

// Firewall is BeeVM's table in the host's namespace.
type Firewall struct {
	egress bool

	mu      sync.Mutex
	table   *nftables.Table
	vmfwd   *nftables.Chain
	tenants map[int64]*nftables.Set
}

// NewFirewall replaces BeeVM's host table (removing one left by a crashed server) with the
// fixed rules. With egress, VMs may also reach the internet (masqueraded).
func NewFirewall(egress bool) (*Firewall, error) {
	c, err := nftables.New()
	if err != nil {
		return nil, err
	}
	deleteHostTable(c)
	f := &Firewall{egress: egress, tenants: map[int64]*nftables.Set{}}
	f.table = c.AddTable(&nftables.Table{Family: nftables.TableFamilyIPv4, Name: hostTable})
	accept := nftables.ChainPolicyAccept
	f.vmfwd = c.AddChain(&nftables.Chain{Name: "vmfwd", Table: f.table})
	forward := c.AddChain(&nftables.Chain{Name: "forward", Table: f.table, Type: nftables.ChainTypeFilter,
		Hooknum: nftables.ChainHookForward, Priority: nftables.ChainPriorityFilter, Policy: &accept})
	input := c.AddChain(&nftables.Chain{Name: "input", Table: f.table, Type: nftables.ChainTypeFilter,
		Hooknum: nftables.ChainHookInput, Priority: nftables.ChainPriorityFilter, Policy: &accept})

	c.AddRule(&nftables.Rule{Table: f.table, Chain: forward, Exprs: cat(ifPrefix(expr.MetaKeyIIFNAME, "bv", true),
		[]expr.Any{jump("vmfwd")})})
	if egress {
		c.AddRule(&nftables.Rule{Table: f.table, Chain: forward, Exprs: cat(ifPrefix(expr.MetaKeyOIFNAME, "bv", true),
			ctEstablished(), []expr.Any{verdict(expr.VerdictAccept)})})
	}
	c.AddRule(&nftables.Rule{Table: f.table, Chain: forward, Exprs: cat(ifPrefix(expr.MetaKeyOIFNAME, "bv", true),
		[]expr.Any{verdict(expr.VerdictDrop)})})

	// vmfwd ends with these; tenant rules are inserted in front of them.
	if egress {
		c.AddRule(&nftables.Rule{Table: f.table, Chain: f.vmfwd, Exprs: cat(ifPrefix(expr.MetaKeyOIFNAME, "bv", false),
			[]expr.Any{verdict(expr.VerdictAccept)})})
	}
	c.AddRule(&nftables.Rule{Table: f.table, Chain: f.vmfwd, Exprs: []expr.Any{verdict(expr.VerdictDrop)}})

	c.AddRule(&nftables.Rule{Table: f.table, Chain: input, Exprs: cat(ifPrefix(expr.MetaKeyIIFNAME, "bv", true),
		[]expr.Any{verdict(expr.VerdictDrop)})})

	if egress {
		post := c.AddChain(&nftables.Chain{Name: "post", Table: f.table, Type: nftables.ChainTypeNAT,
			Hooknum: nftables.ChainHookPostrouting, Priority: nftables.ChainPriorityNATSource, Policy: &accept})
		pfx := ClonePrefix.Addr().As4()
		mask := []byte{255, 255, 0, 0}
		c.AddRule(&nftables.Rule{Table: f.table, Chain: post, Exprs: cat(
			[]expr.Any{ip4(true),
				&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: mask, Xor: []byte{0, 0, 0, 0}},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: pfx[:]}},
			ifPrefix(expr.MetaKeyOIFNAME, "bv", false),
			[]expr.Any{&expr.Masq{}})})
	}
	if err := c.Flush(); err != nil {
		return nil, fmt.Errorf("nft host rules: %w", err)
	}
	return f, nil
}

// ctEstablished matches packets of connections that already exist (ct state established,related).
func ctEstablished() []expr.Any {
	mask := binaryutil.NativeEndian.PutUint32(expr.CtStateBitESTABLISHED | expr.CtStateBitRELATED)
	return []expr.Any{
		&expr.Ct{Register: 1, Key: expr.CtKeySTATE},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: mask, Xor: []byte{0, 0, 0, 0}},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte{0, 0, 0, 0}},
	}
}

func deleteHostTable(c *nftables.Conn) {
	tables, err := c.ListTablesOfFamily(nftables.TableFamilyIPv4)
	if err != nil {
		return
	}
	for _, t := range tables {
		if t.Name == hostTable {
			c.DelTable(t)
			_ = c.Flush()
		}
	}
}

// tenantSet returns the set of a tenant's clone addresses, creating it and its accept rule
// the first time. Callers hold f.mu.
func (f *Firewall) tenantSet(c *nftables.Conn, tenant int64) (*nftables.Set, error) {
	if s, ok := f.tenants[tenant]; ok {
		return s, nil
	}
	s := &nftables.Set{Table: f.table, Name: fmt.Sprintf("t%d", tenant), KeyType: nftables.TypeIPAddr}
	if err := c.AddSet(s, nil); err != nil {
		return nil, err
	}
	// Inserted at the top of vmfwd: before the final drop.
	c.InsertRule(&nftables.Rule{Table: f.table, Chain: f.vmfwd, Exprs: cat(inSet(true, s), inSet(false, s),
		[]expr.Any{verdict(expr.VerdictAccept)})})
	if err := c.Flush(); err != nil {
		return nil, fmt.Errorf("nft tenant %d: %w", tenant, err)
	}
	f.tenants[tenant] = s
	return s, nil
}

// Attach lets slot s's VM reach the other VMs of tenant (and be reached by them).
func (f *Firewall) Attach(s Slot, tenant int64) error {
	return f.change(s, tenant, true)
}

// Detach removes slot s from tenant: its address is nobody's again.
func (f *Firewall) Detach(s Slot, tenant int64) error {
	return f.change(s, tenant, false)
}

func (f *Firewall) change(s Slot, tenant int64, add bool) error {
	if tenant <= 0 {
		return nil // snapshot builds: no tenant, reach nobody
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := nftables.New()
	if err != nil {
		return err
	}
	set, err := f.tenantSet(c, tenant)
	if err != nil {
		return err
	}
	ip := s.CloneIP().As4()
	el := []nftables.SetElement{{Key: ip[:]}}
	if add {
		err = c.SetAddElements(set, el)
	} else {
		err = c.SetDeleteElements(set, el)
	}
	if err != nil {
		return err
	}
	return c.Flush()
}

// Members returns the clone addresses in a tenant's set (for tests).
func (f *Firewall) Members(tenant int64) ([]netip.Addr, error) {
	f.mu.Lock()
	set, ok := f.tenants[tenant]
	f.mu.Unlock()
	if !ok {
		return nil, nil
	}
	c, err := nftables.New()
	if err != nil {
		return nil, err
	}
	els, err := c.GetSetElements(set)
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, e := range els {
		if len(e.Key) == 4 {
			out = append(out, netip.AddrFrom4([4]byte(e.Key)))
		}
	}
	return out, nil
}

// Close removes BeeVM's host table.
func (f *Firewall) Close() error {
	c, err := nftables.New()
	if err != nil {
		return err
	}
	deleteHostTable(c)
	return nil
}
