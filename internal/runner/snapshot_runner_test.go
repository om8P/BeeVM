package runner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/om8P/BeeVM/internal/snapshot"
	"github.com/om8P/BeeVM/internal/vmm"
)

// fakeSnaps stands in for the Snapshot Manager.
type fakeSnaps struct {
	err   error
	calls atomic.Int32
}

func (f *fakeSnaps) Ensure(ctx context.Context, mem, vcpus int64) (snapshot.Snapshot, error) {
	f.calls.Add(1)
	if f.err != nil {
		return snapshot.Snapshot{}, f.err
	}
	return snapshot.Snapshot{Meta: snapshot.Meta{Key: "k"}}, nil
}

// newFakeSnapshotRunner returns a runner whose restore and boot only record that they ran.
func newFakeSnapshotRunner(snaps Snapshots) (*SnapshotRunner, *atomic.Int32, *atomic.Int32) {
	r := NewSnapshotRunner(vmm.Config{}, snaps, 2, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	restores, boots := &atomic.Int32{}, &atomic.Int32{}
	r.restore = func(ctx context.Context, base vmm.Config, s snapshot.Snapshot, req Request) (Result, error) {
		restores.Add(1)
		if !base.SharedRootfs {
			return Result{}, errors.New("restored VMs must use the shared rootfs")
		}
		return Result{StartKind: StartRestore}, nil
	}
	r.boot = func(ctx context.Context, base vmm.Config, req Request) (Result, error) {
		boots.Add(1)
		return Result{StartKind: StartBoot}, nil
	}
	return r, restores, boots
}

func TestSnapshotRunnerRestores(t *testing.T) {
	r, restores, boots := newFakeSnapshotRunner(&fakeSnaps{})
	res, err := r.Run(context.Background(), Request{MemoryMiB: 128, VCPUs: 1})
	if err != nil || res.StartKind != StartRestore {
		t.Fatalf("got %+v, %v", res, err)
	}
	if restores.Load() != 1 || boots.Load() != 0 {
		t.Errorf("restores %d, boots %d", restores.Load(), boots.Load())
	}
}

func TestSnapshotRunnerFallsBackToBootAndDoesNotRetryAtOnce(t *testing.T) {
	snaps := &fakeSnaps{err: errors.New("firecracker failed")}
	r, restores, boots := newFakeSnapshotRunner(snaps)
	for i := 0; i < 3; i++ {
		res, err := r.Run(context.Background(), Request{MemoryMiB: 128, VCPUs: 1})
		if err != nil || res.StartKind != StartBoot {
			t.Fatalf("call %d: %+v, %v", i, res, err)
		}
	}
	if restores.Load() != 0 || boots.Load() != 3 {
		t.Errorf("restores %d, boots %d", restores.Load(), boots.Load())
	}
	if snaps.calls.Load() != 1 {
		t.Errorf("tried to build %d times within a minute, want 1", snaps.calls.Load())
	}
	// Another VM size is tried separately.
	_, _ = r.Run(context.Background(), Request{MemoryMiB: 256, VCPUs: 1})
	if snaps.calls.Load() != 2 {
		t.Errorf("a different size should try its own snapshot (calls %d)", snaps.calls.Load())
	}
}

func TestSnapshotRunnerBusy(t *testing.T) {
	r, _, _ := newFakeSnapshotRunner(&fakeSnaps{})
	r.queueWait = 30 * time.Millisecond
	r.slots <- struct{}{}
	r.slots <- struct{}{} // both slots taken
	if _, err := r.Run(context.Background(), Request{}); !errors.Is(err, ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
}

func TestSnapshotRunnerCallerGoneDoesNotMarkFailure(t *testing.T) {
	snaps := &fakeSnaps{err: context.Canceled}
	r, _, _ := newFakeSnapshotRunner(snaps)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.slots <- struct{}{}
	<-r.slots // keep the pool empty; acquire may still win the race with ctx.Done
	_, _ = r.snapshotFor(ctx, Request{MemoryMiB: 128, VCPUs: 1})
	if len(r.failedUntil) != 0 {
		t.Error("a cancelled caller must not block snapshots for everyone")
	}
}
