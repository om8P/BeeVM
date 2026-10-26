// Package runner runs one function call inside a MicroVM and returns the result.
//
// The API server only knows the Runner interface. Today's implementation, ColdRunner, boots a
// fresh VM for every call. Later milestones add runners that restore snapshots (M6) or reuse
// warm VMs (M7) without the API changing.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

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

// Result is what came back. If FunctionError is set, the VM worked but the function failed
// (non-zero exit, timeout, bad output); Output is then empty.
type Result struct {
	Output        json.RawMessage
	Stderr        string // the function's log
	ExitCode      int
	FunctionError string

	VMID       string
	BootMS     int64 // Start until the agent answered
	FunctionMS int64 // reported by the agent
	TotalMS    int64 // the whole Run
}

// Runner runs a function once.
type Runner interface {
	Run(ctx context.Context, req Request) (Result, error)
}

// ColdRunner boots a new VM for every call and throws it away afterwards: slow (about 1.7 s on
// WSL2) but simple and isolated. At most maxVMs run at once.
type ColdRunner struct {
	base      vmm.Config // paths and kernel settings; memory and vCPUs come from each request
	slots     chan struct{}
	queueWait time.Duration
}

// NewColdRunner creates a runner. base needs BaseDir, FirecrackerBin, KernelPath and RootfsPath.
// A call waits up to queueWait for a free slot, then fails with ErrBusy.
func NewColdRunner(base vmm.Config, maxVMs int, queueWait time.Duration) *ColdRunner {
	if maxVMs < 1 {
		maxVMs = 1
	}
	base.ExtraBootArgs = "beevm.mode=agent"
	return &ColdRunner{base: base, slots: make(chan struct{}, maxVMs), queueWait: queueWait}
}

// readyTimeout limits how long we wait for a VM to boot and its agent to answer.
const readyTimeout = 20 * time.Second

// Run implements Runner.
func (c *ColdRunner) Run(ctx context.Context, req Request) (Result, error) {
	start := time.Now()

	// Take a slot, or give up after queueWait (or when the caller goes away).
	wait := time.NewTimer(c.queueWait)
	defer wait.Stop()
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-wait.C:
		return Result{}, ErrBusy
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}

	cfg := c.base
	cfg.MemMiB = req.MemoryMiB
	cfg.VCPUs = req.VCPUs
	vm, err := vmm.New(cfg)
	if err != nil {
		return Result{}, fmt.Errorf("create vm: %w", err)
	}
	// The VM is disposable: Cleanup kills it at once and deletes its folder. No graceful
	// shutdown is needed, and skipping it keeps the response fast.
	defer vm.Cleanup(false)

	res := Result{VMID: vm.ID()}

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

	if _, err := vm.Agent().UploadFunctionFile(readyCtx, req.CodePath); err != nil {
		return res, fmt.Errorf("upload function: %w", err)
	}

	// The agent enforces the function's own limit; allow a little longer so its answer arrives.
	limit := time.Duration(req.TimeoutMS) * time.Millisecond
	callCtx, cancelCall := context.WithTimeout(ctx, limit+10*time.Second)
	defer cancelCall()
	out, err := vm.Agent().Invoke(callCtx, req.Payload, limit)
	if err != nil {
		return res, fmt.Errorf("invoke: %w", err)
	}
	res.Output, res.Stderr, res.ExitCode = out.Output, out.Stderr, out.ExitCode
	res.FunctionError = out.Error
	res.FunctionMS = out.DurationMS
	res.TotalMS = time.Since(start).Milliseconds()
	return res, nil
}
