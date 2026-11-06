// Package snapshot is BeeVM's Snapshot Manager. A snapshot is a running VM frozen to two files
// (memory + CPU/device state); restoring one takes tens of milliseconds instead of a full boot.
//
// There are two tiers:
//   - A runtime snapshot is a VM of one runtime (binary, python, node) that has booted,
//     started its guest agent (and, for python/node, the interpreter) and settled. One per
//     runtime, memory size and vCPU count.
//   - A function snapshot starts from a runtime snapshot, then the function's code is
//     uploaded and imported before the VM is frozen again. A VM restored from it can answer
//     the first call at once: no upload, no import.
//
// A snapshot only fits the exact kernel, root filesystem, Firecracker version, memory size
// and vCPU count it was taken with. All of these go into its key (a hash), so rebuilding the
// images automatically leads to a new snapshot instead of restoring a wrong one. A function
// snapshot's key contains its runtime snapshot's key (its "parent"), so it goes stale with it.
//
// Layout on disk:  <dir>/<key>/{vmstate, mem, meta.json}   (function snapshots: <dir>/fn-<key>/)
// A snapshot is built in <dir>/tmp-<random>/ and renamed into place when complete, so a
// half-written snapshot is never used.
package snapshot

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/om8P/BeeVM/internal/agentproto"
	"github.com/om8P/BeeVM/internal/pkg"
	"github.com/om8P/BeeVM/internal/vmm"
)

// Inputs is everything a runtime snapshot depends on.
type Inputs struct {
	FirecrackerVersion string `json:"firecracker_version"`
	KernelSHA256       string `json:"kernel_sha256"`
	RootfsSHA256       string `json:"rootfs_sha256"`
	RootfsPath         string `json:"rootfs_path"` // stored inside the snapshot, so part of the key
	BootArgs           string `json:"boot_args"`
	MemMiB             int64  `json:"mem_mib"`
	VCPUs              int64  `json:"vcpus"`
	BuildVersion       int    `json:"build_version"` // how the snapshot is made (see BuildVersion)
	// Runtime is empty for binaries, so binary keys are the same as before runtimes existed.
	Runtime string `json:"runtime,omitempty"`
	// Isolation is the isolator's name ("" without isolation, so those keys stay unchanged).
	// A snapshot taken in a jail names its files as the jail sees them, and one taken with a
	// network card needs one when restored: it only fits VMs isolated the same way.
	Isolation string `json:"isolation,omitempty"`
}

// BuildVersion numbers the way buildVM prepares a VM before snapshotting it. Bump it when that
// changes: older snapshots then no longer match and are rebuilt (and removed as stale).
//
//	1  boot, wait for the agent, settle, snapshot (M6)
//	2  also call /resume once before settling, so that code path is in guest memory (M6 refine)
//	   (M8: python/node runtime snapshots also start the interpreter; binary unchanged)
const BuildVersion = 2

// FunctionBuildVersion numbers the way function snapshots are made (see buildFunctionVM).
//
//	1  restore the runtime snapshot, upload, import, settle, snapshot (M8)
const FunctionBuildVersion = 1

// Key is a short, stable name for a set of inputs.
func Key(in Inputs) string {
	b, _ := json.Marshal(in) // field order is fixed, so the bytes are stable
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:20]
}

// FunctionInputs is what a function snapshot depends on, on top of its parent runtime
// snapshot. The tenant and function are part of it on purpose: two functions never share
// a snapshot, even with identical code, just as they never share a VM.
type FunctionInputs struct {
	ParentKey    string `json:"parent_key"`
	TenantID     int64  `json:"tenant_id"`
	FunctionID   int64  `json:"function_id"`
	CodeSHA256   string `json:"code_sha256"`
	Handler      string `json:"handler,omitempty"`
	BuildVersion int    `json:"build_version"`
}

// FunctionKey is the key (and folder name) of a function snapshot.
func FunctionKey(fi FunctionInputs) string {
	b, _ := json.Marshal(fi)
	sum := sha256.Sum256(b)
	return "fn-" + hex.EncodeToString(sum[:])[:20]
}

// Snapshot kinds, as stored in Meta.Kind.
const (
	KindRuntime  = "runtime"
	KindFunction = "function"
)

// Meta is stored next to a snapshot as meta.json.
type Meta struct {
	Key       string          `json:"key"`
	Kind      string          `json:"kind,omitempty"` // KindRuntime ("" in snapshots from before M8) or KindFunction
	Inputs    Inputs          `json:"inputs"`         // function snapshots: the parent's inputs
	Function  *FunctionInputs `json:"function,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	BuildMS   int64           `json:"build_ms"`
	MemBytes  int64           `json:"mem_bytes"`
}

// RuntimeName is the snapshot's runtime ("binary" when Inputs.Runtime is empty).
func (m Meta) RuntimeName() string {
	if m.Inputs.Runtime == "" {
		return agentproto.RuntimeBinary
	}
	return m.Inputs.Runtime
}

// IsFunction reports whether this is a function snapshot.
func (m Meta) IsFunction() bool { return m.Kind == KindFunction }

// Snapshot is a finished snapshot on disk.
type Snapshot struct {
	Meta
	Dir   string
	Files vmm.SnapshotFiles
}

// Info is a snapshot as listed, with whether it still matches the current images.
type Info struct {
	Snapshot
	Current bool
}

// FunctionSpec describes the function a function snapshot is built for.
type FunctionSpec struct {
	Runtime    string // "binary" (also when empty), "python" or "node"
	Handler    string // python/node
	TenantID   int64
	FunctionID int64
	CodeSHA256 string
	CodePath   string // the binary or zip package on the host (only read while building)
	MemMiB     int64
	VCPUs      int64
}

func (s FunctionSpec) runtime() string {
	if s.Runtime == "" {
		return agentproto.RuntimeBinary
	}
	return s.Runtime
}

// buildJob is one snapshot to make: a runtime snapshot (fn == nil), or a function snapshot
// that starts from parent.
type buildJob struct {
	cfg     vmm.Config
	runtime string
	fn      *FunctionSpec
	parent  Snapshot
}

// buildFunc creates the snapshot files (vmstate, mem) in dir.
type buildFunc func(ctx context.Context, job buildJob, dir string) error

// Manager finds, builds and removes snapshots.
type Manager struct {
	dir    string
	base   vmm.Config // FirecrackerBin, KernelPath, RootfsPath (binary); memory and vCPUs vary
	settle time.Duration
	logf   func(format string, args ...any)

	build     buildFunc              // replaced in unit tests
	fcVersion func() (string, error) // replaced in unit tests
	group     singleflight.Group     // one build per key at a time
	fnBuilds  chan struct{}          // limits function snapshot builds running at once
	mu        sync.Mutex             // guards hashes, version and rootfs
	hashes    map[string]fileHash    // file path -> hash, reused while the file is unchanged
	version   string
	rootfs    map[string]string // runtime -> root filesystem image
}

type fileHash struct {
	size  int64
	mtime time.Time
	sum   string
}

// BootArgs are the extra kernel arguments of a snapshot VM: start the guest agent.
const BootArgs = "beevm.mode=agent"

// DefaultSettle is how long a freshly booted VM runs before it is snapshotted. Snapshotting
// right after the agent starts makes the first reply after every restore slow (~850 ms
// measured); 0.5 s or more avoids it, and 1 s leaves a margin.
const DefaultSettle = time.Second

// FunctionSettle is how long a VM runs after importing a function before it is snapshotted.
// The VM it starts from has already settled once (it is a restored runtime snapshot).
const FunctionSettle = 200 * time.Millisecond

// New creates a Manager that keeps snapshots in dir. base needs FirecrackerBin, KernelPath and
// RootfsPath (the binary runtime's image; add others with SetRootfs); logf may be nil.
func New(dir string, base vmm.Config, logf func(string, ...any)) (*Manager, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create snapshot folder: %w", err)
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	m := &Manager{dir: dir, base: base, settle: DefaultSettle, logf: logf, hashes: map[string]fileHash{},
		fnBuilds: make(chan struct{}, 1), rootfs: map[string]string{agentproto.RuntimeBinary: base.RootfsPath}}
	m.build = m.buildVM
	m.fcVersion = func() (string, error) {
		out, err := exec.Command(base.FirecrackerBin, "--version").Output()
		if err != nil {
			return "", fmt.Errorf("firecracker --version: %w", err)
		}
		line, _, _ := strings.Cut(string(out), "\n")
		return strings.TrimSpace(line), nil
	}
	return m, nil
}

// SetSettle changes how long a VM runs before it is snapshotted (for experiments; the
// default is DefaultSettle). The settle time is not part of the key, so use a separate
// folder for snapshots built with a different value.
func (m *Manager) SetSettle(d time.Duration) { m.settle = d }

// SetRootfs sets the root filesystem image of a runtime ("python", "node"; "binary" is the
// base config's RootfsPath).
func (m *Manager) SetRootfs(runtime, path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rootfs[runtime] = path
}

// HasRuntime reports whether the runtime's image exists.
func (m *Manager) HasRuntime(runtime string) bool {
	if runtime == "" {
		runtime = agentproto.RuntimeBinary
	}
	m.mu.Lock()
	path := m.rootfs[runtime]
	m.mu.Unlock()
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// Dir returns the folder that holds the snapshots.
func (m *Manager) Dir() string { return m.dir }

// Inputs returns what a binary runtime snapshot for this memory size and vCPU count depends on
// right now.
func (m *Manager) Inputs(memMiB, vcpus int64) (Inputs, error) {
	return m.RuntimeInputs(agentproto.RuntimeBinary, memMiB, vcpus)
}

// RuntimeInputs returns what a runtime snapshot depends on right now.
func (m *Manager) RuntimeInputs(runtime string, memMiB, vcpus int64) (Inputs, error) {
	return m.runtimeInputs(runtime, memMiB, vcpus, m.isolation())
}

// isolation is the name of the base config's isolator ("" for none).
func (m *Manager) isolation() string {
	if m.base.Isolator == nil {
		return ""
	}
	return m.base.Isolator.Name()
}

// runtimeInputs is RuntimeInputs for a given isolation (List judges every snapshot by its own).
func (m *Manager) runtimeInputs(runtime string, memMiB, vcpus int64, isolation string) (Inputs, error) {
	if runtime == "" {
		runtime = agentproto.RuntimeBinary
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rootfsPath, ok := m.rootfs[runtime]
	if !ok {
		return Inputs{}, fmt.Errorf("no image configured for runtime %q", runtime)
	}
	if m.version == "" {
		v, err := m.fcVersion()
		if err != nil {
			return Inputs{}, err
		}
		m.version = v
	}
	kernel, err := m.hashFile(m.base.KernelPath)
	if err != nil {
		return Inputs{}, err
	}
	rootfs, err := m.hashFile(rootfsPath)
	if err != nil {
		return Inputs{}, err
	}
	in := Inputs{
		FirecrackerVersion: m.version, KernelSHA256: kernel, RootfsSHA256: rootfs,
		RootfsPath: rootfsPath, BootArgs: BootArgs, MemMiB: memMiB, VCPUs: vcpus,
		BuildVersion: BuildVersion, Isolation: isolation,
	}
	if runtime != agentproto.RuntimeBinary {
		in.Runtime = runtime
	}
	return in, nil
}

// hashFile returns the file's SHA-256, re-reading it only when its size or time changed.
// Callers hold m.mu.
func (m *Manager) hashFile(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if h, ok := m.hashes[path]; ok && h.size == st.Size() && h.mtime.Equal(st.ModTime()) {
		return h.sum, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	h := fileHash{size: st.Size(), mtime: st.ModTime(), sum: hex.EncodeToString(sum.Sum(nil))}
	m.hashes[path] = h
	return h.sum, nil
}

// Ensure returns the binary runtime snapshot for this memory size and vCPU count, building it
// first if it does not exist yet. Parallel calls for the same snapshot share one build.
func (m *Manager) Ensure(ctx context.Context, memMiB, vcpus int64) (Snapshot, error) {
	return m.EnsureRuntime(ctx, agentproto.RuntimeBinary, memMiB, vcpus)
}

// EnsureRuntime returns the runtime snapshot for this runtime, memory size and vCPU count,
// building it first if it does not exist yet.
func (m *Manager) EnsureRuntime(ctx context.Context, runtime string, memMiB, vcpus int64) (Snapshot, error) {
	in, err := m.RuntimeInputs(runtime, memMiB, vcpus)
	if err != nil {
		return Snapshot{}, err
	}
	key := Key(in)
	if s, err := m.load(key); err == nil {
		return s, nil
	}
	cfg := m.vmConfig(in)
	return m.shared(ctx, key, func(bctx context.Context) (Snapshot, error) {
		meta := Meta{Key: key, Kind: KindRuntime, Inputs: in}
		return m.create(bctx, meta, buildJob{cfg: cfg, runtime: in.Runtime})
	})
}

// shared runs a build once per key, for all callers at the same time. The build must not stop
// when the first caller goes away, so it gets its own context.
func (m *Manager) shared(ctx context.Context, key string, fn func(context.Context) (Snapshot, error)) (Snapshot, error) {
	v, err, _ := m.group.Do(key, func() (any, error) {
		bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		return fn(bctx)
	})
	if err != nil {
		return Snapshot{}, err
	}
	return v.(Snapshot), nil
}

// vmConfig is the config of the VM a snapshot with these inputs is built from.
func (m *Manager) vmConfig(in Inputs) vmm.Config {
	cfg := m.base
	cfg.ID = ""
	cfg.BaseDir = filepath.Join(m.dir, "build")
	cfg.RootfsPath = in.RootfsPath
	cfg.SharedRootfs = true
	cfg.ExtraBootArgs = BootArgs
	cfg.MemMiB, cfg.VCPUs = in.MemMiB, in.VCPUs
	return cfg
}

// FunctionInputs returns the inputs of spec's function snapshot right now (its parent is the
// current runtime snapshot of the same runtime and size).
func (m *Manager) FunctionInputs(spec FunctionSpec) (Inputs, FunctionInputs, error) {
	in, err := m.RuntimeInputs(spec.runtime(), spec.MemMiB, spec.VCPUs)
	if err != nil {
		return Inputs{}, FunctionInputs{}, err
	}
	fi := FunctionInputs{ParentKey: Key(in), TenantID: spec.TenantID, FunctionID: spec.FunctionID,
		CodeSHA256: spec.CodeSHA256, BuildVersion: FunctionBuildVersion}
	if pkg.IsPackaged(spec.runtime()) {
		fi.Handler = spec.Handler
	}
	return in, fi, nil
}

// LookupFunction returns spec's function snapshot if it exists. It never builds.
func (m *Manager) LookupFunction(spec FunctionSpec) (Snapshot, bool) {
	_, fi, err := m.FunctionInputs(spec)
	if err != nil {
		return Snapshot{}, false
	}
	s, err := m.load(FunctionKey(fi))
	return s, err == nil
}

// EnsureFunction returns spec's function snapshot, building it (and its runtime snapshot)
// first if needed. Only one function snapshot is built at a time on this node; others wait.
// A failing import of the user's code is returned as a *vmm.LoadError.
func (m *Manager) EnsureFunction(ctx context.Context, spec FunctionSpec) (Snapshot, error) {
	in, fi, err := m.FunctionInputs(spec)
	if err != nil {
		return Snapshot{}, err
	}
	key := FunctionKey(fi)
	if s, err := m.load(key); err == nil {
		return s, nil
	}
	return m.shared(ctx, key, func(bctx context.Context) (Snapshot, error) {
		select {
		case m.fnBuilds <- struct{}{}:
			defer func() { <-m.fnBuilds }()
		case <-bctx.Done():
			return Snapshot{}, bctx.Err()
		}
		parent, err := m.EnsureRuntime(bctx, spec.runtime(), spec.MemMiB, spec.VCPUs)
		if err != nil {
			return Snapshot{}, fmt.Errorf("runtime snapshot: %w", err)
		}
		if parent.Key != fi.ParentKey {
			return Snapshot{}, errors.New("the runtime image changed while building; try again")
		}
		meta := Meta{Key: key, Kind: KindFunction, Inputs: in, Function: &fi}
		return m.create(bctx, meta, buildJob{cfg: m.vmConfig(in), runtime: spec.runtime(), fn: &spec, parent: parent})
	})
}

// load reads a finished snapshot.
func (m *Manager) load(key string) (Snapshot, error) {
	dir := filepath.Join(m.dir, key)
	data, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return Snapshot{}, err
	}
	var meta Meta
	if err := json.Unmarshal(data, &meta); err != nil {
		return Snapshot{}, fmt.Errorf("snapshot %s: bad meta.json: %w", key, err)
	}
	if meta.Kind == "" {
		meta.Kind = KindRuntime // written before M8
	}
	files := vmm.SnapshotFiles{StatePath: filepath.Join(dir, "vmstate"), MemPath: filepath.Join(dir, "mem")}
	for _, p := range []string{files.StatePath, files.MemPath} {
		if _, err := os.Stat(p); err != nil {
			return Snapshot{}, fmt.Errorf("snapshot %s is incomplete: %w", key, err)
		}
	}
	return Snapshot{Meta: meta, Dir: dir, Files: files}, nil
}

// create builds a snapshot in a temporary folder and renames it into place.
func (m *Manager) create(ctx context.Context, meta Meta, job buildJob) (Snapshot, error) {
	key := meta.Key
	if s, err := m.load(key); err == nil {
		return s, nil // finished just before we got here
	}
	tmp := filepath.Join(m.dir, "tmp-"+randomHex(4))
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return Snapshot{}, err
	}
	defer os.RemoveAll(tmp) // gone already if the rename worked

	what := meta.RuntimeName() + " runtime"
	if job.fn != nil {
		what = fmt.Sprintf("%s function %d", meta.RuntimeName(), job.fn.FunctionID)
	}
	m.logf("building snapshot %s (%s, %d MiB, %d vCPU)", key, what, job.cfg.MemMiB, job.cfg.VCPUs)
	t0 := time.Now()
	if err := m.build(ctx, job, tmp); err != nil {
		return Snapshot{}, fmt.Errorf("build snapshot: %w", err)
	}
	meta.CreatedAt = time.Now().UTC()
	meta.BuildMS = time.Since(t0).Milliseconds()
	if st, err := os.Stat(filepath.Join(tmp, "mem")); err == nil {
		meta.MemBytes = st.Size()
	}
	data, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(filepath.Join(tmp, "meta.json"), data, 0o644); err != nil {
		return Snapshot{}, err
	}
	if err := os.Rename(tmp, filepath.Join(m.dir, key)); err != nil {
		if s, lerr := m.load(key); lerr == nil {
			return s, nil // another process finished the same snapshot first
		}
		return Snapshot{}, fmt.Errorf("move snapshot into place: %w", err)
	}
	m.logf("snapshot %s ready in %d ms", key, meta.BuildMS)
	return m.load(key)
}

// buildVM makes a snapshot's VM and snapshots it into dir.
func (m *Manager) buildVM(ctx context.Context, job buildJob, dir string) error {
	if job.fn != nil {
		return m.buildFunctionVM(ctx, job, dir)
	}
	vm, err := vmm.New(job.cfg)
	if err != nil {
		return err
	}
	defer vm.Cleanup(false)
	if err := vm.Start(ctx); err != nil {
		return err
	}
	if _, err := vm.WaitForAgent(ctx); err != nil {
		return err
	}
	// Make the call every restore starts with once now. Its code is then already read from
	// the disk image into guest memory, so a restored VM does not have to read it first.
	if _, err := vm.Agent().Resume(ctx); err != nil {
		return fmt.Errorf("warm up agent: %w", err)
	}
	// Python/Node.js: start the interpreter (no code yet). Restored VMs then skip its start-up.
	if pkg.IsPackaged(job.runtime) {
		if _, err := vm.Agent().Load(ctx, job.runtime, "", 0); err != nil {
			return fmt.Errorf("start %s runtime: %w", job.runtime, err)
		}
	}
	return m.freeze(ctx, vm, m.settle, dir)
}

// buildFunctionVM restores the runtime snapshot, puts the function's code in and imports it,
// then snapshots the VM again.
func (m *Manager) buildFunctionVM(ctx context.Context, job buildJob, dir string) error {
	vm, _, err := vmm.Restore(ctx, job.cfg, job.parent.Files)
	if err != nil {
		return fmt.Errorf("restore runtime snapshot: %w", err)
	}
	defer vm.Cleanup(false)
	if _, err := vm.WaitAndResume(ctx); err != nil {
		return err
	}
	if pkg.IsPackaged(job.runtime) {
		if _, err := vm.Agent().UploadPackageFile(ctx, job.fn.CodePath); err != nil {
			return fmt.Errorf("upload package: %w", err)
		}
		if _, err := vm.Agent().Load(ctx, job.runtime, job.fn.Handler, 0); err != nil {
			return err // a *vmm.LoadError when the user's code does not import
		}
	} else if _, err := vm.Agent().UploadFunctionFile(ctx, job.fn.CodePath); err != nil {
		return fmt.Errorf("upload function: %w", err)
	}
	return m.freeze(ctx, vm, FunctionSettle, dir)
}

// freeze lets the VM settle, then pauses it and writes its snapshot into dir.
func (m *Manager) freeze(ctx context.Context, vm *vmm.VM, settle time.Duration, dir string) error {
	select {
	case <-time.After(settle):
	case <-ctx.Done():
		return ctx.Err()
	}
	vm.Agent().Close() // host connections do not survive a snapshot; drop them first
	if err := vm.Pause(ctx); err != nil {
		return fmt.Errorf("pause: %w", err)
	}
	if err := vm.CreateSnapshot(ctx, filepath.Join(dir, "mem"), filepath.Join(dir, "vmstate")); err != nil {
		return fmt.Errorf("create snapshot: %w", err)
	}
	return nil
}

// List returns all finished snapshots, newest first, marking those that match the current
// images. Unfinished build folders are not listed.
//
// A runtime snapshot is current when it matches the current inputs of its own runtime; a
// function snapshot when its parent is the current runtime snapshot (and it was built the
// current way). Whether its function still exists is not known here: see RemoveOrphans.
// Each snapshot is judged by its own isolation, so a server with --isolation jailer does not
// delete the snapshots of a server without it (and the other way round).
func (m *Manager) List() ([]Info, error) {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "build" || strings.HasPrefix(e.Name(), "tmp-") {
			continue
		}
		s, err := m.load(e.Name())
		if err != nil {
			continue
		}
		info := Info{Snapshot: s}
		if in, err := m.runtimeInputs(s.RuntimeName(), s.Inputs.MemMiB, s.Inputs.VCPUs, s.Inputs.Isolation); err == nil {
			if s.IsFunction() {
				info.Current = s.Function != nil && s.Function.ParentKey == Key(in) &&
					s.Function.BuildVersion == FunctionBuildVersion
			} else {
				info.Current = Key(in) == s.Key
			}
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// ErrNotFound is returned by Remove for an unknown key.
var ErrNotFound = errors.New("no such snapshot")

// Remove deletes one snapshot.
func (m *Manager) Remove(key string) error {
	if key == "" || strings.ContainsAny(key, `/\.`) {
		return fmt.Errorf("bad snapshot key %q", key)
	}
	dir := filepath.Join(m.dir, key)
	if _, err := os.Stat(dir); err != nil {
		return ErrNotFound
	}
	return os.RemoveAll(dir)
}

// RemoveFunction deletes every snapshot of one function (after a redeploy or delete) and
// returns how many it removed.
func (m *Manager) RemoveFunction(functionID int64) (int, error) {
	return m.removeFunctions(func(fi FunctionInputs) bool { return fi.FunctionID == functionID })
}

// RemoveOrphans deletes function snapshots whose function is gone or has other code now.
// keep reports whether the function still exists with this code.
func (m *Manager) RemoveOrphans(keep func(functionID int64, codeSHA256 string) bool) (int, error) {
	return m.removeFunctions(func(fi FunctionInputs) bool { return !keep(fi.FunctionID, fi.CodeSHA256) })
}

func (m *Manager) removeFunctions(match func(FunctionInputs) bool) (int, error) {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "fn-") {
			continue
		}
		s, err := m.load(e.Name())
		if err != nil || s.Function == nil || !match(*s.Function) {
			continue
		}
		if err := os.RemoveAll(s.Dir); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// RemoveStale deletes snapshots that no longer match the current images, and leftovers of
// interrupted builds. It returns how many snapshots it removed.
func (m *Manager) RemoveStale() (int, error) {
	list, err := m.List()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, s := range list {
		if !s.Current {
			if err := os.RemoveAll(s.Dir); err != nil {
				return n, err
			}
			n++
		}
	}
	// Leftovers of builds that were interrupted. A build in progress (perhaps by another
	// process) is younger than this, so it is left alone.
	cutoff := time.Now().Add(-10 * time.Minute)
	for _, parent := range []string{m.dir, filepath.Join(m.dir, "build")} {
		entries, _ := os.ReadDir(parent)
		for _, e := range entries {
			if parent == m.dir && !strings.HasPrefix(e.Name(), "tmp-") {
				continue
			}
			if fi, err := e.Info(); err == nil && fi.ModTime().Before(cutoff) {
				_ = os.RemoveAll(filepath.Join(parent, e.Name()))
			}
		}
	}
	return n, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
