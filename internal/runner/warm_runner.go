package runner

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/om8P/BeeVM/internal/pool"
	"github.com/om8P/BeeVM/internal/vmm"
)

// Optional extras a Runner may offer. The API checks for them with a type assertion, so the
// cold and snapshot runners do not need to implement them.
type (
	// Prewarmer starts up to n idle VMs for a function so its next calls are warm.
	Prewarmer interface {
		Prewarm(ctx context.Context, req Request, n int) (int, error)
	}
	// Drainer throws away a function's VMs (after a redeploy or delete).
	Drainer interface {
		Drain(functionID int64)
	}
	// VMLister lists the VMs of one tenant (tenantID <= 0: all).
	VMLister interface {
		VMs(tenantID int64) []pool.VMInfo
	}
	// Preparer starts building the function's snapshot in the background (after a deploy),
	// so later calls restore with the code already inside. It never blocks.
	Preparer interface {
		Prepare(req Request)
	}
	// SnapshotForgetter deletes a function's snapshots (after a redeploy or delete).
	SnapshotForgetter interface {
		ForgetFunction(functionID int64)
	}
	// RuntimeChecker says whether this server can run a runtime (it has the image).
	RuntimeChecker interface {
		HasRuntime(runtime string) bool
	}
)

// WarmRunner keeps VMs alive between calls (the warm pool). A call first looks for an idle VM
// of the same function; only if there is none is a VM restored from the base snapshot (or
// booted, if there is no snapshot) and given the function's code. After the call the VM goes
// back to the pool unless something went wrong.
type WarmRunner struct {
	snapshotPicker
	pool *pool.Pool
	base vmm.Config

	// create makes a ready VM with the code uploaded; invoke runs one call on it. Replaced in
	// unit tests.
	create func(ctx context.Context, req Request) (*warmVM, error)
	invoke func(ctx context.Context, vm *warmVM, req Request, res *Result) error
}

// warmVM is a VM in the pool, plus how it was made (reported on its first call).
type warmVM struct {
	id      string
	exited  <-chan struct{}
	destroy func()
	vm      *vmm.VM // nil in unit tests
	made    Result  // StartKind, BootMS and UploadMS of its creation
}

func (w *warmVM) ID() string              { return w.id }
func (w *warmVM) Exited() <-chan struct{} { return w.exited }
func (w *warmVM) Destroy()                { w.destroy() }

// NewWarmRunner creates a runner with a warm pool. base needs BaseDir and FirecrackerBin (and
// KernelPath and RootfsPath for booting). snaps may be nil: then new VMs are booted.
// cfg.MaxVMs limits the VMs on this node in any state, idle ones included.
func NewWarmRunner(base vmm.Config, snaps Snapshots, cfg pool.Config, logger *slog.Logger) *WarmRunner {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.Logger == nil {
		cfg.Logger = logger
	}
	w := &WarmRunner{snapshotPicker: newSnapshotPicker(snaps, logger), pool: pool.New(cfg), base: agentConfig(base)}
	w.create = w.newVM
	w.invoke = func(ctx context.Context, vm *warmVM, req Request, res *Result) error {
		return callFunction(ctx, vm.vm, req, res)
	}
	return w
}

// keyOf says which pool a call belongs to.
func keyOf(req Request) pool.Key {
	return pool.Key{TenantID: req.TenantID, FunctionID: req.FunctionID, CodeSHA256: req.CodeSHA256,
		MemoryMiB: req.MemoryMiB, VCPUs: req.VCPUs, Runtime: req.runtime(), Handler: req.Handler}
}

func (w *WarmRunner) creator(req Request) pool.CreateFunc {
	return func(ctx context.Context) (pool.Instance, error) { return w.create(ctx, req) }
}

// Run implements Runner.
func (w *WarmRunner) Run(ctx context.Context, req Request) (Result, error) {
	if !w.HasRuntime(req.Runtime) {
		return Result{}, ErrRuntimeUnavailable
	}
	start := time.Now()
	lease, err := w.pool.Acquire(ctx, keyOf(req), w.creator(req))
	if err != nil {
		if errors.Is(err, pool.ErrBusy) || errors.Is(err, pool.ErrClosed) {
			return Result{}, ErrBusy
		}
		res := Result{TotalMS: time.Since(start).Milliseconds()}
		// The user's code did not import: a function failure (502), not a server error.
		return res, loadFailure(err, req, &res)
	}
	vm := lease.Instance.(*warmVM)
	res := Result{VMID: vm.id, StartKind: StartWarm}
	if !lease.Warm {
		m := vm.made
		res.StartKind, res.Snapshot, res.BootMS, res.UploadMS, res.LoadMS = m.StartKind, m.Snapshot, m.BootMS, m.UploadMS, m.LoadMS
	}

	err = w.invoke(ctx, vm, req, &res)
	w.pool.Release(lease, healthyAfter(ctx, err, res))
	res.TotalMS = time.Since(start).Milliseconds()
	return res, err
}

// healthyAfter decides whether a VM may serve another call. A function that simply failed
// (non-zero exit, bad output) leaves a fine VM. Not reused:
//   - the agent or the connection failed,
//   - the caller hung up (the call may still be running inside the VM),
//   - the function timed out (the agent kills its process group, but something it started
//     in the background could survive),
//   - a python/node runtime exited (the VM can no longer run the function).
func healthyAfter(ctx context.Context, err error, res Result) bool {
	return err == nil && ctx.Err() == nil && !strings.HasPrefix(res.FunctionError, "timeout after") &&
		!strings.HasPrefix(res.FunctionError, "runtime exited")
}

// newVM makes a ready VM with the function's code loaded: restored from the function's own
// snapshot (code already inside), else from its runtime's snapshot (or booted when there is
// none) and then given the code. After such a slow start the function snapshot is built in
// the background for the next VMs.
func (w *WarmRunner) newVM(ctx context.Context, req Request) (*warmVM, error) {
	readyCtx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()

	cfg := w.configFor(w.base, req.Runtime)
	var res Result
	var vm *vmm.VM
	var err error
	codeInside := false
	if snap, ok := w.functionSnapshot(req); ok {
		res.StartKind, res.Snapshot, codeInside = StartRestore, SnapshotFunction, true
		vm, err = restoreReady(readyCtx, cfg, snap, req, &res)
	} else if snap, ok := w.snapshotFor(readyCtx, req); ok {
		res.StartKind, res.Snapshot = StartRestore, SnapshotRuntime
		vm, err = restoreReady(readyCtx, cfg, snap, req, &res)
	} else {
		res.StartKind = StartBoot
		vm, err = bootReady(readyCtx, cfg, req, &res)
	}
	if err != nil {
		return nil, err
	}
	if !codeInside {
		if err := putCode(readyCtx, vm, req, &res); err != nil {
			vm.Cleanup(false)
			return nil, err
		}
		w.Prepare(req)
	}
	return &warmVM{id: vm.ID(), exited: vm.Exited(), destroy: func() { _ = vm.Cleanup(false) }, vm: vm, made: res}, nil
}

// Prewarm implements Prewarmer: it starts up to n idle VMs for req's function, using only free
// slots.
func (w *WarmRunner) Prewarm(ctx context.Context, req Request, n int) (int, error) {
	started, err := w.pool.Prewarm(ctx, keyOf(req), n, w.creator(req))
	if errors.Is(err, pool.ErrBusy) {
		return started, ErrBusy
	}
	return started, err
}

// Drain implements Drainer.
func (w *WarmRunner) Drain(functionID int64) { w.pool.Drain(functionID) }

// VMs implements VMLister.
func (w *WarmRunner) VMs(tenantID int64) []pool.VMInfo { return w.pool.Stats(tenantID) }

// MaxVMs is the node's VM limit.
func (w *WarmRunner) MaxVMs() int { return w.pool.Config().MaxVMs }

// Close destroys idle VMs and waits (up to 30 s) for running calls to finish and their VMs to
// be destroyed. Call it after the HTTP server has stopped.
func (w *WarmRunner) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := w.pool.Close(ctx)
	// Background function snapshot builds run VMs too: wait for them before the server exits.
	return errors.Join(err, w.waitBuilds(30*time.Second))
}
