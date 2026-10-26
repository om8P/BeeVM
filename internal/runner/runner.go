// Package runner runs one function call inside a MicroVM and returns the result.
//
// The API server only knows the Runner interface. ColdRunner boots a fresh VM for every
// call; SnapshotRunner restores one from a base snapshot instead (milestone 6), which is
// about 25 times faster. The warm pool (M7) will be another Runner.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/om8P/BeeVM/internal/snapshot"
	"github.com/om8P/BeeVM/internal/vmm"
)

// ErrBusy means all VM slots are in use and none became free in time. The API answers 503.
var ErrBusy = errors.New("too many VMs running, try again shortly")

// Request is one function call.
type Request struct {
	CodePath  string          // the function binary on the host
	Payload   json.RawMessage // JSON input for the function
	MemoryMiB int64
	VCPUs     int64
	TimeoutMS int64 // limit for the function itself
}

// How a VM was started for a call.
const (
	StartBoot    = "boot"    // a full boot of a new VM
	StartRestore = "restore" // restored from a snapshot
)

// Result is what came back. If FunctionError is set, the VM worked but the function failed
// (non-zero exit, timeout, bad output); Output is then empty.
type Result struct {
	Output        json.RawMessage
	Stderr        string // the function's log
	ExitCode      int
	FunctionError string

	VMID       string
	StartKind  string // StartBoot or StartRestore
	BootMS     int64  // starting the VM until its agent was ready
	UploadMS   int64  // sending the function's code into the VM
	FunctionMS int64  // reported by the agent
	TotalMS    int64  // the whole Run
}

// Runner runs a function once.
type Runner interface {
	Run(ctx context.Context, req Request) (Result, error)
}

// limiter caps how many VMs run at the same time.
type limiter struct {
	slots     chan struct{}
	queueWait time.Duration
}

func newLimiter(maxVMs int, queueWait time.Duration) limiter {
	if maxVMs < 1 {
		maxVMs = 1
	}
	return limiter{slots: make(chan struct{}, maxVMs), queueWait: queueWait}
}

// acquire takes a slot, or gives up after queueWait (ErrBusy) or when the caller goes away.
func (l limiter) acquire(ctx context.Context) (release func(), err error) {
	wait := time.NewTimer(l.queueWait)
	defer wait.Stop()
	select {
	case l.slots <- struct{}{}:
		return func() { <-l.slots }, nil
	case <-wait.C:
		return nil, ErrBusy
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// readyTimeout limits how long we wait for a VM to boot (or restore) and its agent to answer.
const readyTimeout = 20 * time.Second

// agentConfig turns the base config into one for a VM that runs the guest agent. All VMs use
// the shared read-only root filesystem: no per-call copy (that alone took ~0.4 s).
func agentConfig(base vmm.Config) vmm.Config {
	base.ExtraBootArgs = snapshot.BootArgs
	base.SharedRootfs = true
	return base
}

// ColdRunner boots a new VM for every call and throws it away afterwards: slow (about 1 s on
// WSL2) but simple and isolated. At most maxVMs run at once.
type ColdRunner struct {
	limiter
	base vmm.Config // paths and kernel settings; memory and vCPUs come from each request
}

// NewColdRunner creates a runner. base needs BaseDir, FirecrackerBin, KernelPath and RootfsPath.
// A call waits up to queueWait for a free slot, then fails with ErrBusy.
func NewColdRunner(base vmm.Config, maxVMs int, queueWait time.Duration) *ColdRunner {
	return &ColdRunner{limiter: newLimiter(maxVMs, queueWait), base: agentConfig(base)}
}

// Run implements Runner.
func (c *ColdRunner) Run(ctx context.Context, req Request) (Result, error) {
	start := time.Now()
	release, err := c.acquire(ctx)
	if err != nil {
		return Result{}, err
	}
	defer release()
	res, err := bootAndRun(ctx, c.base, req)
	res.TotalMS = time.Since(start).Milliseconds()
	return res, err
}

// bootAndRun boots a VM, runs the call on it and throws it away. The caller holds a slot.
func bootAndRun(ctx context.Context, base vmm.Config, req Request) (Result, error) {
	cfg := base
	cfg.MemMiB = req.MemoryMiB
	cfg.VCPUs = req.VCPUs
	vm, err := vmm.New(cfg)
	if err != nil {
		return Result{StartKind: StartBoot}, fmt.Errorf("create vm: %w", err)
	}
	// The VM is disposable: Cleanup kills it at once and deletes its folder. No graceful
	// shutdown is needed, and skipping it keeps the response fast.
	defer vm.Cleanup(false)
	res := Result{VMID: vm.ID(), StartKind: StartBoot}

	readyCtx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	if err := vm.Start(readyCtx); err != nil {
		return res, fmt.Errorf("start vm: %w", err)
	}
	boot, err := vm.WaitForAgent(readyCtx)
	if err != nil {
		return res, fmt.Errorf("wait for agent: %w", err)
	}
	res.BootMS = boot.Milliseconds()
	return res, runOn(ctx, readyCtx, vm, req, &res)
}

// runOn uploads the function to a ready VM and calls it once.
func runOn(ctx, readyCtx context.Context, vm *vmm.VM, req Request, res *Result) error {
	t0 := time.Now()
	if _, err := vm.Agent().UploadFunctionFile(readyCtx, req.CodePath); err != nil {
		return fmt.Errorf("upload function: %w", err)
	}
	res.UploadMS = time.Since(t0).Milliseconds()

	// The agent enforces the function's own limit; allow a little longer so its answer arrives.
	limit := time.Duration(req.TimeoutMS) * time.Millisecond
	callCtx, cancelCall := context.WithTimeout(ctx, limit+10*time.Second)
	defer cancelCall()
	out, err := vm.Agent().Invoke(callCtx, req.Payload, limit)
	if err != nil {
		return fmt.Errorf("invoke: %w", err)
	}
	res.Output, res.Stderr, res.ExitCode = out.Output, out.Stderr, out.ExitCode
	res.FunctionError = out.Error
	res.FunctionMS = out.DurationMS
	return nil
}

// Snapshots is what SnapshotRunner needs from the Snapshot Manager (an interface, so tests can
// use a fake).
type Snapshots interface {
	Ensure(ctx context.Context, memMiB, vcpus int64) (snapshot.Snapshot, error)
}

// SnapshotRunner restores a VM from a base snapshot for every call: the guest is already
// booted and its agent running, so the VM is ready in tens of milliseconds. The snapshot for
// a memory size and vCPU count is built the first time it is needed. If that fails, calls
// fall back to a full boot (and building is not retried for a minute).
type SnapshotRunner struct {
	limiter
	base   vmm.Config
	snaps  Snapshots
	logger *slog.Logger

	mu          sync.Mutex
	failedUntil map[[2]int64]time.Time // memory and vCPUs -> do not try to build before

	// restore and boot do the work; replaced in unit tests.
	restore func(ctx context.Context, base vmm.Config, snap snapshot.Snapshot, req Request) (Result, error)
	boot    func(ctx context.Context, base vmm.Config, req Request) (Result, error)
}

// retryBuildAfter is how long the runner boots instead of retrying a failed snapshot build.
const retryBuildAfter = time.Minute

// NewSnapshotRunner creates a runner that restores VMs from snaps. base needs BaseDir and
// FirecrackerBin (and KernelPath and RootfsPath for the boot fallback).
func NewSnapshotRunner(base vmm.Config, snaps Snapshots, maxVMs int, queueWait time.Duration, logger *slog.Logger) *SnapshotRunner {
	if logger == nil {
		logger = slog.Default()
	}
	return &SnapshotRunner{
		limiter: newLimiter(maxVMs, queueWait), base: agentConfig(base), snaps: snaps, logger: logger,
		failedUntil: map[[2]int64]time.Time{}, restore: restoreAndRun, boot: bootAndRun,
	}
}

// Run implements Runner.
func (s *SnapshotRunner) Run(ctx context.Context, req Request) (Result, error) {
	start := time.Now()
	release, err := s.acquire(ctx)
	if err != nil {
		return Result{}, err
	}
	defer release()

	var res Result
	if snap, ok := s.snapshotFor(ctx, req); ok {
		res, err = s.restore(ctx, s.base, snap, req)
	} else {
		res, err = s.boot(ctx, s.base, req)
	}
	res.TotalMS = time.Since(start).Milliseconds()
	return res, err
}

// snapshotFor returns the snapshot for this call's VM size, building it if needed. It reports
// false when there is none and the call should boot instead.
func (s *SnapshotRunner) snapshotFor(ctx context.Context, req Request) (snapshot.Snapshot, bool) {
	size := [2]int64{req.MemoryMiB, req.VCPUs}
	s.mu.Lock()
	until := s.failedUntil[size]
	s.mu.Unlock()
	if time.Now().Before(until) {
		return snapshot.Snapshot{}, false
	}
	snap, err := s.snaps.Ensure(ctx, req.MemoryMiB, req.VCPUs)
	if err != nil {
		if ctx.Err() == nil { // not just the caller hanging up
			s.logger.Error("snapshot unavailable, booting instead", "memory_mib", req.MemoryMiB, "vcpus", req.VCPUs,
				"retry_in", retryBuildAfter.String(), "error", err)
			s.mu.Lock()
			s.failedUntil[size] = time.Now().Add(retryBuildAfter)
			s.mu.Unlock()
		}
		return snapshot.Snapshot{}, false
	}
	return snap, true
}

// restoreAndRun restores a VM from snap, runs the call on it and throws it away. The caller
// holds a slot.
func restoreAndRun(ctx context.Context, base vmm.Config, snap snapshot.Snapshot, req Request) (Result, error) {
	res := Result{StartKind: StartRestore}
	readyCtx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()

	cfg := base
	cfg.MemMiB, cfg.VCPUs = req.MemoryMiB, req.VCPUs // informational; the snapshot decides
	vm, _, err := vmm.Restore(readyCtx, cfg, snap.Files)
	if err != nil {
		return res, fmt.Errorf("restore vm: %w", err)
	}
	defer vm.Cleanup(false)
	res.VMID = vm.ID()

	if _, err := vm.WaitForAgent(readyCtx); err != nil {
		return res, fmt.Errorf("wait for agent: %w", err)
	}
	// The restored guest's clock is behind and its random state is a copy: fix both first.
	if _, err := vm.Agent().Resume(readyCtx); err != nil {
		return res, fmt.Errorf("resume agent: %w", err)
	}
	res.BootMS = time.Since(vm.StartedAt()).Milliseconds()
	return res, runOn(ctx, readyCtx, vm, req, &res)
}
