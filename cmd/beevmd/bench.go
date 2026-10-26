package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/om8P/BeeVM/internal/snapshot"
	"github.com/om8P/BeeVM/internal/vmm"
)

// benchRun is one measured cold start. Times are in milliseconds.
type benchRun struct {
	Mode   string
	Run    int
	Spawn  float64 // restore only: firecracker process start until its API answers
	Load   float64 // restore only: loading the snapshot and resuming the vCPUs
	Ready  float64 // start until the agent answered (restore: including the resume call)
	Upload float64 // sending the function's code into the VM
	Invoke float64 // the call itself, round trip
	First  float64 // start until the function's answer arrived: what a caller waits
}

// benchMetrics names the columns that get percentiles, in table order.
var benchMetrics = []struct {
	name string
	get  func(benchRun) float64
	only string // "" = every mode
}{
	{"spawn", func(r benchRun) float64 { return r.Spawn }, "restore"},
	{"load", func(r benchRun) float64 { return r.Load }, "restore"},
	{"vm ready", func(r benchRun) float64 { return r.Ready }, ""},
	{"upload", func(r benchRun) float64 { return r.Upload }, ""},
	{"invoke", func(r benchRun) float64 { return r.Invoke }, ""},
	{"first response", func(r benchRun) float64 { return r.First }, ""},
}

// runBench handles `beevmd bench coldstart`.
func runBench(args []string) int {
	if len(args) == 0 || args[0] != "coldstart" {
		fmt.Fprintln(os.Stderr, "usage: beevmd bench coldstart [--runs N] [--mode boot,restore] [--code F] [--out DIR]")
		return 2
	}
	fs := flag.NewFlagSet("bench coldstart", flag.ExitOnError)
	runs := fs.Int("runs", 100, "measured cold starts per mode")
	modes := fs.String("mode", "boot,restore", "comma-separated: boot, restore")
	code := fs.String("code", "bin/functions/hello-go-binary", "function binary to run")
	payload := fs.String("payload", `{"name":"bench"}`, "JSON payload")
	out := fs.String("out", "bench/results", "folder for the CSV and summary")
	mem := fs.Int64("mem", 128, "guest memory in MiB")
	vcpus := fs.Int64("vcpus", 1, "number of virtual CPUs")
	_ = fs.Parse(args[1:])
	if _, err := os.Stat(*code); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err, "(run: make examples)")
		return 2
	}

	home := beevmHome()
	cfg := vmConfig(home, snapshot.BootArgs, *vcpus, *mem)
	cfg.SharedRootfs = true
	cfg.BaseDir = filepath.Join(home, "run", "bench")
	_ = os.RemoveAll(cfg.BaseDir)

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var snap snapshot.Snapshot
	var results []benchRun
	for _, mode := range strings.Split(*modes, ",") {
		mode = strings.TrimSpace(mode)
		var one func(i int) (benchRun, error)
		switch mode {
		case "boot":
			one = func(i int) (benchRun, error) { return benchBoot(sigCtx, cfg, *code, *payload, i) }
		case "restore":
			m, err := snapshotManager(home, func(f string, a ...any) { fmt.Printf("== "+f+"\n", a...) })
			if err == nil {
				ctx, cancel := context.WithTimeout(sigCtx, 2*time.Minute)
				snap, err = m.Ensure(ctx, *mem, *vcpus)
				cancel()
			}
			if err != nil {
				fmt.Fprintln(os.Stderr, "error: snapshot:", err)
				return 1
			}
			one = func(i int) (benchRun, error) { return benchRestore(sigCtx, cfg, snap, *code, *payload, i) }
		default:
			fmt.Fprintf(os.Stderr, "error: unknown mode %q (boot or restore)\n", mode)
			return 2
		}
		fmt.Printf("== %s: 1 warm-up + %d measured runs, one at a time\n", mode, *runs)
		if _, err := one(0); err != nil { // warm-up: fills the page cache, not counted
			fmt.Fprintf(os.Stderr, "error: %s warm-up: %v\n", mode, err)
			return exitFor(sigCtx, 1)
		}
		for i := 1; i <= *runs; i++ {
			r, err := one(i)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %s run %d: %v\n", mode, i, err)
				return exitFor(sigCtx, 1)
			}
			results = append(results, r)
			if i%10 == 0 || i == *runs {
				fmt.Printf("   %d/%d  (last: ready %.1f ms, first response %.1f ms)\n", i, *runs, r.Ready, r.First)
			}
		}
	}

	table := benchTable(results)
	fmt.Println()
	fmt.Print(table)
	csvPath, mdPath, err := writeBench(*out, results, table, snap, *runs, *mem, *vcpus, *code)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Printf("\nwrote %s and %s\n", csvPath, mdPath)
	return 0
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// benchBoot measures one full boot, upload and call.
func benchBoot(ctx context.Context, cfg vmm.Config, code, payload string, i int) (benchRun, error) {
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
	return r, benchCall(readyCtx, vm, code, payload, t0, &r)
}

// benchRestore measures one restore from snap, resume, upload and call.
func benchRestore(ctx context.Context, cfg vmm.Config, snap snapshot.Snapshot, code, payload string, i int) (benchRun, error) {
	r := benchRun{Mode: "restore", Run: i}
	readyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	t0 := time.Now()
	vm, tm, err := vmm.Restore(readyCtx, cfg, snap.Files)
	if err != nil {
		return r, err
	}
	defer vm.Cleanup(false)
	r.Spawn, r.Load = ms(tm.Spawn), ms(tm.Load)
	if _, err := vm.WaitForAgent(readyCtx); err != nil {
		return r, err
	}
	if _, err := vm.Agent().Resume(readyCtx); err != nil {
		return r, err
	}
	r.Ready = ms(time.Since(t0))
	return r, benchCall(readyCtx, vm, code, payload, t0, &r)
}

// benchCall uploads the function and calls it once, filling Upload, Invoke and First.
func benchCall(ctx context.Context, vm *vmm.VM, code, payload string, t0 time.Time, r *benchRun) error {
	t1 := time.Now()
	if _, err := vm.Agent().UploadFunctionFile(ctx, code); err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	r.Upload = ms(time.Since(t1))
	t2 := time.Now()
	res, err := vm.Agent().Invoke(ctx, json.RawMessage(payload), 10*time.Second)
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
			if m.only != "" && m.only != mode {
				continue
			}
			var v []float64
			for _, r := range results {
				if r.Mode == mode {
					v = append(v, m.get(r))
				}
			}
			sort.Float64s(v)
			fmt.Fprintf(w, "%s\t%s\t%.1f\t%.1f\t%.1f\t%.1f\t%d\t\n", mode, m.name,
				percentile(v, 50), percentile(v, 90), percentile(v, 99), v[len(v)-1], len(v))
		}
	}
	w.Flush()
	return sb.String()
}

// writeBench saves the raw runs as CSV and a short Markdown summary for the report.
func writeBench(dir string, results []benchRun, table string, snap snapshot.Snapshot, runs int, mem, vcpus int64, code string) (string, string, error) {
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
	_ = cw.Write([]string{"mode", "run", "spawn_ms", "load_ms", "ready_ms", "upload_ms", "invoke_ms", "first_response_ms"})
	for _, r := range results {
		_ = cw.Write([]string{r.Mode, fmt.Sprint(r.Run), f2(r.Spawn), f2(r.Load), f2(r.Ready), f2(r.Upload), f2(r.Invoke), f2(r.First)})
	}
	cw.Flush()
	if err := f.Close(); err != nil {
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
	fmt.Fprintf(&md, "- VM: %d MiB, %d vCPU; function %s; %d measured runs per mode after 1 warm-up, one at a time\n", mem, vcpus, filepath.Base(code), runs)
	fmt.Fprintf(&md, "- Page cache warm (snapshot and rootfs files already in RAM). Raw data: `%s`\n\n", filepath.Base(csvPath))
	fmt.Fprintf(&md, "- **vm ready**: start until the guest agent answers (restore: including the clock/entropy resume call)\n")
	fmt.Fprintf(&md, "- **first response**: start until the function's answer arrived, including uploading its code\n\n")
	fmt.Fprintf(&md, "```text\n%s```\n", table)
	mdPath := filepath.Join(dir, "coldstart-"+stamp+".md")
	return csvPath, mdPath, os.WriteFile(mdPath, []byte(md.String()), 0o644)
}

func f2(v float64) string { return fmt.Sprintf("%.2f", v) }
