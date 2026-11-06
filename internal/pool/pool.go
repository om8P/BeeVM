// Package pool is BeeVM's warm pool (the "Scheduler & Warm Pool" component). After a call, the
// VM is not thrown away: it waits, idle, for the next call of the same function, which then
// skips the snapshot restore and the code upload (the warm path).
//
// The pool knows nothing about Firecracker: it manages Instances that a CreateFunc makes. That
// keeps it testable with fake VMs and a fake clock.
//
// Concurrency in one paragraph: one mutex (mu) protects the list of VMs. Slow work (creating a
// VM, destroying one) never runs while mu is held, otherwise one slow restore would block every
// other call. Callers that must wait for a free slot sleep on the "changed" channel, which is
// closed (waking all of them) and replaced every time something changes.
package pool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"time"
)

var (
	// ErrBusy means every VM slot is in use and none became free within QueueWait.
	ErrBusy = errors.New("too many VMs running, try again shortly")
	// ErrClosed means the pool is shutting down.
	ErrClosed = errors.New("pool is closed")
)

// Key says which VMs can serve a call. VMs are never shared between functions (or tenants),
// even with identical code: files one function leaves in /tmp stay inside its own VMs. A
// redeploy changes CodeSHA256 or the size, so old VMs are not reused.
type Key struct {
	TenantID   int64
	FunctionID int64
	CodeSHA256 string
	MemoryMiB  int64
	VCPUs      int64
	Runtime    string // M8: python/node VMs hold an interpreter with one handler imported
	Handler    string
}

// Instance is one running VM with the function's code already loaded.
type Instance interface {
	ID() string
	Exited() <-chan struct{} // closed when the VM's process has ended
	Destroy()                // kill it and remove its files; must be safe to call once at any time
}

// CreateFunc makes a new ready Instance (restore or boot, then upload the code).
type CreateFunc func(ctx context.Context) (Instance, error)

// Config sets the pool's limits.
type Config struct {
	MaxVMs    int           // VMs on this node, in any state; at least 1
	KeepAlive time.Duration // how long an idle VM is kept; default 60 s
	QueueWait time.Duration // how long a call waits for a free slot before ErrBusy; default 5 s
	Now       func() time.Time
	Logger    *slog.Logger
}

// Lease is a VM handed to one call. Give it back with Release.
type Lease struct {
	Instance
	Warm bool // true when the VM already existed (warm hit)

	e        *vmEntry
	released bool
}

// VMInfo describes one VM, for listings.
type VMInfo struct {
	ID         string
	TenantID   int64
	FunctionID int64
	CodeSHA256 string
	State      State
	Since      time.Time // when it entered State
	LastUsed   time.Time // when its last call ended (creation time if never used)
	Calls      int64
	Addr       string // its network address for its tenant's other VMs ("" without networking)
}

// Addresser is an Instance with a network address (optional).
type Addresser interface {
	Addr() string
}

type vmEntry struct {
	id       string
	key      Key
	inst     Instance
	state    State
	since    time.Time
	lastUsed time.Time
	calls    int64
	drained  bool          // throw away instead of keeping when released
	gone     chan struct{} // closed when Destroyed
}

// Pool is a set of per-function warm VMs sharing one node-wide limit.
type Pool struct {
	cfg Config

	mu      sync.Mutex
	vms     map[*vmEntry]struct{}
	changed chan struct{}
	closed  bool
	pending int // counter for the ids of VMs still being created

	destroys sync.WaitGroup // background Destroy calls
	stop     chan struct{}
	stopOnce sync.Once
	reaperUp chan struct{}
}

// New creates a pool and starts its reaper (the goroutine that removes VMs idle for longer than
// KeepAlive). Call Close to stop it.
func New(cfg Config) *Pool {
	p := newPool(cfg)
	p.reaperUp = make(chan struct{})
	go p.reaper()
	return p
}

// newPool creates a pool without the reaper (unit tests call reap themselves).
func newPool(cfg Config) *Pool {
	if cfg.MaxVMs < 1 {
		cfg.MaxVMs = 1
	}
	if cfg.KeepAlive <= 0 {
		cfg.KeepAlive = 60 * time.Second
	}
	if cfg.QueueWait <= 0 {
		cfg.QueueWait = 5 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Pool{cfg: cfg, vms: map[*vmEntry]struct{}{}, changed: make(chan struct{}), stop: make(chan struct{})}
}

// Config returns the pool's settings (with defaults filled in).
func (p *Pool) Config() Config { return p.cfg }

// broadcastLocked wakes everyone waiting on p.changed. Closing a channel is a broadcast: every
// receiver sees it at once. The closed channel is then replaced for the next round.
func (p *Pool) broadcastLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
}

// usedLocked counts VMs that take a slot.
func (p *Pool) usedLocked() int {
	n := 0
	for e := range p.vms {
		if e.state.usesSlot() {
			n++
		}
	}
	return n
}

// mustMove is move for transitions the pool's own logic guarantees; an error is a bug.
func (p *Pool) mustMove(e *vmEntry, to State) {
	if err := e.move(to); err != nil {
		panic(err)
	}
	e.since = p.cfg.Now()
}

// Acquire returns a VM for key: an idle one if there is one (warm), otherwise a new one made
// with create. When the node is full it first throws away the least recently used idle VM of
// any function; if nothing is idle it waits up to QueueWait (ErrBusy) or until ctx ends.
func (p *Pool) Acquire(ctx context.Context, key Key, create CreateFunc) (*Lease, error) {
	var timeout <-chan time.Time
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, ErrClosed
		}
		// 1. Warm hit.
		if e := p.newestIdleLocked(key); e != nil {
			p.mustMove(e, Busy)
			e.calls++
			p.mu.Unlock()
			return &Lease{Instance: e.inst, Warm: true, e: e}, nil
		}
		// 2. Full? Make room by evicting the least recently used idle VM. It still uses its
		// slot until it is destroyed, so we wait for that below (the broadcast wakes us).
		if p.usedLocked() >= p.cfg.MaxVMs {
			if victim := p.oldestIdleLocked(); victim != nil {
				p.recycleLocked(victim, "evicted (node full)")
			}
		}
		// 3. A free slot: create a new VM.
		if p.usedLocked() < p.cfg.MaxVMs {
			e := p.reserveLocked(key)
			p.mu.Unlock()
			return p.finishCreate(ctx, e, create, Busy)
		}
		// 4. Wait for something to change.
		ch := p.changed
		p.mu.Unlock()
		if timeout == nil {
			t := time.NewTimer(p.cfg.QueueWait)
			defer t.Stop()
			timeout = t.C
		}
		select {
		case <-ch:
		case <-timeout:
			return nil, ErrBusy
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// reserveLocked adds a Restoring entry, which takes a slot while the VM is being created.
func (p *Pool) reserveLocked(key Key) *vmEntry {
	p.pending++
	now := p.cfg.Now()
	e := &vmEntry{id: fmt.Sprintf("pending-%d", p.pending), key: key, state: Restoring,
		since: now, lastUsed: now, gone: make(chan struct{})}
	p.vms[e] = struct{}{}
	return e
}

// finishCreate runs create (without the lock) for a reserved entry and moves it to the state
// `to` (Busy for a call, Idle for a prewarm).
func (p *Pool) finishCreate(ctx context.Context, e *vmEntry, create CreateFunc, to State) (*Lease, error) {
	inst, err := create(ctx)

	p.mu.Lock()
	if err != nil {
		p.removeLocked(e)
		p.mu.Unlock()
		return nil, err
	}
	e.inst, e.id = inst, inst.ID()
	if p.closed || e.drained {
		// Shut down (or the function was removed) while we were creating it: not needed.
		p.removeLocked(e)
		p.mu.Unlock()
		inst.Destroy()
		if p.closed {
			return nil, ErrClosed
		}
		return nil, fmt.Errorf("function changed while its VM was being created")
	}
	p.mustMove(e, to)
	e.lastUsed = p.cfg.Now()
	if to == Busy {
		e.calls++
	}
	p.broadcastLocked()
	p.mu.Unlock()

	go p.watchExit(e)
	return &Lease{Instance: inst, Warm: false, e: e}, nil
}

// removeLocked forgets a Restoring entry whose VM was never handed out.
func (p *Pool) removeLocked(e *vmEntry) {
	p.mustMove(e, Destroyed)
	delete(p.vms, e)
	close(e.gone)
	p.broadcastLocked()
}

// watchExit recycles a VM whose process ended by itself (a crash, or the guest rebooting).
func (p *Pool) watchExit(e *vmEntry) {
	select {
	case <-e.inst.Exited():
	case <-e.gone:
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch e.state {
	case Idle:
		p.recycleLocked(e, "vm process exited")
	case Busy:
		e.drained = true // its call fails anyway; do not keep it
	}
}

// Release gives a VM back after a call. A healthy VM becomes idle (warm for the next call); an
// unhealthy one (timed out, agent error, caller hung up) is thrown away. Releasing twice is a
// no-op.
func (p *Pool) Release(l *Lease, healthy bool) {
	if l == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if l.released {
		return
	}
	l.released = true
	e := l.e
	switch {
	case !healthy:
		p.recycleLocked(e, "unhealthy after a call")
	case e.drained:
		p.recycleLocked(e, "function changed or removed")
	case p.closed:
		p.recycleLocked(e, "pool closing")
	default:
		p.mustMove(e, Idle)
		e.lastUsed = p.cfg.Now()
		p.broadcastLocked()
	}
}

// recycleLocked moves a VM to Recycling and destroys it in the background (killing a VM and
// deleting its folder takes a few milliseconds; nobody should wait for that under the lock).
func (p *Pool) recycleLocked(e *vmEntry, reason string) {
	p.mustMove(e, Recycling)
	p.cfg.Logger.Info("recycling vm", "vm", e.id, "function_id", e.key.FunctionID, "reason", reason)
	p.broadcastLocked()
	p.destroys.Add(1)
	go func() {
		defer p.destroys.Done()
		e.inst.Destroy()
		p.mu.Lock()
		p.mustMove(e, Destroyed)
		delete(p.vms, e)
		close(e.gone)
		p.broadcastLocked()
		p.mu.Unlock()
	}()
}

// newestIdleLocked returns the idle VM for key that was used most recently (its memory pages
// are the most likely to still be "hot"), or nil.
func (p *Pool) newestIdleLocked(key Key) *vmEntry {
	var best *vmEntry
	for e := range p.vms {
		if e.state == Idle && e.key == key && !e.drained && (best == nil || e.lastUsed.After(best.lastUsed)) {
			best = e
		}
	}
	return best
}

// oldestIdleLocked returns the least recently used idle VM of any function (LRU), or nil.
func (p *Pool) oldestIdleLocked() *vmEntry {
	var lru *vmEntry
	for e := range p.vms {
		if e.state == Idle && (lru == nil || e.lastUsed.Before(lru.lastUsed)) {
			lru = e
		}
	}
	return lru
}

// Prewarm creates up to n idle VMs for key so that the next calls are warm. It only uses free
// slots (it never evicts other functions' VMs and never waits) and returns how many it started.
// The new VMs expire after KeepAlive like any idle VM.
func (p *Pool) Prewarm(ctx context.Context, key Key, n int, create CreateFunc) (int, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return 0, ErrClosed
	}
	var reserved []*vmEntry
	for i := 0; i < n && p.usedLocked() < p.cfg.MaxVMs; i++ {
		reserved = append(reserved, p.reserveLocked(key))
	}
	p.mu.Unlock()
	if len(reserved) == 0 && n > 0 {
		return 0, ErrBusy
	}

	// Create them in parallel: each restore is independent.
	var wg sync.WaitGroup
	errs := make([]error, len(reserved))
	for i, e := range reserved {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = p.finishCreate(ctx, e, create, Idle)
		}()
	}
	wg.Wait()
	started := 0
	for _, err := range errs {
		if err == nil {
			started++
		}
	}
	return started, errors.Join(errs...)
}

// Drain throws away every VM of a function: idle ones now, busy ones when their call ends, and
// ones being created as soon as they are ready. Used when a function is redeployed or deleted.
func (p *Pool) Drain(functionID int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for e := range p.vms {
		if e.key.FunctionID != functionID {
			continue
		}
		e.drained = true
		if e.state == Idle {
			p.recycleLocked(e, "function changed or removed")
		}
	}
}

// reap recycles VMs that have been idle for KeepAlive or longer at time now.
func (p *Pool) reap(now time.Time) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for e := range p.vms {
		if e.state == Idle && now.Sub(e.lastUsed) >= p.cfg.KeepAlive {
			p.recycleLocked(e, "keep-alive expired")
			n++
		}
	}
	return n
}

// reaper calls reap regularly until Close.
func (p *Pool) reaper() {
	defer close(p.reaperUp)
	every := min(max(p.cfg.KeepAlive/4, 10*time.Millisecond), time.Second)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			p.reap(p.cfg.Now())
		}
	}
}

// Stats lists the VMs of one tenant (tenantID <= 0: all VMs), sorted by id.
func (p *Pool) Stats(tenantID int64) []VMInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := []VMInfo{}
	for e := range p.vms {
		if tenantID > 0 && e.key.TenantID != tenantID {
			continue
		}
		info := VMInfo{ID: e.id, TenantID: e.key.TenantID, FunctionID: e.key.FunctionID,
			CodeSHA256: e.key.CodeSHA256, State: e.state, Since: e.since, LastUsed: e.lastUsed, Calls: e.calls}
		if a, ok := e.inst.(Addresser); ok && e.inst != nil {
			info.Addr = a.Addr()
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Close stops the pool: no new calls, idle VMs are destroyed now, busy ones when their call
// ends. It waits until every VM is gone or ctx ends.
func (p *Pool) Close(ctx context.Context) error {
	p.stopOnce.Do(func() { close(p.stop) })
	if p.reaperUp != nil {
		<-p.reaperUp
	}
	p.mu.Lock()
	p.closed = true
	for e := range p.vms {
		if e.state == Idle {
			p.recycleLocked(e, "pool closing")
		}
	}
	p.broadcastLocked() // wake waiting Acquires so they see closed
	for len(p.vms) > 0 {
		ch := p.changed
		p.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
		p.mu.Lock()
	}
	p.mu.Unlock()
	p.destroys.Wait()
	return nil
}
