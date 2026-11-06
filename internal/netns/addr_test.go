package netns

import (
	"bytes"
	"testing"
)

func TestSlotAddresses(t *testing.T) {
	for _, c := range []struct {
		slot             Slot
		host, clone, nsn string
	}{
		{0, "10.200.0.1", "10.200.0.2", "beevm-0"},
		{1, "10.200.0.5", "10.200.0.6", "beevm-1"},
		{63, "10.200.0.253", "10.200.0.254", "beevm-63"},
		{64, "10.200.1.1", "10.200.1.2", "beevm-64"},
		{MaxSlots - 1, "10.200.255.253", "10.200.255.254", "beevm-16383"},
	} {
		if got := c.slot.HostIP().String(); got != c.host {
			t.Errorf("slot %d host %s, want %s", c.slot, got, c.host)
		}
		if got := c.slot.CloneIP().String(); got != c.clone {
			t.Errorf("slot %d clone %s, want %s", c.slot, got, c.clone)
		}
		if c.slot.Name() != c.nsn || c.slot.Path() != "/run/netns/"+c.nsn {
			t.Errorf("slot %d name %s path %s", c.slot, c.slot.Name(), c.slot.Path())
		}
		if !ClonePrefix.Contains(c.slot.CloneIP()) {
			t.Errorf("slot %d outside %s", c.slot, ClonePrefix)
		}
		if len(c.slot.HostVeth()) > 15 {
			t.Errorf("interface name %s too long", c.slot.HostVeth())
		}
	}
	if Slot(MaxSlots).Valid() || Slot(-1).Valid() || !Slot(0).Valid() {
		t.Error("Valid range wrong")
	}
}

func TestIfname(t *testing.T) {
	b := ifname("veth0")
	if len(b) != 16 || !bytes.Equal(b[:6], []byte("veth0\x00")) {
		t.Errorf("ifname %q", b)
	}
}
