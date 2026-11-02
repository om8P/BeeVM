package runner

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/om8P/BeeVM/internal/pool"
	"github.com/om8P/BeeVM/internal/vmm"
)

// fakeWarm returns a WarmRunner whose VMs are fakes. invoke decides what each call returns.
type fakeWarm struct {
	*WarmRunner
	created   atomic.Int32
	mu        sync.Mutex
	destroyed map[string]bool
}

func newFakeWarm(t *testing.T, maxVMs int, invoke func(ctx context.Context, res *Result) error) *fakeWarm {
	t.Helper()
	w := NewWarmRunner(vmm.Config{}, nil, pool.Config{MaxVMs: maxVMs, KeepAlive: time.Minute, QueueWait: 50 * time.Millisecond}, nil)
	f := &fakeWarm{WarmRunner: w, destroyed: map[string]bool{}}
	w.create = func(ctx context.Context, req Request) (*warmVM, error) {
		id := fmt.Sprintf("vm-%d", f.created.Add(1))
		exited := make(chan struct{})
		var once sync.Once
		return &warmVM{id: id, exited: exited, made: Result{StartKind: StartRestore, BootMS: 45, UploadMS: 20},
			destroy: func() {
				once.Do(func() { close(exited) })
				f.mu.Lock()
				f.destroyed[id] = true
				f.mu.Unlock()
			}}, nil
	}
	w.invoke = func(ctx context.Context, vm *warmVM, req Request, res *Result) error { return invoke(ctx, res) }
	t.Cleanup(func() { _ = w.Close() })
	return f
}

func (f *fakeWarm) isDestroyed(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.destroyed[id]
}

func ok(ctx context.Context, res *Result) error { res.Output = []byte(`{}`); return nil }

var reqA = Request{TenantID: 1, FunctionID: 7, CodeSHA256: "abc", MemoryMiB: 128, VCPUs: 1, TimeoutMS: 1000}

func TestWarmRunnerSecondCallIsWarm(t *testing.T) {
	f := newFakeWarm(t, 2, ok)
	r1, err := f.Run(context.Background(), reqA)
	if err != nil || r1.StartKind != StartRestore || r1.BootMS != 45 || r1.UploadMS != 20 {
		t.Fatalf("first call: %+v, %v", r1, err)
	}
	r2, err := f.Run(context.Background(), reqA)
	if err != nil || r2.StartKind != StartWarm || r2.VMID != r1.VMID || r2.BootMS != 0 || r2.UploadMS != 0 {
		t.Fatalf("second call: %+v, %v", r2, err)
	}
	if f.created.Load() != 1 {
		t.Errorf("created %d VMs", f.created.Load())
	}
}

func TestWarmRunnerKeepsOrRecycles(t *testing.T) {
	cases := []struct {
		name   string
		invoke func(ctx context.Context, res *Result) error
		keep   bool
	}{
		{"success", ok, true},
		{"function failed", func(ctx context.Context, res *Result) error {
			res.FunctionError, res.ExitCode = "exit status 1", 1
			return nil
		}, true},
		{"function timed out", func(ctx context.Context, res *Result) error {
			res.FunctionError, res.ExitCode = "timeout after 1s", -1
			return nil
		}, false},
		{"agent error", func(ctx context.Context, res *Result) error { return errors.New("invoke: connection reset") }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeWarm(t, 2, c.invoke)
			r, _ := f.Run(context.Background(), reqA)
			if c.keep {
				if vms := f.VMs(0); len(vms) != 1 || vms[0].State != pool.Idle {
					t.Errorf("VM should be idle, got %+v", vms)
				}
				return
			}
			deadline := time.Now().Add(2 * time.Second)
			for !f.isDestroyed(r.VMID) {
				if time.Now().After(deadline) {
					t.Fatal("VM not destroyed")
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}

func TestWarmRunnerCallerHungUp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := newFakeWarm(t, 2, func(_ context.Context, res *Result) error {
		cancel() // the caller leaves while the call runs
		return context.Canceled
	})
	r, err := f.Run(ctx, reqA)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !f.isDestroyed(r.VMID) {
		if time.Now().After(deadline) {
			t.Fatal("a VM whose call was abandoned must not be reused")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestWarmRunnerBusy(t *testing.T) {
	release := make(chan struct{})
	f := newFakeWarm(t, 1, func(ctx context.Context, res *Result) error { <-release; return nil })
	go func() { _, _ = f.Run(context.Background(), reqA) }()
	time.Sleep(20 * time.Millisecond)
	other := reqA
	other.FunctionID = 8
	if _, err := f.Run(context.Background(), other); !errors.Is(err, ErrBusy) {
		t.Errorf("want ErrBusy, got %v", err)
	}
	close(release)
}

func TestWarmRunnerCreateFails(t *testing.T) {
	f := newFakeWarm(t, 1, ok)
	f.create = func(ctx context.Context, req Request) (*warmVM, error) { return nil, errors.New("restore vm: boom") }
	if _, err := f.Run(context.Background(), reqA); err == nil || errors.Is(err, ErrBusy) {
		t.Fatalf("want the restore error, got %v", err)
	}
	if len(f.VMs(0)) != 0 {
		t.Error("failed VM listed")
	}
}

func TestWarmRunnerPrewarmDrainAndList(t *testing.T) {
	f := newFakeWarm(t, 3, ok)
	n, err := f.Prewarm(context.Background(), reqA, 2)
	if err != nil || n != 2 {
		t.Fatalf("prewarm: %d, %v", n, err)
	}
	r, _ := f.Run(context.Background(), reqA)
	if r.StartKind != StartWarm {
		t.Errorf("call after prewarm: %s", r.StartKind)
	}
	other := reqA
	other.TenantID, other.FunctionID = 2, 9
	_, _ = f.Run(context.Background(), other)
	if got := len(f.VMs(1)); got != 2 {
		t.Errorf("tenant 1 sees %d VMs, want 2", got)
	}
	if got := len(f.VMs(2)); got != 1 {
		t.Errorf("tenant 2 sees %d VMs, want 1", got)
	}
	if _, err := f.Prewarm(context.Background(), other, 1); !errors.Is(err, ErrBusy) {
		t.Errorf("full node prewarm: %v", err)
	}

	f.Drain(reqA.FunctionID)
	deadline := time.Now().Add(2 * time.Second)
	for len(f.VMs(1)) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("drain left VMs")
		}
		time.Sleep(time.Millisecond)
	}
	if f.MaxVMs() != 3 {
		t.Error("MaxVMs")
	}
}

func TestWarmRunnerImplementsExtras(t *testing.T) {
	var r Runner = NewWarmRunner(vmm.Config{}, nil, pool.Config{}, nil)
	if _, ok := r.(Prewarmer); !ok {
		t.Error("not a Prewarmer")
	}
	if _, ok := r.(Drainer); !ok {
		t.Error("not a Drainer")
	}
	if _, ok := r.(VMLister); !ok {
		t.Error("not a VMLister")
	}
	_ = r.(interface{ Close() error }).Close()
}
