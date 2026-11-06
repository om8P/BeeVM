package main

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"golang.org/x/sys/unix"

	"github.com/om8P/BeeVM/internal/pkg"
	"github.com/om8P/BeeVM/internal/pool"
	"github.com/om8P/BeeVM/internal/runner"
	"github.com/om8P/BeeVM/internal/snapshot"
	"github.com/om8P/BeeVM/internal/vmm"
)

// benchRun is one measured cold start. Times are in milliseconds.
type benchRun struct {
	Mode     string
	Run      int
	Spawn    float64 // restore: firecracker process start until its API answers
	Load     float64 // restore: loading the snapshot and resuming the vCPUs (our request)
	FCLoad   float64 // restore with --fc-metrics: the same load as Firecracker measured it
	Connect  float64 // restore with --probe health: after the load until /health answered
	Agent    float64 // restore: after the load until the agent answered /resume
	Attempts float64 // restore with --probe resume: requests until the agent answered
	FaultsAg float64 // restore: page faults of the firecracker process during the agent phase
	FaultsFn float64 // restore: page faults during upload + invoke
	Ready    float64 // start until the agent answered (restore: including the resume call)
	Upload   float64 // sending the function's code into the VM
	Import   float64 // python/node: starting the runtime and importing the code
	Invoke   float64 // the call itself, round trip
	First    float64 // start until the function's answer arrived: what a caller waits
}

// benchMetrics names the rows that get percentiles, in table order.
var benchMetrics = []struct {
	name     string
	get      func(benchRun) float64
	only     string // comma-separated modes; "" = every mode
	optional bool   // left out when every value is 0 (the flag that fills it was not set)
}{
	{"spawn", func(r benchRun) float64 { return r.Spawn }, "restore,function", false},
	{"load", func(r benchRun) float64 { return r.Load }, "restore,function", false},
	{"fc load", func(r benchRun) float64 { return r.FCLoad }, "restore,function", true},
	{"connect", func(r benchRun) float64 { return r.Connect }, "restore,function", true},
	{"agent", func(r benchRun) float64 { return r.Agent }, "restore,function", false},
	{"attempts (n)", func(r benchRun) float64 { return r.Attempts }, "restore,function", true},
	{"faults agent (n)", func(r benchRun) float64 { return r.FaultsAg }, "restore,function", true},
	{"faults call (n)", func(r benchRun) float64 { return r.FaultsFn }, "restore,function", true},
	{"vm ready", func(r benchRun) float64 { return r.Ready }, "boot,restore,function", false},
	{"upload", func(r benchRun) float64 { return r.Upload }, "boot,restore", false},
	{"import", func(r benchRun) float64 { return r.Import }, "boot,restore", true},
	{"invoke", func(r benchRun) float64 { return r.Invoke }, "boot,restore,function", false},
	{"first response", func(r benchRun) float64 { return r.First }, "", false},
}

// metricFor reports whether a row belongs in a mode's part of the table.
func metricFor(only, mode string) bool {
	if only == "" {
		return true
	}
	for _, m := range strings.Split(only, ",") {
		if m == mode {
			return true
		}
	}
	return false
}

// benchSettings are the knobs of one benchmark run.
type benchSettings struct {
	Runs      int
	Mem       int64
	VCPUs     int64
	Code      string // the binary or zip package that is uploaded
	CodeName  string // what was given on the command line (for the summary)
	Runtime   string // binary, python or node
	Handler   string // python/node
	Payload   string
	Probe     string        // restore: "resume" (one request) or "health" (old way: /health, then /resume)
	Gap       time.Duration // pause after every run
	ColdCache bool          // drop the images and snapshot from the host page cache before every run
	Settle    time.Duration // 0 = the normal snapshots; else a throw-away snapshot with this settle time
	FCMetrics bool          // ask Firecracker for its own load time
	Pin       []int         // restore: host CPUs for the firecracker process
	Isolation string        // "none" or "jailer"
	Isolator  vmm.Isolator  // set with --isolation jailer
}

// runBench handles `beevmd bench coldstart`.
func runBench(args []string) int {
	if len(args) == 0 || args[0] != "coldstart" {
		fmt.Fprintln(os.Stderr, "usage: beevmd bench coldstart [--runs N] [--mode boot,restore,function,warm] [--code F] [--out DIR]")
		fmt.Fprintln(os.Stderr, "         [--runtime binary|python|node] [--handler H]")
		fmt.Fprintln(os.Stderr, "         [--mem N] [--vcpus N] [--probe resume|health] [--gap D] [--cold-cache]")
		fmt.Fprintln(os.Stderr, "         [--settle D] [--fc-metrics] [--pin CPUS]")
		return 2
	}
	var s benchSettings
	fs := flag.NewFlagSet("bench coldstart", flag.ExitOnError)
	fs.IntVar(&s.Runs, "runs", 100, "measured cold starts per mode")
	modes := fs.String("mode", "boot,restore", "comma-separated: boot, restore (runtime snapshot + upload + import), "+
		"function (function snapshot: code already inside), warm (repeat calls through the warm pool)")
	fs.StringVar(&s.CodeName, "code", "bin/functions/hello-go-binary", "the function: a binary, or a folder/.py/.js/.zip")
	fs.StringVar(&s.Runtime, "runtime", "", "binary, python or node (default: detected from --code)")
	fs.StringVar(&s.Handler, "handler", "", "python/node handler (default handler.handler / index.handler)")
	fs.StringVar(&s.Payload, "payload", `{"name":"bench"}`, "JSON payload")
	out := fs.String("out", "bench/results", "folder for the CSV and summary")
	fs.Int64Var(&s.Mem, "mem", 128, "guest memory in MiB")
	fs.Int64Var(&s.VCPUs, "vcpus", 1, "number of virtual CPUs")
	fs.StringVar(&s.Probe, "probe", "resume", "restore: how to wait for the agent: resume (one request) or health (then resume)")
	fs.DurationVar(&s.Gap, "gap", 0, "pause after every run, e.g. 100ms")
	fs.BoolVar(&s.ColdCache, "cold-cache", false, "drop images and snapshot files from the host page cache before every run")
	fs.DurationVar(&s.Settle, "settle", 0, "build a throw-away snapshot with this settle time (default: use the normal snapshot)")
	fs.BoolVar(&s.FCMetrics, "fc-metrics", false, "restore: also record Firecracker's own snapshot load time")
	pin := fs.String("pin", "", "restore: comma-separated host CPUs for the firecracker process, e.g. 3")
	isoFlags := addIsolationFlags(fs)
	_ = fs.Parse(args[1:])
	if _, err := os.Stat(s.CodeName); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err, "(run: make examples)")
		return 2
	}
	fn, err := openCode(s.CodeName, s.Runtime, s.Handler)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	defer fn.Close()
	s.Code, s.Runtime, s.Handler = fn.Path, fn.Runtime, fn.Handler
	if s.Probe != "resume" && s.Probe != "health" {
		fmt.Fprintf(os.Stderr, "error: --probe must be resume or health, not %q\n", s.Probe)
		return 2
	}
	if s.Pin, err = parseCPUs(*pin); err != nil {
		fmt.Fprintln(os.Stderr, "error: --pin:", err)
		return 2
	}

	home := beevmHome()
	cfg := vmConfig(home, snapshot.BootArgs, s.VCPUs, s.Mem)
	cfg.SharedRootfs = true
	cfg.RootfsPath = rootfsFor(home, s.Runtime)
	cfg.BaseDir = filepath.Join(home, "run", "bench")
	_ = os.RemoveAll(cfg.BaseDir)
	defer os.RemoveAll(cfg.BaseDir)
	iso, err := isoFlags.open(home, &cfg, nil, 2)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer closeIsolation(iso)
	s.Isolation = *isoFlags.mode
	s.Isolator = cfg.Isolator

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var snap snapshot.Snapshot
	var results []benchRun
	for _, mode := range strings.Split(*modes, ",") {
		mode = strings.TrimSpace(mode)
		var one func(i int) (benchRun, error)
		cached := []string{cfg.FirecrackerBin, cfg.KernelPath, cfg.RootfsPath}
		switch mode {
		case "boot":
			one = func(i int) (benchRun, error) { return benchBoot(sigCtx, cfg, s, i) }
		case "restore":
			snap, err = benchSnapshot(sigCtx, home, cfg.BaseDir, s)
			if err != nil {
				fmt.Fprintln(os.Stderr, "error: snapshot:", err)
				return exitFor(sigCtx, 1)
			}
			cached = append(cached, snap.Files.StatePath, snap.Files.MemPath)
			rcfg := cfg
			rcfg.FirecrackerMetrics, rcfg.CPUs = s.FCMetrics, s.Pin
			one = func(i int) (benchRun, error) { return benchRestore(sigCtx, rcfg, snap, s, i) }
		case "function":
			// The function's own snapshot (code uploaded and imported before freezing). Built
			// for a made-up function 0 and deleted again at the end.
			m, err := benchManager(home, cfg.BaseDir, s)
			if err != nil {
				fmt.Fprintln(os.Stderr, "error: snapshot:", err)
				return 1
			}
			bctx, cancel := context.WithTimeout(sigCtx, 3*time.Minute)
			fsnap, err := m.EnsureFunction(bctx, benchFunctionSpec(s))
			cancel()
			if err != nil {
				fmt.Fprintln(os.Stderr, "error: function snapshot:", err)
				return exitFor(sigCtx, 1)
			}
			defer m.RemoveFunction(benchFunctionID)
			snap = fsnap
			cached = append(cached, fsnap.Files.StatePath, fsnap.Files.MemPath)
			rcfg := cfg
			rcfg.FirecrackerMetrics, rcfg.CPUs = s.FCMetrics, s.Pin
			one = func(i int) (benchRun, error) { return benchRestore(sigCtx, rcfg, fsnap, s, i) }
		case "warm":
			snap, err = benchSnapshot(sigCtx, home, cfg.BaseDir, s)
			if err != nil {
				fmt.Fprintln(os.Stderr, "error: snapshot:", err)
				return exitFor(sigCtx, 1)
			}
			// The real warm runner with one slot: the warm-up call restores the VM, every
			// measured call reuses it.
			w := runner.NewWarmRunner(cfg, fixedSnapshot{snap}, pool.Config{MaxVMs: 1, KeepAlive: time.Hour},
				slog.New(slog.NewTextHandler(io.Discard, nil)))
			for rt, path := range runtimeImages(home) {
				w.SetRootfs(rt, path)
			}
			defer w.Close()
			one = func(i int) (benchRun, error) { return benchWarm(sigCtx, w, s, i) }
		default:
			fmt.Fprintf(os.Stderr, "error: unknown mode %q (boot, restore, function or warm)\n", mode)
			return 2
		}
		fmt.Printf("== %s: 1 warm-up + %d measured runs, one at a time\n", mode, s.Runs)
		for i := 0; i <= s.Runs; i++ { // run 0 is the warm-up: fills the page cache, not counted
			if s.ColdCache {
				if err := dropFromPageCache(cached...); err != nil {
					fmt.Fprintln(os.Stderr, "error: --cold-cache:", err)
					return 1
				}
			}
			r, err := one(i)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %s run %d: %v\n", mode, i, err)
				return exitFor(sigCtx, 1)
			}
			if i > 0 {
				results = append(results, r)
			}
			if i > 0 && (i%10 == 0 || i == s.Runs) {
				fmt.Printf("   %d/%d  (last: ready %.1f ms, first response %.1f ms)\n", i, s.Runs, r.Ready, r.First)
			}
			if s.Gap > 0 {
				select {
				case <-time.After(s.Gap):
				case <-sigCtx.Done():
					return 130
				}
			}
		}
	}

	table := benchTable(results)
	fmt.Println()
	fmt.Print(table)
	csvPath, mdPath, err := writeBench(*out, results, table, snap, s)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Printf("\nwrote %s and %s\n", csvPath, mdPath)
	return 0
}

// benchManager is the Snapshot Manager the benchmark uses: the normal one, or with --settle a
// throw-away one in the bench folder (deleted when the benchmark ends).
func benchManager(home, benchDir string, s benchSettings) (*snapshot.Manager, error) {
	say := func(f string, a ...any) { fmt.Printf("== "+f+"\n", a...) }
	if s.Settle <= 0 {
		return snapshotManager(home, s.Isolator, say)
	}
	base := vmConfig(home, snapshot.BootArgs, 1, 128)
	base.Isolator = s.Isolator
	m, err := snapshot.New(filepath.Join(benchDir, "snapshots"), base, say)
	if err != nil {
		return nil, err
	}
	for rt, path := range runtimeImages(home) {
		m.SetRootfs(rt, path)
	}
	m.SetSettle(s.Settle)
	say("throw-away snapshot with settle %s", s.Settle)
	return m, nil
}

// benchSnapshot returns the runtime snapshot to restore from (see benchManager).
func benchSnapshot(ctx context.Context, home, benchDir string, s benchSettings) (snapshot.Snapshot, error) {
	m, err := benchManager(home, benchDir, s)
	if err != nil {
		return snapshot.Snapshot{}, err
	}
	bctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	return m.EnsureRuntime(bctx, s.Runtime, s.Mem, s.VCPUs)
}

// benchFunctionID is the made-up function the benchmark builds its function snapshot for.
// No real function has ID 0, and the server removes such leftovers at start-up.
const benchFunctionID = 0

func benchFunctionSpec(s benchSettings) snapshot.FunctionSpec {
	sum := "bench"
	if b, err := os.ReadFile(s.Code); err == nil {
		h := sha256.Sum256(b)
		sum = hex.EncodeToString(h[:])
	}
	return snapshot.FunctionSpec{Runtime: s.Runtime, Handler: s.Handler, FunctionID: benchFunctionID,
		CodeSHA256: sum, CodePath: s.Code, MemMiB: s.Mem, VCPUs: s.VCPUs}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// benchBoot measures one full boot, upload and call.
func benchBoot(ctx context.Context, cfg vmm.Config, s benchSettings, i int) (benchRun, error) {
	r := benchRun{Mode: "boot", Run: i}
	t0 := time.Now()
	vm, err := vmm.New(cfg)
	if err != nil {
		return r, err
	}
	defer vm.Cleanup(false)
	readyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := vm.Start(readyCtx); err != nil {
		return r, err
	}
	if _, err := vm.WaitForAgent(readyCtx); err != nil {
		return r, err
	}
	r.Ready = ms(time.Since(t0))
	return r, benchCall(readyCtx, vm, s, t0, &r)
}

// benchRestore measures one restore from snap, resume, upload (and import) and call. From a
// function snapshot (mode "function") the code is already inside: no upload, no import.
func benchRestore(ctx context.Context, cfg vmm.Config, snap snapshot.Snapshot, s benchSettings, i int) (benchRun, error) {
	r := benchRun{Mode: "restore", Run: i}
	if snap.IsFunction() {
		r.Mode = "function"
	}
	readyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	t0 := time.Now()
	vm, tm, err := vmm.Restore(readyCtx, cfg, snap.Files)
	if err != nil {
		return r, err
	}
	defer vm.Cleanup(false)
	r.Spawn, r.Load = ms(tm.Spawn), ms(tm.Load)
	f0 := processFaults(vm.PID())

	loaded := time.Now()
	if s.Probe == "health" { // the way it was done before WaitAndResume, for comparison
		if _, err := vm.WaitForAgent(readyCtx); err != nil {
			return r, err
		}
		r.Connect = ms(time.Since(loaded))
		if _, err := vm.Agent().Resume(readyCtx); err != nil {
			return r, err
		}
	} else {
		info, err := vm.WaitAndResume(readyCtx)
		if err != nil {
			return r, err
		}
		r.Attempts = float64(info.Attempts)
	}
	r.Agent = ms(time.Since(loaded))
	r.Ready = ms(time.Since(t0))
	f1 := processFaults(vm.PID())
	if err := benchCall(readyCtx, vm, s, t0, &r); err != nil {
		return r, err
	}
	f2 := processFaults(vm.PID())
	if f0 >= 0 && f1 >= 0 && f2 >= 0 {
		r.FaultsAg, r.FaultsFn = float64(f1-f0), float64(f2-f1)
	}
	if s.FCMetrics { // asked last, so the extra request is not part of any measured time
		d, err := vm.FirecrackerLoadTime(readyCtx)
		if err != nil {
			return r, fmt.Errorf("firecracker metrics: %w", err)
		}
		r.FCLoad = ms(d)
	}
	return r, nil
}

// fixedSnapshot hands the warm runner the runtime snapshot the benchmark already chose. It
// has no function snapshots, so the warm-up call restores the runtime snapshot and imports.
type fixedSnapshot struct{ snap snapshot.Snapshot }

func (f fixedSnapshot) Ensure(context.Context, int64, int64) (snapshot.Snapshot, error) {
	return f.snap, nil
}

func (f fixedSnapshot) EnsureRuntime(context.Context, string, int64, int64) (snapshot.Snapshot, error) {
	return f.snap, nil
}

func (f fixedSnapshot) LookupFunction(snapshot.FunctionSpec) (snapshot.Snapshot, bool) {
	return snapshot.Snapshot{}, false
}

func (f fixedSnapshot) EnsureFunction(context.Context, snapshot.FunctionSpec) (snapshot.Snapshot, error) {
	return snapshot.Snapshot{}, errors.New("no function snapshots in the warm benchmark")
}

func (f fixedSnapshot) RemoveFunction(int64) (int, error) { return 0, nil }

// benchWarm measures one call through the warm pool: take the idle VM, call the function, give
// the VM back. Run 0 (the warm-up) creates the VM; every later run must reuse it.
func benchWarm(ctx context.Context, w *runner.WarmRunner, s benchSettings, i int) (benchRun, error) {
	r := benchRun{Mode: "warm", Run: i}
	code, err := filepath.Abs(s.Code)
	if err != nil {
		return r, err
	}
	t0 := time.Now()
	res, err := w.Run(ctx, runner.Request{
		CodePath: code, Payload: json.RawMessage(s.Payload), MemoryMiB: s.Mem, VCPUs: s.VCPUs, TimeoutMS: 10_000,
		Runtime: s.Runtime, Handler: s.Handler, TenantID: 1, FunctionID: 1, CodeSHA256: "bench",
	})
	r.First = ms(time.Since(t0))
	if err != nil {
		return r, err
	}
	if res.FunctionError != "" {
		return r, fmt.Errorf("function failed: %s", res.FunctionError)
	}
	if i > 0 && res.StartKind != runner.StartWarm {
		return r, fmt.Errorf("call was %s, not warm: the VM was not reused", res.StartKind)
	}
	return r, nil
}

// benchCall uploads the function (and imports it, for python/node) and calls it once, filling
// Upload, Import, Invoke and First. In mode "function" the code is already inside the VM.
func benchCall(ctx context.Context, vm *vmm.VM, s benchSettings, t0 time.Time, r *benchRun) error {
	if r.Mode != "function" {
		t1 := time.Now()
		var err error
		if pkg.IsPackaged(s.Runtime) {
			_, err = vm.Agent().UploadPackageFile(ctx, s.Code)
		} else {
			_, err = vm.Agent().UploadFunctionFile(ctx, s.Code)
		}
		if err != nil {
			return fmt.Errorf("upload: %w", err)
		}
		r.Upload = ms(time.Since(t1))
		if pkg.IsPackaged(s.Runtime) {
			t1 = time.Now()
			if _, err := vm.Agent().Load(ctx, s.Runtime, s.Handler, 0); err != nil {
				return fmt.Errorf("import: %w", err)
			}
			r.Import = ms(time.Since(t1))
		}
	}
	t2 := time.Now()
	res, err := vm.Agent().Invoke(ctx, json.RawMessage(s.Payload), 10*time.Second)
	if err != nil {
		return fmt.Errorf("invoke: %w", err)
	}
	if res.Error != "" {
		return fmt.Errorf("function failed: %s", res.Error)
	}
	r.Invoke = ms(time.Since(t2))
	r.First = ms(time.Since(t0))
	return nil
}

// dropFromPageCache asks the kernel to forget the cached contents of these files, so the
// next run reads them from disk again (no root needed, unlike drop_caches). Pages that a
// running process has mapped stay; the previous VM is gone by then.
func dropFromPageCache(paths ...string) error {
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		err = unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED)
		f.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
	}
	return nil
}

// processFaults returns the page faults (minor + major) of all threads of a process so far,
// or -1 if they cannot be read. When the guest touches a page of its memory for the first
// time after a restore, the firecracker vCPU thread takes a fault for it, so this counts how
// much memory the guest pulled in.
func processFaults(pid int) int64 {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return -1
	}
	// The command name (field 2) is in parentheses and may contain spaces: skip past it.
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 {
		return -1
	}
	f := strings.Fields(string(b[i+1:]))
	// After ")": state ppid pgrp session tty tpgid flags minflt cminflt majflt
	if len(f) < 10 {
		return -1
	}
	minflt, err1 := strconv.ParseInt(f[7], 10, 64)
	majflt, err2 := strconv.ParseInt(f[9], 10, 64)
	if err1 != nil || err2 != nil {
		return -1
	}
	return minflt + majflt
}

// parseCPUs turns "2,3" into [2 3]; "" gives nil.
func parseCPUs(s string) ([]int, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var cpus []int
	for _, f := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || n < 0 {
			return nil, fmt.Errorf("bad CPU number %q", f)
		}
		if n >= runtime.NumCPU() {
			return nil, fmt.Errorf("CPU %d does not exist (this machine has %d)", n, runtime.NumCPU())
		}
		cpus = append(cpus, n)
	}
	return cpus, nil
}

// percentile returns the p-th percentile (0-100) by the nearest-rank method.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	return sorted[rank-1]
}

// benchTable formats p50/p90/p99/max per mode and metric.
func benchTable(results []benchRun) string {
	var sb strings.Builder
	w := tabwriter.NewWriter(&sb, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "mode\tmetric (ms)\tp50\tp90\tp99\tmax\truns\t")
	var modes []string
	seen := map[string]bool{}
	for _, r := range results {
		if !seen[r.Mode] {
			seen[r.Mode] = true
			modes = append(modes, r.Mode)
		}
	}
	for _, mode := range modes {
		for _, m := range benchMetrics {
			if !metricFor(m.only, mode) {
				continue
			}
			var v []float64
			for _, r := range results {
				if r.Mode == mode {
					v = append(v, m.get(r))
				}
			}
			sort.Float64s(v)
			if m.optional && v[len(v)-1] == 0 {
				continue
			}
			fmt.Fprintf(w, "%s\t%s\t%.1f\t%.1f\t%.1f\t%.1f\t%d\t\n", mode, m.name,
				percentile(v, 50), percentile(v, 90), percentile(v, 99), v[len(v)-1], len(v))
		}
	}
	w.Flush()
	return sb.String()
}

// describeSettings is one line for the summary saying how the benchmark was run.
func describeSettings(s benchSettings) string {
	parts := []string{"probe " + s.Probe}
	if s.ColdCache {
		parts = append(parts, "**page cache cold** (files dropped before every run)")
	} else {
		parts = append(parts, "page cache warm (files already in RAM)")
	}
	if s.Gap > 0 {
		parts = append(parts, "gap "+s.Gap.String()+" between runs")
	}
	if s.Settle > 0 {
		parts = append(parts, "throw-away snapshot with settle "+s.Settle.String())
	}
	if s.FCMetrics {
		parts = append(parts, "Firecracker metrics on")
	}
	if len(s.Pin) > 0 {
		parts = append(parts, fmt.Sprintf("firecracker pinned to CPUs %v (restore only)", s.Pin))
	}
	if s.Isolation != "" && s.Isolation != "none" {
		parts = append(parts, "**isolation "+s.Isolation+"**")
	}
	return strings.Join(parts, "; ")
}

// writeBench saves the raw runs as CSV and a short Markdown summary for the report.
func writeBench(dir string, results []benchRun, table string, snap snapshot.Snapshot, s benchSettings) (string, string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	stamp := time.Now().Format("20060102-150405")
	csvPath := filepath.Join(dir, "coldstart-"+stamp+".csv")
	f, err := os.Create(csvPath)
	if err != nil {
		return "", "", err
	}
	cw := csv.NewWriter(f)
	_ = cw.Write([]string{"mode", "run", "spawn_ms", "load_ms", "fc_load_ms", "connect_ms", "agent_ms", "attempts", "faults_agent", "faults_call",
		"ready_ms", "upload_ms", "import_ms", "invoke_ms", "first_response_ms"})
	for _, r := range results {
		_ = cw.Write([]string{r.Mode, fmt.Sprint(r.Run), f2(r.Spawn), f2(r.Load), f2(r.FCLoad), f2(r.Connect), f2(r.Agent),
			fmt.Sprint(r.Attempts), fmt.Sprint(r.FaultsAg), fmt.Sprint(r.FaultsFn), f2(r.Ready), f2(r.Upload), f2(r.Import), f2(r.Invoke), f2(r.First)})
	}
	cw.Flush()
	if err := errors.Join(cw.Error(), f.Close()); err != nil {
		return "", "", err
	}

	kernel, _ := exec.Command("uname", "-r").Output()
	host, _ := os.Hostname()
	var md strings.Builder
	fmt.Fprintf(&md, "# Cold start benchmark, %s\n\n", time.Now().Format("2006-01-02 15:04"))
	fmt.Fprintf(&md, "- Host: %s, kernel %s, %d CPUs (Go sees)\n", host, strings.TrimSpace(string(kernel)), runtime.NumCPU())
	if snap.Key != "" {
		fmt.Fprintf(&md, "- Firecracker: %s; snapshot %s (memory file %s)\n", snap.Inputs.FirecrackerVersion, snap.Key, humanBytes(snap.MemBytes))
	}
	fmt.Fprintf(&md, "- VM: %d MiB, %d vCPU; function %s (%s); %d measured runs per mode after 1 warm-up, one at a time\n",
		s.Mem, s.VCPUs, filepath.Base(s.CodeName), s.Runtime, s.Runs)
	fmt.Fprintf(&md, "- Settings: %s. Raw data: `%s`\n\n", describeSettings(s), filepath.Base(csvPath))
	fmt.Fprintf(&md, "- **load**: our `PUT /snapshot/load` request (incl. resuming the vCPUs); **fc load**: the same, as Firecracker measured it\n")
	fmt.Fprintf(&md, "- **agent**: after the load until the guest agent answered `/resume` (clock and entropy fixed); **connect**: until `/health` answered (probe health only)\n")
	fmt.Fprintf(&md, "- **vm ready**: start until the guest agent is ready (restore: spawn + load + agent)\n")
	fmt.Fprintf(&md, "- **first response**: start until the function's answer arrived, including uploading its code\n")
	fmt.Fprintf(&md, "- **import** (python/node): starting the runtime's import of the uploaded package\n")
	fmt.Fprintf(&md, "- **function**: restored from the function's own snapshot: code already uploaded and imported, so no upload or import\n")
	fmt.Fprintf(&md, "- **warm**: a repeat call through the warm pool (take the idle VM, call, give it back); no restore, no upload\n\n")
	fmt.Fprintf(&md, "```text\n%s```\n", table)
	mdPath := filepath.Join(dir, "coldstart-"+stamp+".md")
	return csvPath, mdPath, os.WriteFile(mdPath, []byte(md.String()), 0o644)
}

func f2(v float64) string { return fmt.Sprintf("%.2f", v) }
