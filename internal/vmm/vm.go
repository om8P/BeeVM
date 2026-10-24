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

	agent *AgentClient // talks to the guest agent over vsock

	exited       chan struct{} // closed when the firecracker process has exited
	shutdownOnce sync.Once     // machine.Shutdown closes a channel, so it may only run once
}

// New creates the VM's folder and copies the base rootfs into it. No process is started.
func New(cfg Config) (*VM, error) {
	cfg, err := cfg.applyDefaults()
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid vm config: %w", err)
	}
	if err := os.MkdirAll(cfg.Dir(), 0o700); err != nil {
		return nil, fmt.Errorf("create vm folder: %w", err)
	}
	if err := copyFile(cfg.RootfsPath, cfg.VMRootfsPath()); err != nil {
		_ = os.RemoveAll(cfg.Dir())
		return nil, fmt.Errorf("copy rootfs: %w", err)
	}
	return &VM{cfg: cfg, exited: make(chan struct{}), agent: newAgentClient(cfg.VsockPath())}, nil
}

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
	tick := time.NewTicker(10 * time.Millisecond)
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

	cmd := firecracker.VMCommandBuilder{}.
		WithBin(v.cfg.FirecrackerBin).
		WithSocketPath(v.cfg.SocketPath()).
		WithStdout(console).
		WithStderr(console).
		Build(vmCtx) // Stdin is nil, which means /dev/null

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
	return firecracker.Config{
		SocketPath:      v.cfg.SocketPath(),
		KernelImagePath: v.cfg.KernelPath,
		KernelArgs:      v.cfg.BootArgs(),
		Drives:          firecracker.NewDrivesBuilder(v.cfg.VMRootfsPath()).Build(),
		// Leave ID empty: Firecracker 1.x no longer uses vsock_id, and the SDK then omits it.
		VsockDevices: []firecracker.VsockDevice{{Path: v.cfg.VsockPath(), CID: v.cfg.VsockCID}},
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

// Stop asks the guest to shut down (Ctrl-Alt-Del) and waits until ctx ends; if the VM has
// not exited by then it is killed. It reports whether the shutdown was graceful.
func (v *VM) Stop(ctx context.Context) (graceful bool, err error) {
	v.mu.Lock()
	machine, started := v.machine, v.started
	v.mu.Unlock()
	if !started || machine == nil {
		return true, nil
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

// Cleanup kills the VM if it is still running and deletes its folder. With keepLogs the
// folder stays but only console.log is kept.
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

	if !keepLogs {
		return os.RemoveAll(v.cfg.Dir())
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
