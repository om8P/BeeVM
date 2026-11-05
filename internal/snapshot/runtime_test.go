package snapshot

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordJobs wraps the test manager's fake build and remembers every job.
func recordJobs(m *Manager) func() []buildJob {
	var mu sync.Mutex
	var jobs []buildJob
	inner := m.build
	m.build = func(ctx context.Context, job buildJob, dir string) error {
		mu.Lock()
		jobs = append(jobs, job)
		mu.Unlock()
		return inner(ctx, job, dir)
	}
	return func() []buildJob {
		mu.Lock()
		defer mu.Unlock()
		return append([]buildJob(nil), jobs...)
	}
}

// Snapshots built before runtimes existed must keep their key: adding the Runtime field
// (omitted when empty) must not change the bytes that are hashed.
func TestBinaryKeyUnchanged(t *testing.T) {
	in := Inputs{FirecrackerVersion: "v1", KernelSHA256: "k", RootfsSHA256: "r", RootfsPath: "/r",
		BootArgs: "a", MemMiB: 128, VCPUs: 1, BuildVersion: 2}
	if got := Key(in); got != "07d8e5815f052afe15fc" { // the M6/M7 code's key for these inputs
		t.Fatalf("binary key changed: %s", got)
	}
	in.Runtime = "python"
	if Key(in) == "07d8e5815f052afe15fc" {
		t.Fatal("the runtime must be part of the key")
	}
}

func TestRuntimeSnapshotsUseTheirImage(t *testing.T) {
	m, builds := newTestManager(t)
	jobs := recordJobs(m)
	ctx := context.Background()
	bin, err := m.Ensure(ctx, 128, 1)
	if err != nil {
		t.Fatal(err)
	}
	py, err := m.EnsureRuntime(ctx, "python", 128, 1)
	if err != nil {
		t.Fatal(err)
	}
	if py.Key == bin.Key || builds.Load() != 2 {
		t.Fatalf("python snapshot: key %s, builds %d", py.Key, builds.Load())
	}
	if py.Inputs.Runtime != "python" || py.RuntimeName() != "python" || bin.RuntimeName() != "binary" || py.Kind != KindRuntime {
		t.Fatalf("meta: %+v / %+v", py.Meta, bin.Meta)
	}
	j := jobs()
	if j[1].runtime != "python" || !strings.HasSuffix(j[1].cfg.RootfsPath, "rootfs-python.ext4") || j[1].fn != nil {
		t.Fatalf("python build job: %+v", j[1])
	}
	if j[0].runtime != "" || !strings.HasSuffix(j[0].cfg.RootfsPath, "rootfs.ext4") {
		t.Fatalf("binary build job: %+v", j[0])
	}

	// Both are current: the stale sweep at server start must keep them.
	n, err := m.RemoveStale()
	if err != nil || n != 0 {
		t.Fatalf("RemoveStale removed %d (%v)", n, err)
	}
	list, _ := m.List()
	if len(list) != 2 || !list[0].Current || !list[1].Current {
		t.Fatalf("list: %+v", list)
	}
}

func TestUnknownOrMissingRuntime(t *testing.T) {
	m, _ := newTestManager(t)
	if _, err := m.EnsureRuntime(context.Background(), "node", 128, 1); err == nil {
		t.Fatal("node has no image configured")
	}
	if m.HasRuntime("node") || !m.HasRuntime("python") || !m.HasRuntime("") {
		t.Fatal("HasRuntime")
	}
	m.SetRootfs("node", "/does/not/exist.ext4")
	if m.HasRuntime("node") {
		t.Fatal("a missing image is not available")
	}
}

func pySpec(fnID int64, sha string) FunctionSpec {
	return FunctionSpec{Runtime: "python", Handler: "handler.handler", TenantID: 1, FunctionID: fnID,
		CodeSHA256: sha, CodePath: "/code/" + sha, MemMiB: 128, VCPUs: 1}
}

func TestFunctionSnapshot(t *testing.T) {
	m, builds := newTestManager(t)
	jobs := recordJobs(m)
	ctx := context.Background()
	spec := pySpec(7, "aaa")
	if _, ok := m.LookupFunction(spec); ok {
		t.Fatal("nothing built yet")
	}
	fs, err := m.EnsureFunction(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	// The runtime snapshot is built first (the parent), then the function's.
	if builds.Load() != 2 {
		t.Fatalf("builds %d", builds.Load())
	}
	j := jobs()
	parent := j[1].parent
	if j[0].fn != nil || j[1].fn == nil || j[1].fn.CodePath != "/code/aaa" || parent.Kind != KindRuntime || parent.Inputs.Runtime != "python" {
		t.Fatalf("jobs: %+v", j)
	}
	if !fs.IsFunction() || fs.Function.ParentKey != parent.Key || fs.Function.FunctionID != 7 ||
		fs.Function.Handler != "handler.handler" || !strings.HasPrefix(fs.Key, "fn-") {
		t.Fatalf("function meta: %+v %+v", fs.Meta, fs.Function)
	}
	got, ok := m.LookupFunction(spec)
	if !ok || got.Key != fs.Key {
		t.Fatal("lookup after build")
	}
	if _, err := m.EnsureFunction(ctx, spec); err != nil || builds.Load() != 2 {
		t.Fatalf("second ensure rebuilt (builds %d, %v)", builds.Load(), err)
	}

	// Another function with the same code, another tenant, other code or another handler:
	// each is its own snapshot.
	others := []FunctionSpec{pySpec(8, "aaa"), pySpec(7, "bbb")}
	other := pySpec(7, "aaa")
	other.TenantID = 2
	others = append(others, other)
	other = pySpec(7, "aaa")
	other.Handler = "handler.other"
	others = append(others, other)
	for _, o := range others {
		if _, ok := m.LookupFunction(o); ok {
			t.Fatalf("%+v must not find function 7's snapshot", o)
		}
	}
}

func TestBinaryFunctionSnapshotIgnoresHandler(t *testing.T) {
	m, _ := newTestManager(t)
	spec := FunctionSpec{TenantID: 1, FunctionID: 1, CodeSHA256: "abc", CodePath: "/x", MemMiB: 128, VCPUs: 1}
	fs, err := m.EnsureFunction(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if fs.RuntimeName() != "binary" || fs.Function.Handler != "" {
		t.Fatalf("binary function snapshot: %+v", fs.Meta)
	}
	spec.Handler = "ignored"
	if _, ok := m.LookupFunction(spec); !ok {
		t.Fatal("a binary's handler is meaningless and must not change the key")
	}
}

func TestFunctionSnapshotGoesStaleWithItsRuntime(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := context.Background()
	fs, _ := m.EnsureFunction(ctx, pySpec(1, "aaa"))
	bin, _ := m.Ensure(ctx, 128, 1)

	// The python image is rebuilt: the python runtime snapshot and the function snapshot on
	// top of it are stale; the binary one is not.
	later := time.Now().Add(time.Minute)
	py := m.rootfs["python"]
	_ = os.WriteFile(py, []byte("new python image"), 0o644)
	_ = os.Chtimes(py, later, later)

	if _, ok := m.LookupFunction(pySpec(1, "aaa")); ok {
		t.Fatal("lookup must not return a snapshot of the old image")
	}
	list, _ := m.List()
	for _, s := range list {
		if s.Current != (s.Key == bin.Key) {
			t.Errorf("%s (%s) current=%v", s.Key, s.Kind, s.Current)
		}
	}
	n, _ := m.RemoveStale()
	if n != 2 {
		t.Fatalf("RemoveStale removed %d, want 2", n)
	}
	if _, err := os.Stat(fs.Dir); !os.IsNotExist(err) {
		t.Fatal("stale function snapshot still on disk")
	}
}

func TestRemoveFunctionAndOrphans(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := context.Background()
	a, _ := m.EnsureFunction(ctx, pySpec(1, "aaa"))
	b, _ := m.EnsureFunction(ctx, pySpec(2, "bbb"))
	c, _ := m.EnsureFunction(ctx, pySpec(3, "ccc"))
	rt, _ := m.EnsureRuntime(ctx, "python", 128, 1)

	if n, err := m.RemoveFunction(1); err != nil || n != 1 {
		t.Fatalf("RemoveFunction: %d %v", n, err)
	}
	// Function 2 still exists with its code; function 3 was redeployed with other code.
	n, err := m.RemoveOrphans(func(id int64, sha string) bool { return id == 2 && sha == "bbb" || id == 3 && sha == "new" })
	if err != nil || n != 1 {
		t.Fatalf("RemoveOrphans: %d %v", n, err)
	}
	for dir, want := range map[string]bool{a.Dir: false, b.Dir: true, c.Dir: false, rt.Dir: true} {
		_, err := os.Stat(dir)
		if (err == nil) != want {
			t.Errorf("%s exists=%v, want %v", dir, err == nil, want)
		}
	}
}

func TestOneFunctionBuildAtATime(t *testing.T) {
	m, _ := newTestManager(t)
	var running, most int32
	var mu sync.Mutex
	inner := m.build
	m.build = func(ctx context.Context, job buildJob, dir string) error {
		if job.fn != nil {
			mu.Lock()
			running++
			if running > most {
				most = running
			}
			mu.Unlock()
			defer func() { mu.Lock(); running--; mu.Unlock() }()
		}
		return inner(ctx, job, dir)
	}
	var wg sync.WaitGroup
	for i := int64(1); i <= 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.EnsureFunction(context.Background(), pySpec(i, "x")); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if most != 1 {
		t.Fatalf("%d function builds ran at once, want 1", most)
	}
}
