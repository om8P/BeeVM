package pool

import "testing"

// Every pair of states is checked: the legal moves from the design, and nothing else.
func TestEveryStateTransition(t *testing.T) {
	legal := map[[2]State]bool{
		{Restoring, Idle}:      true,
		{Restoring, Busy}:      true,
		{Restoring, Destroyed}: true,
		{Idle, Busy}:           true,
		{Idle, Recycling}:      true,
		{Busy, Idle}:           true,
		{Busy, Recycling}:      true,
		{Recycling, Destroyed}: true,
	}
	all := []State{Restoring, Idle, Busy, Recycling, Destroyed}
	for _, from := range all {
		for _, to := range all {
			want := legal[[2]State{from, to}]
			if got := canMove(from, to); got != want {
				t.Errorf("%s -> %s: allowed=%v, want %v", from, to, got, want)
			}
			e := &vmEntry{id: "vm", state: from}
			err := e.move(to)
			if want && (err != nil || e.state != to) {
				t.Errorf("%s -> %s should work: %v", from, to, err)
			}
			if !want && (err == nil || e.state != from) {
				t.Errorf("%s -> %s should fail and leave the state alone", from, to)
			}
		}
	}
}

func TestStateNamesAndSlots(t *testing.T) {
	names := map[State]string{Restoring: "restoring", Idle: "idle", Busy: "busy", Recycling: "recycling", Destroyed: "destroyed"}
	for s, n := range names {
		if s.String() != n {
			t.Errorf("%d: %q", s, s.String())
		}
		if s.usesSlot() != (s != Destroyed) {
			t.Errorf("%s usesSlot wrong", s)
		}
	}
}
