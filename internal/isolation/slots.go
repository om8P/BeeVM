package isolation

import (
	"errors"
	"sync"
)

// ErrNoSlot means every slot (UID and network) is in use.
var ErrNoSlot = errors.New("isolation: no free slot (too many VMs)")

// slots hands out small numbers 0..n-1. A slot is a VM's identity on the host: its UID
// (UIDBase+slot) and, with networking, its network namespace and addresses.
//
// Freed slots go to the back of the queue (first in, first out), so a number is reused as late
// as possible: a new VM does not get the UID or IP address a VM had a moment ago.
//
// With networking, a slot whose network already exists is "ready". get prefers ready slots, so
// networks are reused instead of being created while a call waits.
type slots struct {
	mu    sync.Mutex
	free  []int
	used  map[int]bool
	ready map[int]bool
}

func newSlots(n int) *slots {
	s := &slots{used: map[int]bool{}, ready: map[int]bool{}}
	for i := 0; i < n; i++ {
		s.free = append(s.free, i)
	}
	return s
}

func (s *slots) get() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.free) == 0 {
		return 0, ErrNoSlot
	}
	i := 0
	for j, n := range s.free {
		if s.ready[n] {
			i = j
			break
		}
	}
	n := s.free[i]
	s.free = append(s.free[:i], s.free[i+1:]...)
	s.used[n] = true
	return n, nil
}

func (s *slots) markReady(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ready[n] = true
}

func (s *slots) isReady(n int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready[n]
}

// readyList returns every slot that has a network.
func (s *slots) readyList() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []int
	for n := range s.ready {
		out = append(out, n)
	}
	return out
}

// take reserves a specific free slot (for networks prepared at start-up). It reports false if
// the slot is not free.
func (s *slots) take(n int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, f := range s.free {
		if f == n {
			s.free = append(s.free[:i], s.free[i+1:]...)
			s.used[n] = true
			return true
		}
	}
	return false
}

func (s *slots) put(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.used[n] {
		return // freed twice: ignore
	}
	delete(s.used, n)
	s.free = append(s.free, n)
}

func (s *slots) inUse() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.used)
}
