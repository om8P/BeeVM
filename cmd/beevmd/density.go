package main

import (
	"context"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/om8P/BeeVM/internal/apiclient"
	"github.com/om8P/BeeVM/internal/apitypes"
)

// `beevmd bench density` answers "how many idle MicroVMs fit on this node?". It talks to a running
// server through the public API only: it asks the server to prewarm VMs in steps, reads
// GET /api/v1/node after every step, and makes a few warm calls to see whether the VMs already
// there get slower. It stops when memory would drop below a reserve, when the server cannot start
// more VMs, or at --max.
//
// Three numbers per VM are reported, because "memory per VM" has three honest answers:
//   - RSS: all pages the Firecracker process has in RAM; pages shared with other VMs count in every VM.
//   - PSS: RSS with shared pages divided by the number of processes sharing them (sums add up).
//   - host: how much the node's MemAvailable really dropped, divided by the VMs added. This also
//     includes the server's own bookkeeping and the kernel's page tables, so it is the number to plan with.

// densityAPI is the part of apiclient.Client the density run needs.
type densityAPI interface {
	Prewarm(ctx context.Context, name string, n int) (apitypes.PrewarmResult, error)
	Node(ctx context.Context) (apitypes.Node, error)
	Invoke(ctx context.Context, name string, payload []byte) (apiclient.InvokeResult, error)
}

type densitySettings struct {
	Fns     []string
	Step    int
	Max     int           // VMs to start at most (ours, not counting ones already there)
	Reserve uint64        // stop before the node's available memory falls below this
	Settle  time.Duration // wait after a step before measuring (the restores finish faulting pages)
	Probe   int           // warm calls after every step
	Payload []byte
}

// densityStep is one measurement after a step.
type densityStep struct {
	VMs       int // VMs this run started (all idle)
	Idle      int // idle VMs on the whole node
	PSSBytes  uint64
	RSSBytes  uint64
	Available uint64 // node MemAvailable
	PSSPerVM  float64
	RSSPerVM  float64
	HostPerVM float64
	ProbeP50  float64 // ms, warm call round trip
	ProbeP99  float64
	ProbeFail int
}

type densityResult struct {
	BaseVMs      int    // VMs already on the node before the run
	BaseAvail    uint64 // MemAvailable before the first step
	MemTotal     uint64
	Steps        []densityStep
	Stop         string
	Extrapolated int // how many VMs would fit above the reserve at the measured host cost
}

// splitSteps spreads n VMs over the functions as evenly as possible (first ones get the rest).
func splitSteps(n, fns int) []int {
	out := make([]int, fns)
	for i := range out {
		out[i] = n / fns
		if i < n%fns {
			out[i]++
		}
	}
	return out
}

func runDensity(ctx context.Context, c densityAPI, s densitySettings, progress io.Writer) (densityResult, error) {
	var res densityResult
	base, err := c.Node(ctx)
	if err != nil {
		return res, fmt.Errorf("reading /api/v1/node: %w", err)
	}
	res.BaseVMs, res.BaseAvail, res.MemTotal = base.VMs, base.MemAvailableBytes, base.MemTotalBytes
	if base.MaxVMs > 0 && base.MaxVMs-base.VMs < s.Max {
		fmt.Fprintf(progress, "warning: the server allows %d VMs (--max-vms) and %d are already there; this run will stop at %d\n",
			base.MaxVMs, base.VMs, base.MaxVMs-base.VMs)
	}

	started := 0
	var hostPerVM float64 // from the last measurement, used to predict the next step
	for started < s.Max {
		if ctx.Err() != nil {
			res.Stop = "interrupted"
			break
		}
		ask := min(s.Step, s.Max-started)
		if hostPerVM > 0 && len(res.Steps) > 0 {
			avail := res.Steps[len(res.Steps)-1].Available
			room := 0.0
			if avail > s.Reserve {
				room = float64(avail-s.Reserve) / hostPerVM
			}
			if int(room) < ask {
				ask = int(room)
			}
			if ask < 1 {
				res.Stop = "memory reserve reached"
				break
			}
		}

		got := 0
		var errs []error
		for i, n := range splitSteps(ask, len(s.Fns)) {
			if n == 0 {
				continue
			}
			r, err := c.Prewarm(ctx, s.Fns[i], n)
			got += r.Started
			if err != nil {
				errs = append(errs, err)
			}
		}
		started += got

		if err := sleepCtx(ctx, s.Settle); err != nil {
			res.Stop = "interrupted"
			break
		}
		n, err := c.Node(ctx)
		if err != nil {
			return res, fmt.Errorf("reading /api/v1/node: %w", err)
		}
		row := densityStep{VMs: started, Idle: n.States["idle"], PSSBytes: n.PSSBytes, RSSBytes: n.RSSBytes, Available: n.MemAvailableBytes}
		if n.MeasuredVMs > 0 {
			row.PSSPerVM = float64(n.PSSBytes) / float64(n.MeasuredVMs)
			row.RSSPerVM = float64(n.RSSBytes) / float64(n.MeasuredVMs)
		}
		if started > 0 && res.BaseAvail > n.MemAvailableBytes {
			row.HostPerVM = float64(res.BaseAvail-n.MemAvailableBytes) / float64(started)
			hostPerVM = row.HostPerVM
		}
		row.ProbeP50, row.ProbeP99, row.ProbeFail = densityProbe(ctx, c, s)
		res.Steps = append(res.Steps, row)
		fmt.Fprintf(progress, "%5d VMs  idle %5d  PSS %5.1f MiB  RSS %5.1f MiB  host %5.1f MiB per VM  available %6.0f MiB  warm p50 %.1f ms p99 %.1f ms\n",
			started, row.Idle, row.PSSPerVM/mib, row.RSSPerVM/mib, row.HostPerVM/mib, float64(row.Available)/mib, row.ProbeP50, row.ProbeP99)

		switch {
		case got < ask:
			res.Stop = fmt.Sprintf("the server started only %d of %d VMs (pool full or errors)", got, ask)
			if len(errs) > 0 {
				res.Stop += ": " + errors.Join(errs...).Error()
			}
		case n.MemAvailableBytes < s.Reserve:
			res.Stop = "memory reserve reached"
		}
		if res.Stop != "" {
			break
		}
	}
	if res.Stop == "" {
		res.Stop = fmt.Sprintf("reached --max %d", s.Max)
	}
	if hostPerVM > 0 && res.BaseAvail > s.Reserve {
		res.Extrapolated = int(float64(res.BaseAvail-s.Reserve) / hostPerVM)
	}
	return res, nil
}

const mib = 1 << 20

// densityProbe makes s.Probe warm calls and returns the p50/p99 round trip and the failed calls.
func densityProbe(ctx context.Context, c densityAPI, s densitySettings) (p50, p99 float64, failed int) {
	var rtt []float64
	for i := 0; i < s.Probe; i++ {
		t0 := time.Now()
		_, err := c.Invoke(ctx, s.Fns[i%len(s.Fns)], s.Payload)
		if err != nil {
			failed++
			continue
		}
		rtt = append(rtt, ms(time.Since(t0)))
	}
	sort.Float64s(rtt)
	if len(rtt) == 0 {
		return 0, 0, failed
	}
	return percentile(rtt, 50), percentile(rtt, 99), failed
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// densityTable formats the steps for the terminal and the Markdown summary.
func densityTable(r densityResult) string {
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "VMs\tidle\tPSS MiB/VM\tRSS MiB/VM\thost MiB/VM\tavailable MiB\twarm p50 ms\twarm p99 ms\tfailed\t")
	for _, s := range r.Steps {
		fmt.Fprintf(tw, "%d\t%d\t%.1f\t%.1f\t%.1f\t%.0f\t%.1f\t%.1f\t%d\t\n", s.VMs, s.Idle,
			s.PSSPerVM/mib, s.RSSPerVM/mib, s.HostPerVM/mib, float64(s.Available)/mib, s.ProbeP50, s.ProbeP99, s.ProbeFail)
	}
	tw.Flush()
	return b.String()
}

// writeDensity saves density-<label>-<time>.csv (one row per step) and .md (summary for the report).
func writeDensity(dir, label string, r densityResult, s densitySettings) (string, string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	stamp := time.Now().Format("20060102-150405")
	base := filepath.Join(dir, "density-"+label+"-"+stamp)
	f, err := os.Create(base + ".csv")
	if err != nil {
		return "", "", err
	}
	cw := csv.NewWriter(f)
	_ = cw.Write([]string{"scenario", "vms", "idle", "pss_bytes", "rss_bytes", "available_bytes", "pss_per_vm", "rss_per_vm", "host_per_vm",
		"probe_p50_ms", "probe_p99_ms", "probe_failed"})
	for _, st := range r.Steps {
		_ = cw.Write([]string{label, strconv.Itoa(st.VMs), strconv.Itoa(st.Idle), strconv.FormatUint(st.PSSBytes, 10), strconv.FormatUint(st.RSSBytes, 10),
			strconv.FormatUint(st.Available, 10), f2(st.PSSPerVM), f2(st.RSSPerVM), f2(st.HostPerVM), f2(st.ProbeP50), f2(st.ProbeP99), strconv.Itoa(st.ProbeFail)})
	}
	cw.Flush()
	if err := errors.Join(cw.Error(), f.Close()); err != nil {
		return "", "", err
	}

	var md strings.Builder
	last := densityStep{}
	if len(r.Steps) > 0 {
		last = r.Steps[len(r.Steps)-1]
	}
	fmt.Fprintf(&md, "# Density: %s, %s\n\n", label, time.Now().Format("2006-01-02 15:04"))
	fmt.Fprintf(&md, "- Functions: %s (round-robin); step %d, max %d, reserve %d MiB, settle %s, %d warm calls per step\n",
		strings.Join(s.Fns, ", "), s.Step, s.Max, s.Reserve/mib, s.Settle, s.Probe)
	fmt.Fprintf(&md, "- Node: %d MiB RAM, %d MiB available at the start, %d VMs already running\n", r.MemTotal/mib, r.BaseAvail/mib, r.BaseVMs)
	fmt.Fprintf(&md, "- **Idle VMs reached: %d** (stopped: %s)\n", last.VMs, r.Stop)
	fmt.Fprintf(&md, "- Memory per idle VM: **PSS %.1f MiB**, RSS %.1f MiB, **host %.1f MiB** (MemAvailable drop / VMs)\n",
		last.PSSPerVM/mib, last.RSSPerVM/mib, last.HostPerVM/mib)
	if r.Extrapolated > 0 {
		fmt.Fprintf(&md, "- At that host cost, %d MiB above a %d MiB reserve would hold about **%d idle VMs**\n",
			(r.BaseAvail-s.Reserve)/mib, s.Reserve/mib, r.Extrapolated)
	}
	fmt.Fprintf(&md, "\n```text\n%s```\n", densityTable(r))
	return base + ".csv", base + ".md", os.WriteFile(base+".md", []byte(md.String()), 0o644)
}

// parseSize reads "1GiB", "512MiB" or a plain number of bytes.
func parseSize(v string) (uint64, error) {
	v = strings.TrimSpace(v)
	mult := uint64(1)
	for suffix, m := range map[string]uint64{"GiB": 1 << 30, "MiB": 1 << 20, "KiB": 1 << 10} {
		if strings.HasSuffix(v, suffix) {
			v, mult = strings.TrimSuffix(v, suffix), m
			break
		}
	}
	n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bad size %q (use e.g. 1GiB, 512MiB)", v)
	}
	return n * mult, nil
}

// runBenchDensity handles `beevmd bench density`.
func runBenchDensity(args []string) int {
	fs := flag.NewFlagSet("bench density", flag.ExitOnError)
	fns := fs.String("fn", "", "deployed function(s), comma-separated; VMs are spread over them")
	step := fs.Int("step", 20, "VMs to start per step")
	max := fs.Int("max", 1000, "most VMs to start")
	reserve := fs.String("reserve", "1GiB", "stop before the node's available memory falls below this")
	settle := fs.Duration("settle", 2*time.Second, "wait after each step before measuring")
	probe := fs.Int("probe", 20, "warm calls after each step (latency with this many VMs)")
	payload := fs.String("payload", `{"name":"density"}`, "JSON payload of the warm calls")
	label := fs.String("label", "density", "scenario name (used in the file names and charts)")
	url := fs.String("url", envOr("BEEVM_URL", "http://127.0.0.1:8080"), "server address (env BEEVM_URL)")
	out := fs.String("out", "", "write density-<label>-<time>.{csv,md} here (e.g. bench/results)")
	_ = fs.Parse(args)

	s := densitySettings{Step: *step, Max: *max, Settle: *settle, Probe: *probe, Payload: []byte(*payload)}
	for _, f := range strings.Split(*fns, ",") {
		if f = strings.TrimSpace(f); f != "" {
			s.Fns = append(s.Fns, f)
		}
	}
	res, err := parseSize(*reserve)
	key := os.Getenv("BEEVM_API_KEY")
	switch {
	case len(s.Fns) == 0:
		fmt.Fprintln(os.Stderr, "usage: beevmd bench density --fn NAME[,NAME...] [--step 20] [--max 1000] [--reserve 1GiB] [--settle 2s] [--probe 20] [--label L] [--url U] [--out DIR]")
		fmt.Fprintln(os.Stderr, "       start the server with --max-vms >= --max and --keep-alive 1h so idle VMs stay")
		return 2
	case err != nil:
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	case key == "":
		fmt.Fprintln(os.Stderr, "error: set BEEVM_API_KEY (a tenant's key; the functions must belong to that tenant)")
		return 2
	case s.Step < 1 || s.Max < 1 || s.Probe < 0:
		fmt.Fprintln(os.Stderr, "error: --step and --max must be at least 1, --probe at least 0")
		return 2
	}
	s.Reserve = res

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Printf("density: %s, %d VMs per step up to %d, reserve %s, against %s (Ctrl-C stops early)\n",
		strings.Join(s.Fns, ","), s.Step, s.Max, *reserve, *url)
	r, err := runDensity(ctx, apiclient.New(*url, key), s, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Printf("\nstopped: %s\n\n%s", r.Stop, densityTable(r))
	if *out != "" {
		_, md, err := writeDensity(*out, *label, r, s)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		fmt.Println("wrote", md)
	}
	return 0
}
