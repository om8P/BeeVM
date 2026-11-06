package pool

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// changeLog collects OnStateChange reports.
type changeLog struct {
	mu  sync.Mutex
	all []StateChange
}

func (c *changeLog) add(s StateChange) {
	c.mu.Lock()
	c.all = append(c.all, s)
	c.mu.Unlock()
}

// trail returns "from>to" steps for one VM, following its ID change.
func (c *changeLog) trail(id string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, s := range c.all {
		if s.VM.ID == id {
			out = append(out, fmt.Sprintf("%s>%s", s.From, s.VM.State))
		}
	}
	return out
}

func (c *changeLog) snapshot() []StateChange {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]StateChange(nil), c.all...)
}

func newHookPool(maxVMs int, log *changeLog) (*Pool, *clock) {
	c := &clock{now: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}
	return newPool(Config{MaxVMs: maxVMs, KeepAlive: time.Minute, QueueWait: 50 * time.Millisecond,
		Now: c.Now, OnStateChange: log.add}), c
}

func TestOnStateChangeFollowsAVMsLife(t *testing.T) {
	log := &changeLog{}
	p, c := newHookPool(2, log)
	f := &factory{}
	ctx := context.Background()

	l, err := p.Acquire(ctx, keyA, f.create) // new: restoring, then busy
	if err != nil {
		t.Fatal(err)
	}
	p.Release(l, true) // idle
	l, _ = p.Acquire(ctx, keyA, f.create)
	p.Release(l, true) // warm call: busy, idle
	c.Add(2 * time.Minute)
	p.reap(c.Now()) // recycling, destroyed
	waitFor(t, "reaped", func() bool { return len(p.Stats(0)) == 0 })

	all := log.snapshot()
	if len(all) < 1 || !strings.HasPrefix(all[0].VM.ID, "pending-") || all[0].From != Restoring || all[0].VM.State != Restoring {
		t.Fatalf("first report = %+v, want a new pending VM in Restoring", all)
	}
	if all[1].VM.ID != "vm-1" || all[1].PrevID != all[0].VM.ID {
		t.Errorf("second report id %q prev %q, want vm-1 replacing %q", all[1].VM.ID, all[1].PrevID, all[0].VM.ID)
	}
	got := strings.Join(log.trail("vm-1"), " ")
	want := "restoring>busy busy>idle idle>busy busy>idle idle>recycling recycling>destroyed"
	if got != want {
		t.Errorf("trail = %s\nwant    %s", got, want)
	}
	for _, s := range all[2:] {
		if s.PrevID != "" {
			t.Errorf("PrevID %q reported again in %+v", s.PrevID, s)
		}
	}
	// The report carries the values after the change: the second busy shows 2 calls.
	var busyCalls []int64
	for _, s := range all {
		if s.VM.State == Busy {
			busyCalls = append(busyCalls, s.VM.Calls)
		}
	}
	if fmt.Sprint(busyCalls) != "[1 2]" {
		t.Errorf("calls when busy = %v, want [1 2]", busyCalls)
	}
	for _, s := range all {
		if s.VM.TenantID != keyA.TenantID || s.VM.FunctionID != keyA.FunctionID {
			t.Errorf("report for wrong VM: %+v", s.VM)
		}
	}
}

func TestOnStateChangeForPrewarmAndFailure(t *testing.T) {
	log := &changeLog{}
	p, _ := newHookPool(2, log)
	ctx := context.Background()

	if n, err := p.Prewarm(ctx, keyB, 1, (&factory{}).create); n != 1 || err != nil {
		t.Fatalf("prewarm = %d, %v", n, err)
	}
	if got := strings.Join(log.trail("vm-1"), " "); got != "restoring>idle" {
		t.Errorf("prewarm trail = %q, want restoring>idle", got)
	}

	before := len(log.snapshot())
	_, err := p.Acquire(ctx, keyA, (&factory{err: fmt.Errorf("boom")}).create)
	if err == nil {
		t.Fatal("want an error")
	}
	tail := log.snapshot()[before:]
	if len(tail) != 2 || tail[0].VM.State != Restoring || tail[1].From != Restoring || tail[1].VM.State != Destroyed ||
		tail[0].VM.ID != tail[1].VM.ID {
		t.Errorf("failed create reports = %+v, want restoring then destroyed for one pending id", tail)
	}
}

// Every VM that appeared must be reported Destroyed exactly once, even under heavy use (run with
// -race: the hook runs under the pool lock from many goroutines).
func TestOnStateChangeUnderLoad(t *testing.T) {
	log := &changeLog{}
	p, _ := newHookPool(3, log)
	f := &factory{}
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := keyA
			if i%2 == 1 {
				key = keyB
			}
			for j := 0; j < 10; j++ {
				l, err := p.Acquire(ctx, key, f.create)
				if err != nil {
					continue // busy is fine here
				}
				p.Release(l, j%4 != 0)
			}
		}()
	}
	wg.Wait()
	if err := p.Close(ctx); err != nil {
		t.Fatal(err)
	}
	created, destroyed := map[string]bool{}, map[string]int{}
	for _, s := range log.snapshot() {
		if s.From == Restoring && s.VM.State == Restoring {
			created[s.VM.ID] = true
		}
		if s.VM.State == Destroyed {
			destroyed[s.VM.ID]++
		}
	}
	if len(created) == 0 {
		t.Fatal("no VM was created")
	}
	for id, n := range destroyed {
		if n != 1 {
			t.Errorf("%s destroyed %d times", id, n)
		}
	}
	// Each creation ends either as a real VM's Destroyed or as its pending id's Destroyed.
	if len(destroyed) != len(created) {
		t.Errorf("created %d VMs, %d destroyed", len(created), len(destroyed))
	}
}
