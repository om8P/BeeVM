package pool

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeVM stands in for a Firecracker VM.
type fakeVM struct {
	id        string
	exited    chan struct{}
	destroyed atomic.Bool
	exitOnce  sync.Once
}

func (f *fakeVM) ID() string              { return f.id }
func (f *fakeVM) Exited() <-chan struct{} { return f.exited }
func (f *fakeVM) Destroy() {
	f.destroyed.Store(true)
	f.crash()
}
func (f *fakeVM) crash() { f.exitOnce.Do(func() { close(f.exited) }) }

// factory counts created VMs and can be told to fail or to block.
type factory struct {
	n     atomic.Int32
	err   error
	block chan struct{} // if set, create waits for it
	mu    sync.Mutex
	vms   []*fakeVM
}

func (f *factory) create(ctx context.Context) (Instance, error) {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	vm := &fakeVM{id: fmt.Sprintf("vm-%d", f.n.Add(1)), exited: make(chan struct{})}
	f.mu.Lock()
	f.vms = append(f.vms, vm)
	f.mu.Unlock()
	return vm, nil
}

// clock is a fake clock that tests move by hand.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newTestPool(maxVMs int) (*Pool, *clock) {
	c := &clock{now: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)}
	return newPool(Config{MaxVMs: maxVMs, KeepAlive: time.Minute, QueueWait: 50 * time.Millisecond, Now: c.Now}), c
}

var (
	keyA = Key{TenantID: 1, FunctionID: 10, CodeSHA256: "aaa", MemoryMiB: 128, VCPUs: 1}
	keyB = Key{TenantID: 2, FunctionID: 20, CodeSHA256: "bbb", MemoryMiB: 128, VCPUs: 1}
)

// waitFor polls until cond is true (background destroys finish asynchronously).
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func states(p *Pool) map[State]int {
	out := map[State]int{}
	for _, v := range p.Stats(0) {
		out[v.State]++
	}
	return out
}

func TestColdThenWarm(t *testing.T) {
	p, _ := newTestPool(4)
	f := &factory{}
	l1, err := p.Acquire(context.Background(), keyA, f.create)
	if err != nil || l1.Warm {
		t.Fatalf("first call should create a VM: %+v, %v", l1, err)
	}
	if s := states(p); s[Busy] != 1 {
		t.Fatalf("states %v", s)
	}
	p.Release(l1, true)
	if s := states(p); s[Idle] != 1 {
		t.Fatalf("after release: %v", s)
	}
	l2, err := p.Acquire(context.Background(), keyA, f.create)
	if err != nil || !l2.Warm || l2.ID() != l1.ID() {
		t.Fatalf("second call should reuse %s: %+v, %v", l1.ID(), l2, err)
	}
	if f.n.Load() != 1 {
		t.Errorf("created %d VMs, want 1", f.n.Load())
	}
	p.Release(l2, true)
	if got := p.Stats(0)[0].Calls; got != 2 {
		t.Errorf("calls %d, want 2", got)
	}
}

func TestOtherFunctionDoesNotGetTheVM(t *testing.T) {
	p, _ := newTestPool(4)
	f := &factory{}
	l, _ := p.Acquire(context.Background(), keyA, f.create)
	p.Release(l, true)
	// Same code, different tenant and function: must not share a VM.
	other := keyA
	other.TenantID, other.FunctionID = 2, 11
	l2, err := p.Acquire(context.Background(), other, f.create)
	if err != nil || l2.Warm {
		t.Fatalf("another function got a warm VM: %+v %v", l2, err)
	}
	// A redeploy (new code) of the same function also gets a new VM.
	changed := keyA
	changed.CodeSHA256 = "new"
	l3, _ := p.Acquire(context.Background(), changed, f.create)
	if l3.Warm {
		t.Fatal("new code must not reuse a VM with the old code")
	}
}

func TestParallelCallsGetSeparateVMs(t *testing.T) {
	p, _ := newTestPool(4)
	f := &factory{}
	l1, _ := p.Acquire(context.Background(), keyA, f.create)
	l2, _ := p.Acquire(context.Background(), keyA, f.create)
	if l1.ID() == l2.ID() {
		t.Fatal("two concurrent calls share one VM")
	}
	p.Release(l1, true)
	p.Release(l2, true)
	if s := states(p); s[Idle] != 2 {
		t.Errorf("states %v", s)
	}
}

func TestUnhealthyReleaseDestroys(t *testing.T) {
	p, _ := newTestPool(4)
	f := &factory{}
	l, _ := p.Acquire(context.Background(), keyA, f.create)
	p.Release(l, false)
	waitFor(t, "vm destroyed", func() bool { return len(p.Stats(0)) == 0 })
	if !f.vms[0].destroyed.Load() {
		t.Error("Destroy not called")
	}
	p.Release(l, true) // second release is a no-op, no panic
	if len(p.Stats(0)) != 0 {
		t.Error("double release brought a VM back")
	}
}

func TestReapAfterKeepAlive(t *testing.T) {
	p, c := newTestPool(4)
	f := &factory{}
	l, _ := p.Acquire(context.Background(), keyA, f.create)
	p.Release(l, true)
	c.Add(59 * time.Second)
	if n := p.reap(c.Now()); n != 0 {
		t.Fatalf("reaped %d before the keep-alive", n)
	}
	c.Add(time.Second)
	if n := p.reap(c.Now()); n != 1 {
		t.Fatalf("reaped %d, want 1", n)
	}
	waitFor(t, "vm destroyed", func() bool { return len(p.Stats(0)) == 0 })
	if !f.vms[0].destroyed.Load() {
		t.Error("Destroy not called")
	}
}

func TestReapNeverTouchesBusyVMs(t *testing.T) {
	p, c := newTestPool(4)
	l, _ := p.Acquire(context.Background(), keyA, (&factory{}).create)
	c.Add(time.Hour)
	if n := p.reap(c.Now()); n != 0 {
		t.Fatal("reaped a busy VM")
	}
	p.Release(l, true) // its keep-alive starts now
	if n := p.reap(c.Now()); n != 0 {
		t.Fatal("reaped a VM that was just used")
	}
}

func TestReaperGoroutine(t *testing.T) {
	p := New(Config{MaxVMs: 2, KeepAlive: 30 * time.Millisecond})
	defer p.Close(context.Background())
	l, _ := p.Acquire(context.Background(), keyA, (&factory{}).create)
	p.Release(l, true)
	waitFor(t, "reaper removes the idle VM", func() bool { return len(p.Stats(0)) == 0 })
}

func TestLRUEvictionWhenFull(t *testing.T) {
	p, c := newTestPool(2)
	f := &factory{}
	la, _ := p.Acquire(context.Background(), keyA, f.create)
	p.Release(la, true) // vm-1, used at t0
	c.Add(time.Second)
	lb, _ := p.Acquire(context.Background(), keyB, f.create)
	p.Release(lb, true) // vm-2, used at t0+1s
	// Node is full (2 idle). A third function evicts the least recently used: vm-1.
	keyC := Key{TenantID: 3, FunctionID: 30, CodeSHA256: "ccc", MemoryMiB: 128, VCPUs: 1}
	lc, err := p.Acquire(context.Background(), keyC, f.create)
	if err != nil {
		t.Fatalf("should evict and create: %v", err)
	}
	if !f.vms[0].destroyed.Load() || f.vms[1].destroyed.Load() {
		t.Error("should evict vm-1 (oldest), keep vm-2")
	}
	if lc.Warm {
		t.Error("new function cannot be warm")
	}
}

func TestBusyWhenFullAndNothingIdle(t *testing.T) {
	p, _ := newTestPool(1)
	f := &factory{}
	l, _ := p.Acquire(context.Background(), keyA, f.create)
	start := time.Now()
	_, err := p.Acquire(context.Background(), keyB, f.create)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
	if took := time.Since(start); took < 40*time.Millisecond {
		t.Errorf("should wait about QueueWait, waited %v", took)
	}
	p.Release(l, true)
}

func TestWaiterGetsTheReleasedVM(t *testing.T) {
	p, _ := newTestPool(1)
	p.cfg.QueueWait = 2 * time.Second
	f := &factory{}
	l, _ := p.Acquire(context.Background(), keyA, f.create)
	done := make(chan *Lease)
	go func() {
		l2, err := p.Acquire(context.Background(), keyA, f.create)
		if err != nil {
			t.Error(err)
		}
		done <- l2
	}()
	time.Sleep(20 * time.Millisecond)
	p.Release(l, true)
	l2 := <-done
	if !l2.Warm || l2.ID() != l.ID() {
		t.Errorf("waiter should get the released VM warm: %+v", l2)
	}
}

func TestWaitingCallerGoesAway(t *testing.T) {
	p, _ := newTestPool(1)
	p.cfg.QueueWait = time.Minute
	l, _ := p.Acquire(context.Background(), keyA, (&factory{}).create)
	defer p.Release(l, true)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := p.Acquire(ctx, keyB, (&factory{}).create); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context error, got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Error("kept waiting after the caller left")
	}
}

func TestFailedCreateFreesItsSlot(t *testing.T) {
	p, _ := newTestPool(1)
	bad := &factory{err: errors.New("restore failed")}
	if _, err := p.Acquire(context.Background(), keyA, bad.create); err == nil {
		t.Fatal("want the create error")
	}
	if len(p.Stats(0)) != 0 {
		t.Fatal("failed VM still listed")
	}
	if _, err := p.Acquire(context.Background(), keyA, (&factory{}).create); err != nil {
		t.Fatalf("slot not freed: %v", err)
	}
}

func TestRestoringCountsAgainstTheLimit(t *testing.T) {
	p, _ := newTestPool(1)
	slow := &factory{block: make(chan struct{})}
	go func() { _, _ = p.Acquire(context.Background(), keyA, slow.create) }()
	waitFor(t, "restoring entry", func() bool { return states(p)[Restoring] == 1 })
	if _, err := p.Acquire(context.Background(), keyB, (&factory{}).create); !errors.Is(err, ErrBusy) {
		t.Fatalf("a VM being restored must take a slot: %v", err)
	}
	close(slow.block)
	waitFor(t, "busy", func() bool { return states(p)[Busy] == 1 })
}

func TestIdleVMThatExitsIsRemoved(t *testing.T) {
	p, _ := newTestPool(2)
	f := &factory{}
	l, _ := p.Acquire(context.Background(), keyA, f.create)
	p.Release(l, true)
	f.vms[0].crash()
	waitFor(t, "crashed VM removed", func() bool { return len(p.Stats(0)) == 0 })
	l2, _ := p.Acquire(context.Background(), keyA, f.create)
	if l2.Warm {
		t.Error("got a crashed VM")
	}
}

func TestBusyVMThatExitsIsNotKept(t *testing.T) {
	p, _ := newTestPool(2)
	f := &factory{}
	l, _ := p.Acquire(context.Background(), keyA, f.create)
	f.vms[0].crash()
	time.Sleep(10 * time.Millisecond) // let watchExit see it
	p.Release(l, true)                // the runner may not have noticed; the pool did
	waitFor(t, "removed", func() bool { return len(p.Stats(0)) == 0 })
}

func TestDrain(t *testing.T) {
	p, _ := newTestPool(4)
	f := &factory{}
	idle, _ := p.Acquire(context.Background(), keyA, f.create)
	busy, _ := p.Acquire(context.Background(), keyA, f.create)
	other, _ := p.Acquire(context.Background(), keyB, f.create)
	p.Release(idle, true)
	p.Release(other, true)

	p.Drain(keyA.FunctionID)
	waitFor(t, "idle VM of the function destroyed", func() bool { return states(p)[Idle] == 1 })
	p.Release(busy, true) // healthy, but drained: thrown away
	waitFor(t, "busy VM destroyed after its call", func() bool { return len(p.Stats(0)) == 1 })
	if got := p.Stats(0)[0].FunctionID; got != keyB.FunctionID {
		t.Errorf("drained the wrong function: %d left", got)
	}
}

func TestDrainWhileCreating(t *testing.T) {
	p, _ := newTestPool(2)
	slow := &factory{block: make(chan struct{})}
	errc := make(chan error)
	go func() {
		_, err := p.Acquire(context.Background(), keyA, slow.create)
		errc <- err
	}()
	waitFor(t, "restoring", func() bool { return states(p)[Restoring] == 1 })
	p.Drain(keyA.FunctionID)
	close(slow.block)
	if err := <-errc; err == nil {
		t.Error("a VM for a drained function must not be handed out")
	}
	waitFor(t, "nothing left", func() bool { return len(p.Stats(0)) == 0 })
	if !slow.vms[0].destroyed.Load() {
		t.Error("the new VM was not destroyed")
	}
}

func TestPrewarm(t *testing.T) {
	p, _ := newTestPool(3)
	f := &factory{}
	busy, _ := p.Acquire(context.Background(), keyB, f.create) // one slot taken
	n, err := p.Prewarm(context.Background(), keyA, 5, f.create)
	if err != nil || n != 2 {
		t.Fatalf("prewarm should fill the 2 free slots: %d, %v", n, err)
	}
	if s := states(p); s[Idle] != 2 || s[Busy] != 1 {
		t.Errorf("states %v", s)
	}
	l, _ := p.Acquire(context.Background(), keyA, f.create)
	if !l.Warm {
		t.Error("call after prewarm should be warm")
	}
	p.Release(l, true)
	if _, err := p.Prewarm(context.Background(), keyA, 1, f.create); !errors.Is(err, ErrBusy) {
		t.Errorf("full node: want ErrBusy, got %v", err)
	}
	p.Release(busy, true)
	if n, _ := p.Prewarm(context.Background(), keyA, 0, f.create); n != 0 {
		t.Error("prewarm 0 started VMs")
	}
}

func TestPrewarmFailure(t *testing.T) {
	p, _ := newTestPool(2)
	n, err := p.Prewarm(context.Background(), keyA, 2, (&factory{err: errors.New("boom")}).create)
	if n != 0 || err == nil {
		t.Fatalf("got %d, %v", n, err)
	}
	if len(p.Stats(0)) != 0 {
		t.Error("failed prewarm left entries")
	}
}

func TestStatsPerTenant(t *testing.T) {
	p, _ := newTestPool(4)
	f := &factory{}
	a, _ := p.Acquire(context.Background(), keyA, f.create)
	b, _ := p.Acquire(context.Background(), keyB, f.create)
	defer p.Release(a, true)
	defer p.Release(b, true)
	got := p.Stats(keyA.TenantID)
	if len(got) != 1 || got[0].FunctionID != keyA.FunctionID || got[0].State != Busy {
		t.Errorf("tenant A sees %+v", got)
	}
	if len(p.Stats(0)) != 2 {
		t.Error("Stats(0) should list all")
	}
}

func TestClose(t *testing.T) {
	p, _ := newTestPool(4)
	f := &factory{}
	idle, _ := p.Acquire(context.Background(), keyA, f.create)
	busy, _ := p.Acquire(context.Background(), keyA, f.create)
	p.Release(idle, true)

	closed := make(chan error)
	go func() { closed <- p.Close(context.Background()) }()
	waitFor(t, "idle destroyed at once", func() bool { return states(p)[Idle] == 0 })
	select {
	case <-closed:
		t.Fatal("Close returned while a call was still running")
	case <-time.After(20 * time.Millisecond):
	}
	if _, err := p.Acquire(context.Background(), keyA, f.create); !errors.Is(err, ErrClosed) {
		t.Errorf("acquire after close: %v", err)
	}
	p.Release(busy, true)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	for _, vm := range f.vms {
		if !vm.destroyed.Load() {
			t.Errorf("%s not destroyed", vm.id)
		}
	}
}

func TestCloseTimesOut(t *testing.T) {
	p, _ := newTestPool(1)
	l, _ := p.Acquire(context.Background(), keyA, (&factory{}).create)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := p.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("want deadline, got %v", err)
	}
	p.Release(l, true) // destroyed now, since the pool is closed
	waitFor(t, "destroyed", func() bool { return len(p.Stats(0)) == 0 })
}

// Many goroutines hammering the pool; run with -race. The limit must never be exceeded.
func TestConcurrentUseRespectsTheLimit(t *testing.T) {
	p, _ := newTestPool(3)
	p.cfg.QueueWait = 5 * time.Second
	f := &factory{}
	var live, peak atomic.Int32
	keys := []Key{keyA, keyB}
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := p.Acquire(context.Background(), keys[i%2], f.create)
			if err != nil {
				t.Error(err)
				return
			}
			n := live.Add(1)
			for {
				old := peak.Load()
				if n <= old || peak.CompareAndSwap(old, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			live.Add(-1)
			p.Release(l, i%7 != 0) // some calls are unhealthy
		}()
	}
	wg.Wait()
	if peak.Load() > 3 {
		t.Errorf("%d VMs busy at once, limit 3", peak.Load())
	}
	if got := len(p.Stats(0)); got > 3 {
		t.Errorf("%d VMs left, limit 3", got)
	}
	_ = p.Close(context.Background())
}
