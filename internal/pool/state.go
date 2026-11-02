package pool

import "fmt"

// State is where a VM is in its life. Every VM is in exactly one state at a time, and only
// the moves listed in allowed are possible. Keeping the rules in one table makes them easy
// to read and to test.
//
//	Restoring ──> Idle ──> Busy ──> Idle ...
//	    │          │        │
//	    │          └──> Recycling <──┘
//	    │                   │
//	    └──────────> Destroyed
type State int

const (
	Restoring State = iota // being created: restored (or booted) and given the function's code
	Idle                   // ready and waiting for a call of its function
	Busy                   // running a call
	Recycling              // being thrown away (killed, folder deleted)
	Destroyed              // gone; the pool forgets it
)

func (s State) String() string {
	switch s {
	case Restoring:
		return "restoring"
	case Idle:
		return "idle"
	case Busy:
		return "busy"
	case Recycling:
		return "recycling"
	case Destroyed:
		return "destroyed"
	}
	return fmt.Sprintf("state(%d)", int(s))
}

// allowed lists every legal move.
var allowed = map[State][]State{
	Restoring: {Idle, Busy, Destroyed}, // prewarmed / created for a call / creating failed
	Idle:      {Busy, Recycling},       // warm hit / TTL, eviction, drain, crash, close
	Busy:      {Idle, Recycling},       // healthy release / unhealthy release or drained
	Recycling: {Destroyed},             // cleanup finished
	Destroyed: nil,                     // the end
}

// canMove reports whether a VM may go from one state to another.
func canMove(from, to State) bool {
	for _, s := range allowed[from] {
		if s == to {
			return true
		}
	}
	return false
}

// move changes v's state, or returns an error for an illegal move (a bug in the pool).
func (v *vmEntry) move(to State) error {
	if !canMove(v.state, to) {
		return fmt.Errorf("pool: vm %s cannot go from %s to %s", v.id, v.state, to)
	}
	v.state = to
	return nil
}

// usesSlot reports whether a VM in this state counts against MaxVMs: everything that still
// has (or is about to have) a firecracker process.
func (s State) usesSlot() bool { return s != Destroyed }
