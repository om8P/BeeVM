// Package snapshot is BeeVM's Snapshot Manager. It builds "base snapshots": a VM that has
// booted, started its guest agent and settled, frozen to two files (memory + CPU/device
// state). Restoring one takes tens of milliseconds instead of a full boot.
//
// A snapshot only fits the exact kernel, root filesystem, Firecracker version, memory size
// and vCPU count it was taken with. All of these go into its key (a hash), so rebuilding the
// images automatically leads to a new snapshot instead of restoring a wrong one.
//
// Layout on disk:  <dir>/<key>/{vmstate, mem, meta.json}
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

	"github.com/om8P/BeeVM/internal/vmm"
)

// Inputs is everything a snapshot depends on.
type Inputs struct {
	FirecrackerVersion string `json:"firecracker_version"`
	KernelSHA256       string `json:"kernel_sha256"`
	RootfsSHA256       string `json:"rootfs_sha256"`
	RootfsPath         string `json:"rootfs_path"` // stored inside the snapshot, so part of the key
	BootArgs           string `json:"boot_args"`
	MemMiB             int64  `json:"mem_mib"`
	VCPUs              int64  `json:"vcpus"`
}

// Key is a short, stable name for a set of inputs.
func Key(in Inputs) string {
	b, _ := json.Marshal(in) // field order is fixed, so the bytes are stable
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:20]
}

// Meta is stored next to a snapshot as meta.json.
type Meta struct {
	Key       string    `json:"key"`
	Inputs    Inputs    `json:"inputs"`
	CreatedAt time.Time `json:"created_at"`
	BuildMS   int64     `json:"build_ms"`
	MemBytes  int64     `json:"mem_bytes"`
}

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

// buildFunc creates the snapshot files (vmstate, mem) in dir for a VM described by cfg.
type buildFunc func(ctx context.Context, cfg vmm.Config, dir string) error

// Manager finds, builds and removes snapshots.
type Manager struct {
	dir    string
	base   vmm.Config // FirecrackerBin, KernelPath, RootfsPath; memory and vCPUs vary
	settle time.Duration
	logf   func(format string, args ...any)

	build     buildFunc              // replaced in unit tests
	fcVersion func() (string, error) // replaced in unit tests
	group     singleflight.Group     // one build per key at a time
	mu        sync.Mutex             // guards hashes and version
	hashes    map[string]fileHash    // file path -> hash, reused while the file is unchanged
	version   string
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

// New creates a Manager that keeps snapshots in dir. base needs FirecrackerBin, KernelPath and
// RootfsPath; logf may be nil.
func New(dir string, base vmm.Config, logf func(string, ...any)) (*Manager, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create snapshot folder: %w", err)
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	m := &Manager{dir: dir, base: base, settle: DefaultSettle, logf: logf, hashes: map[string]fileHash{}}
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

// Dir returns the folder that holds the snapshots.
func (m *Manager) Dir() string { return m.dir }

// Inputs returns what a snapshot for this memory size and vCPU count depends on right now.
func (m *Manager) Inputs(memMiB, vcpus int64) (Inputs, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
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
	rootfs, err := m.hashFile(m.base.RootfsPath)
	if err != nil {
		return Inputs{}, err
	}
	return Inputs{
		FirecrackerVersion: m.version, KernelSHA256: kernel, RootfsSHA256: rootfs,
		RootfsPath: m.base.RootfsPath, BootArgs: BootArgs, MemMiB: memMiB, VCPUs: vcpus,
	}, nil
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

// Ensure returns the snapshot for this memory size and vCPU count, building it first if it
// does not exist yet. Parallel calls for the same snapshot share one build.
func (m *Manager) Ensure(ctx context.Context, memMiB, vcpus int64) (Snapshot, error) {
	in, err := m.Inputs(memMiB, vcpus)
	if err != nil {
		return Snapshot{}, err
	}
	key := Key(in)
	if s, err := m.load(key); err == nil {
		return s, nil
	}
	// The build is shared, so it must not stop when the first caller goes away.
	v, err, _ := m.group.Do(key, func() (any, error) {
		bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		return m.create(bctx, key, in)
	})
	if err != nil {
		return Snapshot{}, err
	}
	return v.(Snapshot), nil
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
	files := vmm.SnapshotFiles{StatePath: filepath.Join(dir, "vmstate"), MemPath: filepath.Join(dir, "mem")}
	for _, p := range []string{files.StatePath, files.MemPath} {
		if _, err := os.Stat(p); err != nil {
			return Snapshot{}, fmt.Errorf("snapshot %s is incomplete: %w", key, err)
		}
	}
	return Snapshot{Meta: meta, Dir: dir, Files: files}, nil
}

// create builds a snapshot in a temporary folder and renames it into place.
func (m *Manager) create(ctx context.Context, key string, in Inputs) (Snapshot, error) {
	if s, err := m.load(key); err == nil {
		return s, nil // finished just before we got here
	}
	tmp := filepath.Join(m.dir, "tmp-"+randomHex(4))
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return Snapshot{}, err
	}
	defer os.RemoveAll(tmp) // gone already if the rename worked

	cfg := m.base
	cfg.ID = ""
	cfg.BaseDir = filepath.Join(m.dir, "build")
	cfg.SharedRootfs = true
	cfg.ExtraBootArgs = BootArgs
	cfg.MemMiB, cfg.VCPUs = in.MemMiB, in.VCPUs

	m.logf("building snapshot %s (%d MiB, %d vCPU)", key, in.MemMiB, in.VCPUs)
	t0 := time.Now()
	if err := m.build(ctx, cfg, tmp); err != nil {
		return Snapshot{}, fmt.Errorf("build snapshot: %w", err)
	}
	meta := Meta{Key: key, Inputs: in, CreatedAt: time.Now().UTC(), BuildMS: time.Since(t0).Milliseconds()}
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

// buildVM boots a base VM, lets it settle, and snapshots it into dir.
func (m *Manager) buildVM(ctx context.Context, cfg vmm.Config, dir string) error {
	vm, err := vmm.New(cfg)
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
	select {
	case <-time.After(m.settle):
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
		if in, err := m.Inputs(s.Inputs.MemMiB, s.Inputs.VCPUs); err == nil {
			info.Current = Key(in) == s.Key
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
