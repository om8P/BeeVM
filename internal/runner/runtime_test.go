package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/om8P/BeeVM/internal/pool"
	"github.com/om8P/BeeVM/internal/snapshot"
	"github.com/om8P/BeeVM/internal/vmm"
)

// fakeRuntimeSnaps is a Snapshot Manager with runtime and function snapshots in memory.
type fakeRuntimeSnaps struct {
	fakeSnaps
	mu        sync.Mutex
	functions map[int64]snapshot.Snapshot // built function snapshots by function ID
	builds    atomic.Int32
	buildErr  error
	release   chan struct{} // if set, EnsureFunction waits for it
	runtimes  []string      // EnsureRuntime calls
}

func newFakeRuntimeSnaps() *fakeRuntimeSnaps {
	return &fakeRuntimeSnaps{functions: map[int64]snapshot.Snapshot{}}
}

func (f *fakeRuntimeSnaps) EnsureRuntime(ctx context.Context, runtime string, mem, vcpus int64) (snapshot.Snapshot, error) {
	f.mu.Lock()
	f.runtimes = append(f.runtimes, runtime)
	f.mu.Unlock()
	return snapshot.Snapshot{Meta: snapshot.Meta{Key: "rt-" + runtime, Kind: snapshot.KindRuntime}}, nil
}

func (f *fakeRuntimeSnaps) LookupFunction(spec snapshot.FunctionSpec) (snapshot.Snapshot, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.functions[spec.FunctionID]
	return s, ok
}

func (f *fakeRuntimeSnaps) EnsureFunction(ctx context.Context, spec snapshot.FunctionSpec) (snapshot.Snapshot, error) {
	f.builds.Add(1)
	if f.release != nil {
		<-f.release
	}
	if f.buildErr != nil {
		return snapshot.Snapshot{}, f.buildErr
	}
	s := snapshot.Snapshot{Meta: snapshot.Meta{Key: "fn", Kind: snapshot.KindFunction,
		Function: &snapshot.FunctionInputs{FunctionID: spec.FunctionID, Handler: spec.Handler}}}
	f.mu.Lock()
	f.functions[spec.FunctionID] = s
	f.mu.Unlock()
	return s, nil
}

func (f *fakeRuntimeSnaps) RemoveFunction(id int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, had := f.functions[id]
	delete(f.functions, id)
	if had {
		return 1, nil
	}
	return 0, nil
}

// withPython gives r a python image that exists.
func withPython(t *testing.T, set func(runtime, path string)) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "rootfs-python.ext4")
	os.WriteFile(p, []byte("image"), 0o644)
	set("python", p)
}

var pyReq = Request{Runtime: "python", Handler: "handler.handler", CodePath: "/code.zip", TenantID: 1,
	FunctionID: 9, CodeSHA256: "py1", MemoryMiB: 128, VCPUs: 1, TimeoutMS: 1000}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSnapshotRunnerRuntimeThenFunctionSnapshot(t *testing.T) {
	snaps := newFakeRuntimeSnaps()
	r, restores, boots := newFakeSnapshotRunner(snaps)
	withPython(t, r.SetRootfs)
	var used []snapshot.Snapshot
	var usedCfg []vmm.Config
	var mu sync.Mutex
	r.restore = func(ctx context.Context, base vmm.Config, s snapshot.Snapshot, req Request) (Result, error) {
		restores.Add(1)
		mu.Lock()
		used, usedCfg = append(used, s), append(usedCfg, base)
		mu.Unlock()
		return Result{StartKind: StartRestore, Snapshot: snapshotKind(s)}, nil
	}

	// First call: no function snapshot yet -> runtime snapshot, and a background build starts.
	res, err := r.Run(context.Background(), pyReq)
	if err != nil || res.Snapshot != SnapshotRuntime {
		t.Fatalf("first call: %+v %v", res, err)
	}
	if !strings.HasSuffix(usedCfg[0].RootfsPath, "rootfs-python.ext4") || snaps.runtimes[0] != "python" {
		t.Fatalf("python call must use the python image: %+v %v", usedCfg[0], snaps.runtimes)
	}
	waitFor(t, "function snapshot build", func() bool { _, ok := snaps.LookupFunction(specOf(pyReq)); return ok })

	// Second call: restored from the function snapshot, nothing more is built.
	res, err = r.Run(context.Background(), pyReq)
	if err != nil || res.Snapshot != SnapshotFunction || !used[1].IsFunction() {
		t.Fatalf("second call: %+v %v", res, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if snaps.builds.Load() != 1 || boots.Load() != 0 {
		t.Fatalf("builds %d boots %d", snaps.builds.Load(), boots.Load())
	}
}

func TestPrepareBuildsOncePerFunctionAndRemembersFailures(t *testing.T) {
	snaps := newFakeRuntimeSnaps()
	snaps.release = make(chan struct{})
	r, _, _ := newFakeSnapshotRunner(snaps)
	withPython(t, r.SetRootfs)

	// Many calls while the build runs: still one build.
	for i := 0; i < 5; i++ {
		r.Prepare(pyReq)
	}
	close(snaps.release)
	r.builds.Wait()
	if snaps.builds.Load() != 1 {
		t.Fatalf("%d builds, want 1", snaps.builds.Load())
	}
	r.Prepare(pyReq) // exists now: nothing to do
	r.builds.Wait()
	if snaps.builds.Load() != 1 {
		t.Fatal("rebuilt an existing snapshot")
	}

	// A failing build (for example the code does not import) is not retried at once...
	snaps.buildErr = &vmm.LoadError{Msg: "SyntaxError"}
	other := pyReq
	other.FunctionID = 10
	r.Prepare(other)
	r.builds.Wait()
	r.Prepare(other)
	r.builds.Wait()
	if snaps.builds.Load() != 2 {
		t.Fatalf("failed build retried at once: %d builds", snaps.builds.Load())
	}
	// ...but a redeploy forgets the failure (and the old snapshots).
	snaps.buildErr = nil
	r.ForgetFunction(10)
	r.ForgetFunction(9)
	if _, ok := snaps.LookupFunction(specOf(pyReq)); ok {
		t.Fatal("ForgetFunction must remove the snapshot")
	}
	r.Prepare(other)
	r.builds.Wait()
	if snaps.builds.Load() != 3 {
		t.Fatalf("builds %d after forgetting", snaps.builds.Load())
	}

	// After Close nothing new starts.
	r.Close()
	r.Prepare(pyReq)
	r.builds.Wait()
	if snaps.builds.Load() != 3 {
		t.Fatal("built after Close")
	}
}

func TestPrepareNeedsAFunctionAndAnImage(t *testing.T) {
	snaps := newFakeRuntimeSnaps()
	r, _, _ := newFakeSnapshotRunner(snaps)
	r.Prepare(pyReq) // no python image configured
	noID := pyReq
	noID.FunctionID = 0
	withPython(t, r.SetRootfs)
	r.Prepare(noID)
	r.builds.Wait()
	if snaps.builds.Load() != 0 {
		t.Fatalf("builds %d", snaps.builds.Load())
	}
	// A Snapshots without runtime support (the old interface) never builds function snapshots.
	r2, _, _ := newFakeSnapshotRunner(&fakeSnaps{})
	r2.Prepare(pyReq)
	r2.builds.Wait()
}

func TestRuntimeUnavailable(t *testing.T) {
	r, _, _ := newFakeSnapshotRunner(newFakeRuntimeSnaps())
	if _, err := r.Run(context.Background(), pyReq); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("snapshot runner: %v", err)
	}
	c := NewColdRunner(vmm.Config{}, 1, time.Second)
	if _, err := c.Run(context.Background(), pyReq); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("cold runner: %v", err)
	}
	if !c.HasRuntime("binary") || !c.HasRuntime("") || c.HasRuntime("node") {
		t.Fatal("HasRuntime")
	}
	f := newFakeWarm(t, 1, ok)
	if _, err := f.Run(context.Background(), pyReq); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("warm runner: %v", err)
	}
}

func TestWarmRunnerLoadErrorIsAFunctionFailure(t *testing.T) {
	f := newFakeWarm(t, 1, ok)
	withPython(t, f.SetRootfs)
	f.create = func(ctx context.Context, req Request) (*warmVM, error) {
		return nil, &vmm.LoadError{Msg: "Traceback (most recent call last):\n  File \"handler.py\"\nSyntaxError: invalid syntax"}
	}
	res, err := f.Run(context.Background(), pyReq)
	if err != nil || res.FunctionError != "function failed to load: SyntaxError: invalid syntax" ||
		res.ExitCode != 1 || !strings.Contains(res.Stderr, "Traceback") {
		t.Fatalf("got %+v, %v", res, err)
	}
	// The failed create freed its slot: the pool is empty.
	if vms := f.VMs(0); len(vms) != 0 {
		t.Fatalf("VMs left: %+v", vms)
	}
}

func TestWarmRunnerReportsHowTheVMWasMade(t *testing.T) {
	f := newFakeWarm(t, 1, ok)
	withPython(t, f.SetRootfs)
	f.create = func(ctx context.Context, req Request) (*warmVM, error) {
		return &warmVM{id: "vm", exited: make(chan struct{}), destroy: func() {},
			made: Result{StartKind: StartRestore, Snapshot: SnapshotFunction, BootMS: 40, LoadMS: 0}}, nil
	}
	res, _ := f.Run(context.Background(), pyReq)
	if res.Snapshot != SnapshotFunction || res.BootMS != 40 {
		t.Fatalf("first call: %+v", res)
	}
	res, _ = f.Run(context.Background(), pyReq)
	if res.StartKind != StartWarm || res.Snapshot != "" {
		t.Fatalf("second call: %+v", res)
	}
}

func TestRuntimeExitMakesTheVMUnhealthy(t *testing.T) {
	ctx := context.Background()
	if healthyAfter(ctx, nil, Result{FunctionError: "runtime exited: exit status 3"}) {
		t.Fatal("a VM whose runtime died must not be reused")
	}
	if !healthyAfter(ctx, nil, Result{FunctionError: "function raised an error: ValueError: x"}) {
		t.Fatal("a handler that raised leaves a fine VM")
	}
}

func TestPoolKeySeparatesHandlers(t *testing.T) {
	a := keyOf(pyReq)
	b := pyReq
	b.Handler = "handler.other"
	if a == keyOf(b) {
		t.Fatal("different handlers need different VMs")
	}
	if keyOf(Request{}).Runtime != "binary" {
		t.Fatal("empty runtime is binary")
	}
	var _ pool.Key = a
}

func TestLoadFailureKeepsOtherErrors(t *testing.T) {
	var res Result
	boom := errors.New("vsock broke")
	if err := loadFailure(boom, pyReq, &res); err != boom || res.FunctionError != "" {
		t.Fatalf("other errors must stay errors: %v %+v", err, res)
	}
	node := pyReq
	node.Runtime = "node"
	if err := loadFailure(&vmm.LoadError{Msg: "Error: boom\n    at x"}, node, &res); err != nil || res.FunctionError != "function failed to load: Error: boom" {
		t.Fatalf("node: %v %+v", err, res)
	}
}
