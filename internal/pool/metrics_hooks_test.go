package pool

import (
	"context"
	"sync"
	"testing"
	"time"
)

// recorder collects OnRecycle reasons.
type recorder struct {
	mu      sync.Mutex
	reasons map[string]int
}

func (r *recorder) add(reason string) {
	r.mu.Lock()
	r.reasons[reason]++
	r.mu.Unlock()
}

func (r *recorder) get(reason string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reasons[reason]
}

func TestRecycleReasonLabels(t *testing.T) {
	for text, want := range map[string]string{
		"keep-alive expired": ReasonKeepAlive, "evicted (node full)": ReasonEvicted,
		"unhealthy after a call": ReasonUnhealthy, "function changed or removed": ReasonDrained,
		"pool closing": ReasonClosing, "vm process exited": ReasonExited, "something new": ReasonOther,
	} {
		if got := RecycleReason(text); got != want {
			t.Errorf("RecycleReason(%q) = %s, want %s", text, got, want)
		}
	}
}

func TestOnRecycleIsCalledForEveryReason(t *testing.T) {
	rec := &recorder{reasons: map[string]int{}}
	c := &clock{now: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}
	p := newPool(Config{MaxVMs: 1, KeepAlive: time.Minute, QueueWait: 50 * time.Millisecond, Now: c.Now, OnRecycle: rec.add})
	f := &factory{}
	ctx := context.Background()

	// unhealthy
	l, _ := p.Acquire(ctx, keyA, f.create)
	p.Release(l, false)
	waitFor(t, "unhealthy recycled", func() bool { return len(p.Stats(0)) == 0 })
	// keep-alive
	l, _ = p.Acquire(ctx, keyA, f.create)
	p.Release(l, true)
	c.Add(2 * time.Minute)
	p.reap(c.Now())
	waitFor(t, "reaped", func() bool { return len(p.Stats(0)) == 0 })
	// evicted: node full (1 slot), another function needs it
	l, _ = p.Acquire(ctx, keyA, f.create)
	p.Release(l, true)
	l, err := p.Acquire(ctx, keyB, f.create)
	if err != nil {
		t.Fatal(err)
	}
	p.Release(l, true)
	// drained
	p.Drain(keyB.FunctionID)
	waitFor(t, "drained", func() bool { return len(p.Stats(0)) == 0 })
	// closing
	l, _ = p.Acquire(ctx, keyA, f.create)
	p.Release(l, true)
	if err := p.Close(ctx); err != nil {
		t.Fatal(err)
	}

	for _, r := range []string{ReasonUnhealthy, ReasonKeepAlive, ReasonEvicted, ReasonDrained, ReasonClosing} {
		if rec.get(r) != 1 {
			t.Errorf("reason %s seen %d times, want 1 (all: %v)", r, rec.get(r), rec.reasons)
		}
	}
}

// pidVM is a fakeVM that knows its process id.
type pidVM struct {
	*fakeVM
	pid int
}

func (p pidVM) PID() int { return p.pid }

func TestStatsReportPID(t *testing.T) {
	p, _ := newTestPool(2)
	l, err := p.Acquire(context.Background(), keyA, func(context.Context) (Instance, error) {
		return pidVM{fakeVM: &fakeVM{id: "vm-pid", exited: make(chan struct{})}, pid: 4242}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release(l, false)
	if s := p.Stats(0); len(s) != 1 || s[0].PID != 4242 {
		t.Errorf("stats = %+v, want PID 4242", s)
	}
	l2, _ := p.Acquire(context.Background(), keyB, (&factory{}).create)
	defer p.Release(l2, false)
	for _, v := range p.Stats(keyB.TenantID) {
		if v.PID != 0 {
			t.Errorf("VM without PIDer has PID %d", v.PID)
		}
	}
}
