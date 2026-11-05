// Package runner runs one function call inside a MicroVM and returns the result.
//
// The API server only knows the Runner interface. ColdRunner boots a fresh VM for every
// call; SnapshotRunner restores one from a snapshot instead (milestone 6), which is about 25
// times faster. WarmRunner (milestone 7) keeps VMs alive between calls in a warm pool, so
// repeat calls skip the restore and the upload.
//
// Milestone 8 added runtimes (binary, python, node) and function snapshots: a VM is restored
// from the function's own snapshot (code already uploaded and imported) when there is one,
// otherwise from its runtime's snapshot, and then gets the code. After such a slow start the
// function snapshot is built in the background, so later VMs start the fast way.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/om8P/BeeVM/internal/agentproto"
	"github.com/om8P/BeeVM/internal/pkg"
	"github.com/om8P/BeeVM/internal/snapshot"
	"github.com/om8P/BeeVM/internal/vmm"
)

// ErrBusy means all VM slots are in use and none became free in time. The API answers 503.
var ErrBusy = errors.New("too many VMs running, try again shortly")

// Request is one function call.
type Request struct {
	CodePath  string          // the function binary (or zip package) on the host
	Payload   json.RawMessage // JSON input for the function
	Runtime   string          // "binary" (also when empty), "python" or "node"
	Handler   string          // python/node: the function to call inside the package
	MemoryMiB int64
	VCPUs     int64
	TimeoutMS int64 // limit for the function itself

	// Which function this is. The warm pool reuses a VM only for calls of the same function
	// with the same code, and function snapshots belong to one function.
	TenantID   int64
	FunctionID int64
	CodeSHA256 string
}

func (r Request) runtime() string {
	if r.Runtime == "" {
		return agentproto.RuntimeBinary
	}
	return r.Runtime
}

// How a VM was started for a call.
const (
	StartBoot    = "boot"    // a full boot of a new VM
	StartRestore = "restore" // restored from a snapshot
	StartWarm    = "warm"    // an idle VM from the warm pool, code already loaded
)

// Which snapshot a restored VM came from.
const (
	SnapshotFunction = "function" // the function's own snapshot: code already uploaded and imported
	SnapshotRuntime  = "runtime"  // the runtime's snapshot: code still had to be uploaded (and imported)
)

// ErrRuntimeUnavailable means this server has no image for the function's runtime (for
// example `make images` was not run for Python). The API answers 503.
var ErrRuntimeUnavailable = errors.New("runtime not available on this server")

// Result is what came back. If FunctionError is set, the VM worked but the function failed
// (non-zero exit, timeout, bad output, import error); Output is then empty.
type Result struct {
	Output        json.RawMessage
	Stderr        string // the function's log
	ExitCode      int
	FunctionError string

	VMID       string
	StartKind  string // StartBoot, StartRestore or StartWarm
	Snapshot   string // for StartRestore: SnapshotFunction or SnapshotRuntime
	BootMS     int64  // starting the VM until its agent was ready
	UploadMS   int64  // sending the function's code into the VM
	LoadMS     int64  // python/node: starting the runtime and importing the code
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

// images knows the root filesystem image of each runtime. Binaries use the base config's
// RootfsPath; python and node are added with SetRootfs.
type images struct {
	mu     sync.Mutex
	rootfs map[string]string
}

// SetRootfs sets the root filesystem image of a runtime ("python", "node").
func (i *images) SetRootfs(runtime, path string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.rootfs == nil {
		i.rootfs = map[string]string{}
	}
	i.rootfs[runtime] = path
}

// HasRuntime implements RuntimeChecker: binaries always run; python and node need their image.
func (i *images) HasRuntime(runtime string) bool {
	if runtime == "" || runtime == agentproto.RuntimeBinary {
		return true
	}
	i.mu.Lock()
	path := i.rootfs[runtime]
	i.mu.Unlock()
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// configFor is base with the runtime's root filesystem.
func (i *images) configFor(base vmm.Config, runtime string) vmm.Config {
	if runtime == "" || runtime == agentproto.RuntimeBinary {
		return base
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if p := i.rootfs[runtime]; p != "" {
		base.RootfsPath = p
	}
	return base
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
	images
	base vmm.Config // paths and kernel settings; memory and vCPUs come from each request
}

// NewColdRunner creates a runner. base needs BaseDir, FirecrackerBin, KernelPath and RootfsPath.
// A call waits up to queueWait for a free slot, then fails with ErrBusy.
func NewColdRunner(base vmm.Config, maxVMs int, queueWait time.Duration) *ColdRunner {
	return &ColdRunner{limiter: newLimiter(maxVMs, queueWait), base: agentConfig(base)}
}

// Run implements Runner.
func (c *ColdRunner) Run(ctx context.Context, req Request) (Result, error) {
	if !c.HasRuntime(req.Runtime) {
		return Result{}, ErrRuntimeUnavailable
	}
	start := time.Now()
	release, err := c.acquire(ctx)
	if err != nil {
		return Result{}, err
	}
	defer release()
	res, err := bootAndRun(ctx, c.configFor(c.base, req.Runtime), req)
	res.TotalMS = time.Since(start).Milliseconds()
	return res, err
}

// bootAndRun boots a VM, runs the call on it and throws it away. The caller holds a slot.
func bootAndRun(ctx context.Context, base vmm.Config, req Request) (Result, error) {
	readyCtx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	res := Result{StartKind: StartBoot}
	vm, err := bootReady(readyCtx, base, req, &res)
	if err != nil {
		return res, err
	}
	// The VM is disposable: Cleanup kills it at once and deletes its folder. No graceful
	// shutdown is needed, and skipping it keeps the response fast.
	defer vm.Cleanup(false)
	return res, runOn(ctx, readyCtx, vm, req, &res, false)
}

// bootReady boots a new VM and waits until its agent answers. It fills in VMID and BootMS.
// On error nothing is left behind.
func bootReady(ctx context.Context, base vmm.Config, req Request, res *Result) (*vmm.VM, error) {
	cfg := base
	cfg.MemMiB = req.MemoryMiB
	cfg.VCPUs = req.VCPUs
	vm, err := vmm.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("create vm: %w", err)
	}
	res.VMID = vm.ID()
	if err := vm.Start(ctx); err != nil {
		vm.Cleanup(false)
		return nil, fmt.Errorf("start vm: %w", err)
	}
	boot, err := vm.WaitForAgent(ctx)
	if err != nil {
		vm.Cleanup(false)
		return nil, fmt.Errorf("wait for agent: %w", err)
	}
	res.BootMS = boot.Milliseconds()
	return vm, nil
}

// runOn gives a ready VM the function's code (unless codeInside: it came from a function
// snapshot) and calls it once. Code that does not import is a function failure, not an error.
func runOn(ctx, readyCtx context.Context, vm *vmm.VM, req Request, res *Result, codeInside bool) error {
	if !codeInside {
		if err := putCode(readyCtx, vm, req, res); err != nil {
			return loadFailure(err, req, res)
		}
	}
	return callFunction(ctx, vm, req, res)
}

// putCode uploads the function and, for python/node, imports it.
func putCode(ctx context.Context, vm *vmm.VM, req Request, res *Result) error {
	if err := uploadCode(ctx, vm, req, res); err != nil {
		return err
	}
	return loadCode(ctx, vm, req, res)
}

// uploadCode sends the function's binary (or zip package) into a ready VM.
func uploadCode(ctx context.Context, vm *vmm.VM, req Request, res *Result) error {
	t0 := time.Now()
	var err error
	if pkg.IsPackaged(req.runtime()) {
		_, err = vm.Agent().UploadPackageFile(ctx, req.CodePath)
	} else {
		_, err = vm.Agent().UploadFunctionFile(ctx, req.CodePath)
	}
	if err != nil {
		return fmt.Errorf("upload function: %w", err)
	}
	res.UploadMS = time.Since(t0).Milliseconds()
	return nil
}

// loadCode starts the runtime and imports the uploaded package (python/node only).
func loadCode(ctx context.Context, vm *vmm.VM, req Request, res *Result) error {
	if !pkg.IsPackaged(req.runtime()) {
		return nil
	}
	t0 := time.Now()
	_, err := vm.Agent().Load(ctx, req.runtime(), req.Handler, 0)
	res.LoadMS = time.Since(t0).Milliseconds()
	if err != nil {
		return fmt.Errorf("load function: %w", err)
	}
	return nil
}

// loadFailure turns an import error of the user's code into a function failure (the API
// answers 502 with the traceback as the log). Other errors stay errors.
func loadFailure(err error, req Request, res *Result) error {
	var le *vmm.LoadError
	if !errors.As(err, &le) {
		return err
	}
	res.ExitCode = 1
	res.Stderr = le.Msg
	res.FunctionError = "function failed to load: " + summaryLine(req.runtime(), le.Msg)
	return nil
}

// summaryLine picks the line of an error text that says what went wrong: Python prints it
// last, Node.js first.
func summaryLine(runtime, text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	line := lines[0]
	if runtime == agentproto.RuntimePython {
		line = lines[len(lines)-1]
	}
	return strings.TrimSpace(line)
}

// callFunction runs the already uploaded function once and fills in res.
func callFunction(ctx context.Context, vm *vmm.VM, req Request, res *Result) error {
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
// use a fake). With only this, VMs are restored from the binary runtime snapshot.
type Snapshots interface {
	Ensure(ctx context.Context, memMiB, vcpus int64) (snapshot.Snapshot, error)
}

// RuntimeSnapshots is the rest of the Snapshot Manager: runtime snapshots for python and node,
// and function snapshots. A Snapshots that also implements it gets the full behaviour.
type RuntimeSnapshots interface {
	EnsureRuntime(ctx context.Context, runtime string, memMiB, vcpus int64) (snapshot.Snapshot, error)
	LookupFunction(spec snapshot.FunctionSpec) (snapshot.Snapshot, bool)
	EnsureFunction(ctx context.Context, spec snapshot.FunctionSpec) (snapshot.Snapshot, error)
	RemoveFunction(functionID int64) (int, error)
}

// SnapshotRunner restores a VM from a snapshot for every call: the guest is already booted
// and its agent running, so the VM is ready in tens of milliseconds. Snapshots are built the
// first time they are needed. If a runtime snapshot cannot be built, calls fall back to a full
// boot (and building is not retried for a minute).
type SnapshotRunner struct {
	limiter
	snapshotPicker
	base vmm.Config

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
		limiter: newLimiter(maxVMs, queueWait), base: agentConfig(base),
		snapshotPicker: newSnapshotPicker(snaps, logger), restore: restoreAndRun, boot: bootAndRun,
	}
}

// snapshotPicker finds the snapshot for a call's VM, builds function snapshots in the
// background, and remembers snapshots that could not be built so it does not retry at once.
type snapshotPicker struct {
	images
	snaps  Snapshots
	logger *slog.Logger

	mu          sync.Mutex
	failedUntil map[string]time.Time // snapshot -> do not try to build before
	building    map[string]bool      // function snapshots being built in the background
	closed      bool                 // no new background builds
	builds      sync.WaitGroup       // background builds still running
}

func newSnapshotPicker(snaps Snapshots, logger *slog.Logger) snapshotPicker {
	return snapshotPicker{snaps: snaps, logger: logger, failedUntil: map[string]time.Time{}, building: map[string]bool{}}
}

// Run implements Runner.
func (s *SnapshotRunner) Run(ctx context.Context, req Request) (Result, error) {
	if !s.HasRuntime(req.Runtime) {
		return Result{}, ErrRuntimeUnavailable
	}
	start := time.Now()
	release, err := s.acquire(ctx)
	if err != nil {
		return Result{}, err
	}
	defer release()

	cfg := s.configFor(s.base, req.Runtime)
	var res Result
	if snap, ok := s.functionSnapshot(req); ok {
		res, err = s.restore(ctx, cfg, snap, req)
	} else if snap, ok := s.snapshotFor(ctx, req); ok {
		res, err = s.restore(ctx, cfg, snap, req)
		if err == nil && !strings.HasPrefix(res.FunctionError, "function failed to load") {
			s.Prepare(req) // next time, start with the code already inside
		}
	} else {
		res, err = s.boot(ctx, cfg, req)
	}
	res.TotalMS = time.Since(start).Milliseconds()
	return res, err
}

// Close waits for background snapshot builds (so no VM is left running when the server exits).
func (s *SnapshotRunner) Close() error { return s.waitBuilds(30 * time.Second) }

// failed reports whether building key failed recently.
func (s *snapshotPicker) failed(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Now().Before(s.failedUntil[key])
}

func (s *snapshotPicker) markFailed(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failedUntil[key] = time.Now().Add(retryBuildAfter)
}

// snapshotFor returns the runtime snapshot for this call's runtime and VM size, building it
// if needed. It reports false when there is none and the call should boot instead.
func (s *snapshotPicker) snapshotFor(ctx context.Context, req Request) (snapshot.Snapshot, bool) {
	if s.snaps == nil {
		return snapshot.Snapshot{}, false
	}
	key := fmt.Sprintf("%s/%d/%d", req.runtime(), req.MemoryMiB, req.VCPUs)
	if s.failed(key) {
		return snapshot.Snapshot{}, false
	}
	var snap snapshot.Snapshot
	var err error
	if rs, ok := s.snaps.(RuntimeSnapshots); ok {
		snap, err = rs.EnsureRuntime(ctx, req.runtime(), req.MemoryMiB, req.VCPUs)
	} else if req.runtime() == agentproto.RuntimeBinary {
		snap, err = s.snaps.Ensure(ctx, req.MemoryMiB, req.VCPUs)
	} else {
		return snapshot.Snapshot{}, false
	}
	if err != nil {
		if ctx.Err() == nil { // not just the caller hanging up
			s.logger.Error("snapshot unavailable, booting instead", "runtime", req.runtime(), "memory_mib", req.MemoryMiB,
				"vcpus", req.VCPUs, "retry_in", retryBuildAfter.String(), "error", err)
			s.markFailed(key)
		}
		return snapshot.Snapshot{}, false
	}
	return snap, true
}

func specOf(req Request) snapshot.FunctionSpec {
	return snapshot.FunctionSpec{Runtime: req.Runtime, Handler: req.Handler, TenantID: req.TenantID,
		FunctionID: req.FunctionID, CodeSHA256: req.CodeSHA256, CodePath: req.CodePath,
		MemMiB: req.MemoryMiB, VCPUs: req.VCPUs}
}

// functionSnapshot returns the call's function snapshot if it has been built. It never builds.
func (s *snapshotPicker) functionSnapshot(req Request) (snapshot.Snapshot, bool) {
	rs, ok := s.snaps.(RuntimeSnapshots)
	if !ok || req.FunctionID == 0 {
		return snapshot.Snapshot{}, false
	}
	return rs.LookupFunction(specOf(req))
}

func functionBuildKey(req Request) string {
	return fmt.Sprintf("fn/%d/%s/%s/%d/%d", req.FunctionID, req.CodeSHA256, req.Handler, req.MemoryMiB, req.VCPUs)
}

// Prepare implements Preparer: it starts building req's function snapshot in the background,
// unless it exists, is being built, or failed less than a minute ago. It never blocks.
func (s *snapshotPicker) Prepare(req Request) {
	rs, ok := s.snaps.(RuntimeSnapshots)
	if !ok || req.FunctionID == 0 || req.CodePath == "" || !s.HasRuntime(req.Runtime) {
		return
	}
	spec := specOf(req)
	if _, ok := rs.LookupFunction(spec); ok {
		return
	}
	key := functionBuildKey(req)
	s.mu.Lock()
	if s.closed || s.building[key] || time.Now().Before(s.failedUntil[key]) {
		s.mu.Unlock()
		return
	}
	s.building[key] = true
	s.builds.Add(1)
	s.mu.Unlock()

	go func() {
		defer s.builds.Done()
		_, err := rs.EnsureFunction(context.Background(), spec)
		s.mu.Lock()
		delete(s.building, key)
		if err != nil {
			s.failedUntil[key] = time.Now().Add(retryBuildAfter)
		}
		s.mu.Unlock()
		if err != nil {
			s.logger.Warn("function snapshot not built; calls restore the runtime snapshot instead",
				"function_id", req.FunctionID, "runtime", req.runtime(), "error", err)
		}
	}()
}

// ForgetFunction implements SnapshotForgetter: it deletes the function's snapshots and
// forgets failed builds for it (a redeploy may have fixed the code).
func (s *snapshotPicker) ForgetFunction(functionID int64) {
	rs, ok := s.snaps.(RuntimeSnapshots)
	if !ok {
		return
	}
	if _, err := rs.RemoveFunction(functionID); err != nil {
		s.logger.Warn("remove function snapshots", "function_id", functionID, "error", err)
	}
	prefix := fmt.Sprintf("fn/%d/", functionID)
	s.mu.Lock()
	for k := range s.failedUntil {
		if strings.HasPrefix(k, prefix) {
			delete(s.failedUntil, k)
		}
	}
	s.mu.Unlock()
}

// waitBuilds stops new background builds and waits up to max for running ones.
func (s *snapshotPicker) waitBuilds(max time.Duration) error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.builds.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-time.After(max):
		return errors.New("snapshot builds still running")
	}
}

// restoreAndRun restores a VM from snap, runs the call on it and throws it away. The caller
// holds a slot. A VM from a function snapshot already has the code.
func restoreAndRun(ctx context.Context, base vmm.Config, snap snapshot.Snapshot, req Request) (Result, error) {
	res := Result{StartKind: StartRestore, Snapshot: snapshotKind(snap)}
	readyCtx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	vm, err := restoreReady(readyCtx, base, snap, req, &res)
	if err != nil {
		return res, err
	}
	defer vm.Cleanup(false)
	return res, runOn(ctx, readyCtx, vm, req, &res, snap.IsFunction())
}

func snapshotKind(snap snapshot.Snapshot) string {
	if snap.IsFunction() {
		return SnapshotFunction
	}
	return SnapshotRuntime
}

// restoreReady restores a VM from snap and waits until its agent answers. It fills in VMID and
// BootMS. On error nothing is left behind.
func restoreReady(ctx context.Context, base vmm.Config, snap snapshot.Snapshot, req Request, res *Result) (*vmm.VM, error) {
	cfg := base
	cfg.MemMiB, cfg.VCPUs = req.MemoryMiB, req.VCPUs // informational; the snapshot decides
	vm, _, err := vmm.Restore(ctx, cfg, snap.Files)
	if err != nil {
		return nil, fmt.Errorf("restore vm: %w", err)
	}
	res.VMID = vm.ID()

	// The restored guest's clock is behind and its random state is a copy: the first request
	// that reaches the agent fixes both, and its answer also means the agent is ready.
	info, err := vm.WaitAndResume(ctx)
	if err != nil {
		vm.Cleanup(false)
		return nil, fmt.Errorf("wait for agent: %w", err)
	}
	res.BootMS = info.Ready.Milliseconds()
	return vm, nil
}
