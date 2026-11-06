package vmm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/sirupsen/logrus"
)

// VM is one Firecracker MicroVM and its private working folder.
//
// Lifecycle: New (folder + rootfs copy) -> Start -> Stop or Kill -> Cleanup.
// Stop, Kill and Cleanup are safe to call more than once and from any state.
type VM struct {
	cfg Config

	mu        sync.Mutex
	started   bool
	startedAt time.Time
	cmd       *exec.Cmd
	machine   *firecracker.Machine
	console   *os.File
	cancelVM  context.CancelFunc

	agent     *AgentClient  // talks to the guest agent over vsock
	agentPoll time.Duration // how often WaitForAgent retries

	exited       chan struct{} // closed when the firecracker process has exited
	shutdownOnce sync.Once     // machine.Shutdown closes a channel, so it may only run once

	release     func() // undoes the Isolator's setup (nil without one)
	releaseOnce sync.Once
}

// New creates the VM's folder and (unless SharedRootfs) copies the base rootfs into it.
// With an Isolator it also sets up the VM's isolation and jail. No process is started.
func New(cfg Config) (*VM, error) {
	cfg, release, err := isolate(cfg)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*VM, error) {
		_ = os.RemoveAll(cfg.Dir())
		if cfg.Jail != nil {
			_ = removeJail(cfg)
		}
		if release != nil {
			release()
		}
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return fail(fmt.Errorf("invalid vm config: %w", err))
	}
	if err := os.MkdirAll(cfg.Dir(), 0o700); err != nil {
		return fail(fmt.Errorf("create vm folder: %w", err))
	}
	if !cfg.SharedRootfs {
		if err := copyFile(cfg.RootfsPath, cfg.VMRootfsPath()); err != nil {
			return fail(fmt.Errorf("copy rootfs: %w", err))
		}
	}
	if cfg.Jail != nil {
		if err := prepareJail(cfg, map[string]string{jailKernel: cfg.KernelPath, jailRootfs: cfg.RootfsPath}); err != nil {
			return fail(err)
		}
	}
	v := newVM(cfg, 10*time.Millisecond)
	v.release = release
	return v, nil
}

// isolate fills in defaults and lets the Isolator (if any) set up Jail and Net.
func isolate(cfg Config) (Config, func(), error) {
	cfg, err := cfg.applyDefaults()
	if err != nil {
		return cfg, nil, err
	}
	if cfg.Isolator == nil {
		return cfg, nil, nil
	}
	release, err := cfg.Isolator.Prepare(&cfg)
	if err != nil {
		return cfg, nil, fmt.Errorf("isolate vm: %w", err)
	}
	return cfg, release, nil
}

func newVM(cfg Config, agentPoll time.Duration) *VM {
	return &VM{cfg: cfg, exited: make(chan struct{}), agent: newAgentClient(cfg.VsockPath()), agentPoll: agentPoll}
}

// Config returns the VM's config (with defaults and isolation filled in).
func (v *VM) Config() Config { return v.cfg }

// Agent returns the client for this VM's guest agent. It only works when the guest runs with
// beevm.mode=agent, and after WaitForAgent has succeeded.
func (v *VM) Agent() *AgentClient { return v.agent }

// WaitForAgent polls the agent's /health until it answers, and returns how long that took
// since Start. It fails if the VM exits first or ctx ends.
func (v *VM) WaitForAgent(ctx context.Context) (time.Duration, error) {
	v.mu.Lock()
	t0 := v.startedAt
	v.mu.Unlock()
	if t0.IsZero() {
		return 0, errors.New("vm not started")
	}
	var lastErr error
	tick := time.NewTicker(v.agentPoll)
	defer tick.Stop()
	for {
		// A short limit per attempt: before the agent listens, Firecracker closes the
		// connection or the socket does not exist yet, and we simply try again.
		try, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		_, err := v.agent.Health(try)
		cancel()
		if err == nil {
			return time.Since(t0), nil
		}
		lastErr = err
		select {
		case <-v.exited:
			return 0, fmt.Errorf("vm exited before the agent answered (last error: %v)", lastErr)
		case <-ctx.Done():
			return 0, fmt.Errorf("waiting for agent: %w (last error: %v)", ctx.Err(), lastErr)
		case <-tick.C:
		}
	}
}

// ResumeInfo describes how a restored VM's agent came back.
type ResumeInfo struct {
	Ready           time.Duration // since the VM was started, until the resume call answered
	Attempts        int           // requests sent; more than 1 means the agent was not listening yet
	ClockAdjustedMS int64         // how far the guest clock was moved
}

// WaitAndResume is WaitForAgent and AgentClient.Resume in one request, for a VM that was just
// restored from a snapshot. It sends /resume until the agent answers, so the first answer
// already means "agent is up, clock and random state are fixed". That saves one round trip
// over vsock compared with polling /health first. Sending /resume twice is harmless.
func (v *VM) WaitAndResume(ctx context.Context) (ResumeInfo, error) {
	var info ResumeInfo
	v.mu.Lock()
	t0 := v.startedAt
	v.mu.Unlock()
	if t0.IsZero() {
		return info, errors.New("vm not started")
	}
	var lastErr error
	tick := time.NewTicker(v.agentPoll)
	defer tick.Stop()
	for {
		info.Attempts++
		try, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		res, err := v.agent.Resume(try)
		cancel()
		if err == nil {
			info.Ready, info.ClockAdjustedMS = time.Since(t0), res.ClockAdjustedMS
			return info, nil
		}
		var se *statusError
		if errors.As(err, &se) {
			return info, fmt.Errorf("resume agent: %w", err) // it answered "no": retrying won't help
		}
		lastErr = err
		select {
		case <-v.exited:
			return info, fmt.Errorf("vm exited before the agent answered (last error: %v)", lastErr)
		case <-ctx.Done():
			return info, fmt.Errorf("waiting for agent: %w (last error: %v)", ctx.Err(), lastErr)
		case <-tick.C:
		}
	}
}

// ID returns the VM's id (useful when it was generated).
func (v *VM) ID() string { return v.cfg.ID }

// Dir returns the VM's working folder.
func (v *VM) Dir() string { return v.cfg.Dir() }

// ConsolePath returns the file that receives the guest's serial output.
func (v *VM) ConsolePath() string { return v.cfg.ConsolePath() }

// PID returns the firecracker process id, or 0 if it has not started.
func (v *VM) PID() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.cmd == nil || v.cmd.Process == nil {
		return 0
	}
	return v.cmd.Process.Pid
}

// StartedAt is when the firecracker process was started (zero before Start or Restore).
func (v *VM) StartedAt() time.Time {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.startedAt
}

// Exited is closed when the firecracker process has ended.
func (v *VM) Exited() <-chan struct{} { return v.exited }

// Start launches Firecracker, configures the VM over its API socket, and boots it.
// ctx only limits how long Start may take; it does not tie the VM's life to ctx.
func (v *VM) Start(ctx context.Context) error {
	v.mu.Lock()
	if v.started {
		v.mu.Unlock()
		return errors.New("vm already started")
	}
	v.started = true
	v.mu.Unlock()

	console, err := os.OpenFile(v.cfg.ConsolePath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open console log: %w", err)
	}

	// The firecracker process is tied to this context, so it must outlive Start.
	vmCtx, cancel := context.WithCancel(context.Background())

	var cmd *exec.Cmd
	if v.cfg.Jail != nil {
		// The jailer makes the jail's root Firecracker's "/" and working folder.
		cmd = jailerCommand(vmCtx, v.cfg)
		cmd.Stdout, cmd.Stderr = console, console
	} else {
		cmd = firecracker.VMCommandBuilder{}.
			WithBin(v.cfg.FirecrackerBin).
			WithSocketPath(v.cfg.SocketPath()).
			WithStdout(console).
			WithStderr(console).
			Build(vmCtx) // Stdin is nil, which means /dev/null
		// Firecracker runs inside the VM folder, so relative paths (the vsock socket) land there.
		cmd.Dir = v.cfg.Dir()
	}

	logger := logrus.New()
	logger.SetOutput(io.Discard) // the SDK is chatty; real problems come back as errors
	machine, err := firecracker.NewMachine(vmCtx, v.sdkConfig(),
		firecracker.WithProcessRunner(cmd),
		firecracker.WithLogger(logrus.NewEntry(logger)))
	if err != nil {
		cancel()
		_ = console.Close()
		return fmt.Errorf("create machine: %w", err)
	}

	v.mu.Lock()
	v.cmd, v.machine, v.console, v.cancelVM = cmd, machine, console, cancel
	v.startedAt = time.Now()
	v.mu.Unlock()

	// Run the SDK's Start in a goroutine so the caller's deadline can interrupt it.
	startDone := make(chan error, 1)
	go func() { startDone <- machine.Start(vmCtx) }()

	select {
	case err = <-startDone:
	case <-ctx.Done():
		v.Kill()
		<-startDone
		err = ctx.Err()
	}

	// Watch for the process to end, so Stop/Kill/WaitForConsole can notice. This runs even
	// when Start failed, so Kill never waits for a signal that will not come.
	go func() {
		_ = machine.Wait(context.Background())
		close(v.exited)
	}()

	if err != nil {
		v.Kill()
		return fmt.Errorf("start vm: %w", err)
	}
	return nil
}

// sdkConfig translates our Config into the SDK's.
func (v *VM) sdkConfig() firecracker.Config {
	smtOff := false
	kernel, rootfs := v.cfg.KernelPath, v.cfg.VMRootfsPath()
	if v.cfg.Jail != nil {
		kernel, rootfs = "/"+jailKernel, "/"+jailRootfs // as Firecracker sees them in its jail
	}
	var nics firecracker.NetworkInterfaces
	if n := v.cfg.Net; n != nil {
		// No IP settings here: the guest's init configures eth0 itself (beevm.net=1).
		nics = firecracker.NetworkInterfaces{{StaticConfiguration: &firecracker.StaticNetworkConfiguration{
			MacAddress: n.GuestMAC, HostDevName: n.HostDev}}}
	}
	return firecracker.Config{
		SocketPath:      v.cfg.SocketPath(),
		KernelImagePath: kernel,
		KernelArgs:      v.cfg.BootArgs(),
		// In a jail the SDK cannot check the files (it looks for /vmlinux on the host); New
		// already checked the real ones.
		DisableValidation: v.cfg.Jail != nil,
		NetworkInterfaces: nics,
		// A shared rootfs is read-only (Firecracker then tells the kernel to mount it "ro"),
		// so many VMs can use the same file at once.
		Drives: firecracker.DrivesBuilder{}.WithRootDrive(rootfs,
			firecracker.WithReadOnly(v.cfg.SharedRootfs)).Build(),
		// Leave ID empty: Firecracker 1.x no longer uses vsock_id, and the SDK then omits it.
		// The path is relative to the VM folder (see VsockRelPath).
		VsockDevices: []firecracker.VsockDevice{{Path: VsockRelPath, CID: v.cfg.VsockCID}},
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  firecracker.Int64(v.cfg.VCPUs),
			MemSizeMib: firecracker.Int64(v.cfg.MemMiB),
			Smt:        &smtOff,
		},
		// An empty (non-nil) list turns off the SDK's default of forwarding Ctrl-C to
		// firecracker, so the caller decides how to stop the VM.
		ForwardSignals: []os.Signal{},
	}
}

// WaitForConsole waits until the guest prints a line equal to marker, and returns how long
// that took since Start. It fails if the VM exits first or ctx ends.
func (v *VM) WaitForConsole(ctx context.Context, marker string) (time.Duration, error) {
	v.mu.Lock()
	t0 := v.startedAt
	v.mu.Unlock()
	if t0.IsZero() {
		return 0, errors.New("vm not started")
	}
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if found, err := consoleHasLine(v.cfg.ConsolePath(), marker); err != nil {
			return 0, err
		} else if found {
			return time.Since(t0), nil
		}
		select {
		case <-v.exited:
			// One last look: the marker may have been written just before exit.
			if found, _ := consoleHasLine(v.cfg.ConsolePath(), marker); found {
				return time.Since(t0), nil
			}
			return 0, fmt.Errorf("vm exited before printing %q", marker)
		case <-ctx.Done():
			return 0, fmt.Errorf("waiting for %q: %w", marker, ctx.Err())
		case <-tick.C:
		}
	}
}

// consoleHasLine reports whether any line of the file equals marker (ignoring surrounding spaces).
func consoleHasLine(path, marker string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == marker {
			return true, nil
		}
	}
	return false, nil
}

// Pause freezes the VM's vCPUs. A snapshot can only be taken of a paused VM.
func (v *VM) Pause(ctx context.Context) error {
	v.mu.Lock()
	machine, started := v.machine, v.started
	v.mu.Unlock()
	switch {
	case machine != nil:
		return machine.PauseVM(ctx)
	case started: // restored: no SDK machine, talk to Firecracker directly
		api := newFCAPI(v.cfg.SocketPath())
		defer api.close()
		return api.patch(ctx, "/vm", map[string]string{"state": "Paused"})
	}
	return errors.New("vm not started")
}

// CreateSnapshot writes the paused VM's memory (memPath) and its CPU and device state
// (statePath). Both files must not exist yet. A restored VM can be snapshotted again (that is
// how function snapshots are made from a runtime snapshot).
func (v *VM) CreateSnapshot(ctx context.Context, memPath, statePath string) error {
	v.mu.Lock()
	machine, started := v.machine, v.started
	v.mu.Unlock()
	if !started {
		return errors.New("vm not started")
	}
	// A jailed Firecracker can only write inside its jail: it writes there, then we move the
	// files out (same filesystem, so a rename) and hand them to the owner of BeeVM's folder.
	fcMem, fcState := memPath, statePath
	if j := v.cfg.Jail; j != nil {
		fcMem, fcState = "/"+jailNewMem, "/"+jailNewState
	}
	var err error
	if machine != nil {
		err = machine.CreateSnapshot(ctx, fcMem, fcState)
	} else {
		api := newFCAPI(v.cfg.SocketPath())
		defer api.close()
		err = api.put(ctx, "/snapshot/create", map[string]string{
			"snapshot_type": "Full", "snapshot_path": fcState, "mem_file_path": fcMem,
		})
	}
	if err != nil || v.cfg.Jail == nil {
		return err
	}
	root := v.cfg.JailRoot()
	if err := moveOut(filepath.Join(root, jailNewMem), memPath, v.cfg.Jail.FileOwner); err != nil {
		return fmt.Errorf("move snapshot out of the jail: %w", err)
	}
	if err := moveOut(filepath.Join(root, jailNewState), statePath, v.cfg.Jail.FileOwner); err != nil {
		return fmt.Errorf("move snapshot out of the jail: %w", err)
	}
	return nil
}

// Stop asks the guest to shut down (Ctrl-Alt-Del) and waits until ctx ends; if the VM has
// not exited by then it is killed. It reports whether the shutdown was graceful.
func (v *VM) Stop(ctx context.Context) (graceful bool, err error) {
	v.mu.Lock()
	machine, started := v.machine, v.started
	v.mu.Unlock()
	if !started {
		return true, nil
	}
	if machine == nil {
		// A restored VM has no SDK machine; it is disposable, so stopping means killing.
		v.Kill()
		return false, nil
	}
	select {
	case <-v.exited:
		return true, nil
	default:
	}

	v.shutdownOnce.Do(func() {
		if serr := machine.Shutdown(ctx); serr != nil {
			err = fmt.Errorf("send ctrl-alt-del: %w", serr)
		}
	})
	if err == nil {
		select {
		case <-v.exited:
			return true, nil
		case <-ctx.Done():
			err = fmt.Errorf("guest did not shut down in time: %w", ctx.Err())
		}
	}
	v.Kill()
	return false, err
}

// Kill ends the firecracker process immediately and waits for it to be gone.
func (v *VM) Kill() {
	v.mu.Lock()
	cmd, started := v.cmd, v.started
	v.mu.Unlock()
	if !started || cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill() // an error here just means it already exited
	select {
	case <-v.exited:
	case <-time.After(5 * time.Second):
	}
}

// Cleanup kills the VM if it is still running and deletes its folder (and its jail, and undoes
// its isolation). With keepLogs the folder stays but only console.log is kept.
func (v *VM) Cleanup(keepLogs bool) error {
	v.Kill()
	v.agent.Close()
	v.mu.Lock()
	if v.cancelVM != nil {
		v.cancelVM()
	}
	if v.console != nil {
		_ = v.console.Close()
		v.console = nil
	}
	v.mu.Unlock()

	jailErr := removeJail(v.cfg)
	// Only after the process is gone: the isolator then frees the VM's UID, cgroup and network.
	v.releaseOnce.Do(func() {
		if v.release != nil {
			v.release()
		}
	})
	if !keepLogs {
		return errors.Join(jailErr, os.RemoveAll(v.cfg.Dir()))
	}
	entries, err := os.ReadDir(v.cfg.Dir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var errs []error
	for _, e := range entries {
		if e.Name() == filepath.Base(v.cfg.ConsolePath()) {
			continue
		}
		errs = append(errs, os.RemoveAll(filepath.Join(v.cfg.Dir(), e.Name())))
	}
	return errors.Join(errs...)
}

// copyFile copies src to dst (dst is created, mode 0600).
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
