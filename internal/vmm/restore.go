package vmm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// SnapshotFiles are the two files a Firecracker snapshot consists of.
type SnapshotFiles struct {
	StatePath string // CPU registers and device state (small)
	MemPath   string // the guest's whole memory (as large as the VM's RAM)
}

// RestoreTimings says where the time of a restore went.
type RestoreTimings struct {
	Spawn time.Duration // starting the firecracker process until its API socket answers
	Load  time.Duration // PUT /snapshot/load, including resuming the vCPUs
}

// Restore starts a new VM from a snapshot instead of booting one. The VM continues exactly
// where the snapshotted VM was paused; with a base snapshot that means the guest agent is
// already running and only needs to be reconnected (call WaitForAgent).
//
// The snapshot must have been taken from a VM with SharedRootfs (the disk path is stored in
// the snapshot) and the relative vsock path. cfg gives the folder, the firecracker binary and
// the ID; kernel, memory and vCPUs come from the snapshot. A restored VM cannot be stopped
// gracefully: it is meant to be used once and then killed (Cleanup).
func Restore(ctx context.Context, cfg Config, snap SnapshotFiles) (*VM, RestoreTimings, error) {
	var tm RestoreTimings
	cfg, err := cfg.applyDefaults()
	if err != nil {
		return nil, tm, err
	}
	if cfg.BaseDir == "" || cfg.FirecrackerBin == "" {
		return nil, tm, errors.New("restore: BaseDir and FirecrackerBin are required")
	}
	for _, p := range []string{snap.StatePath, snap.MemPath, cfg.FirecrackerBin} {
		if _, err := os.Stat(p); err != nil {
			return nil, tm, fmt.Errorf("restore: %w", err)
		}
	}
	if err := os.MkdirAll(cfg.Dir(), 0o700); err != nil {
		return nil, tm, fmt.Errorf("create vm folder: %w", err)
	}
	// Poll the agent every millisecond: after a restore it answers within a few ms.
	v := newVM(cfg, time.Millisecond)
	fail := func(err error) (*VM, RestoreTimings, error) {
		_ = v.Cleanup(false)
		return nil, tm, err
	}

	console, err := os.OpenFile(cfg.ConsolePath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fail(fmt.Errorf("open console log: %w", err))
	}
	cmd := exec.Command(cfg.FirecrackerBin, "--api-sock", cfg.SocketPath())
	cmd.Dir = cfg.Dir() // the snapshot's relative vsock path resolves inside this folder
	cmd.Stdout, cmd.Stderr = console, console

	t0 := time.Now()
	if err := cmd.Start(); err != nil {
		_ = console.Close()
		return fail(fmt.Errorf("start firecracker: %w", err))
	}
	v.mu.Lock()
	v.started, v.cmd, v.console, v.startedAt = true, cmd, console, t0
	v.mu.Unlock()
	go func() {
		_ = cmd.Wait()
		close(v.exited)
	}()

	api := newFCAPI(cfg.SocketPath())
	defer api.close()
	if err := api.waitReady(ctx, time.Millisecond, v.exited); err != nil {
		return fail(err)
	}
	tm.Spawn = time.Since(t0)

	if len(cfg.CPUs) > 0 {
		// Pin the threads that exist now; the vCPU thread is created during the load and
		// inherits the setting.
		if err := pinProcess(cmd.Process.Pid, cfg.CPUs); err != nil {
			return fail(err)
		}
	}

	if cfg.FirecrackerMetrics {
		if err := os.WriteFile(cfg.MetricsPath(), nil, 0o600); err != nil {
			return fail(err)
		}
		if err := api.put(ctx, "/metrics", map[string]string{"metrics_path": cfg.MetricsPath()}); err != nil {
			return fail(err)
		}
	}

	t1 := time.Now()
	if err := api.loadSnapshot(ctx, snap.StatePath, snap.MemPath); err != nil {
		return fail(fmt.Errorf("load snapshot: %w", err))
	}
	tm.Load = time.Since(t1)
	return v, tm, nil
}
