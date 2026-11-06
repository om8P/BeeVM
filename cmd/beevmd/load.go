package main

import (
	"context"
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
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/om8P/BeeVM/internal/apiclient"
)

// `beevmd bench load` sends calls to a running server at a fixed rate, through the public API,
// like real users would. It is "open loop": calls start on a timer whether or not earlier calls
// have finished (up to --concurrency at once), so a slow server shows up as higher latency and
// skipped calls instead of quietly lowering the load. Watch Grafana while it runs.

type loadSettings struct {
	Fns         []string
	Rate        float64 // calls per second, over all functions
	Duration    time.Duration
	Concurrency int
	Payload     []byte
	Report      time.Duration // progress line interval; 0 = none
}

// loadCall is one finished call.
type loadCall struct {
	Fn     string
	Start  string  // warm, restore, boot; "-" when the call failed before a VM ran it
	Status string  // HTTP status ("200", "502", "503", ...) or "error" (no answer)
	RTT    float64 // round trip in milliseconds, as the caller saw it
}

// invoker is the part of apiclient.Client the load test needs (tests use a real client
// against an httptest server).
type invoker interface {
	Invoke(ctx context.Context, name string, payload []byte) (apiclient.InvokeResult, error)
}

// loadResult is everything a load run measured.
type loadResult struct {
	Calls   []loadCall
	Skipped int // ticks with --concurrency calls already running
	Elapsed time.Duration
}

// runLoadTest fires calls at s.Rate for s.Duration (or until ctx ends), round-robin over s.Fns,
// then waits for the calls still running.
func runLoadTest(ctx context.Context, c invoker, s loadSettings, progress io.Writer) loadResult {
	var (
		mu      sync.Mutex
		res     loadResult
		wg      sync.WaitGroup
		slots   = make(chan struct{}, s.Concurrency)
		every   = time.Duration(float64(time.Second) / s.Rate)
		ticker  = time.NewTicker(every)
		started = time.Now()
		end     = time.NewTimer(s.Duration)
		report  <-chan time.Time
		next    int
	)
	defer ticker.Stop()
	defer end.Stop()
	if s.Report > 0 && progress != nil {
		t := time.NewTicker(s.Report)
		defer t.Stop()
		report = t.C
	}

	// Calls run on their own context so the ones in flight at the end may finish; Ctrl-C (ctx)
	// cancels them too.
	fire := func(fn string) {
		defer wg.Done()
		defer func() { <-slots }()
		t0 := time.Now()
		r, err := c.Invoke(ctx, fn, s.Payload)
		call := loadCall{Fn: fn, Start: r.StartKind, Status: "200", RTT: ms(time.Since(t0))}
		var apiErr *apiclient.APIError
		switch {
		case errors.As(err, &apiErr):
			call.Status = strconv.Itoa(apiErr.Status)
		case err != nil:
			call.Status = "error"
		}
		if call.Start == "" {
			call.Start = "-"
		}
		if ctx.Err() != nil && err != nil {
			return // cancelled by Ctrl-C: not a measurement
		}
		mu.Lock()
		res.Calls = append(res.Calls, call)
		mu.Unlock()
	}

loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-end.C:
			break loop
		case <-report:
			mu.Lock()
			line := progressLine(res.Calls, res.Skipped, time.Since(started))
			mu.Unlock()
			fmt.Fprintln(progress, line)
		case <-ticker.C:
			select {
			case slots <- struct{}{}:
				fn := s.Fns[next%len(s.Fns)]
				next++
				wg.Add(1)
				go fire(fn)
			default:
				mu.Lock()
				res.Skipped++
				mu.Unlock()
			}
		}
	}
	res.Elapsed = time.Since(started)
	wg.Wait()
	return res
}

// progressLine summarises the calls so far.
func progressLine(calls []loadCall, skipped int, since time.Duration) string {
	ok, warm := 0, 0
	var rtt []float64
	for _, c := range calls {
		if c.Status == "200" {
			ok++
		}
		if c.Start == "warm" {
			warm++
		}
		rtt = append(rtt, c.RTT)
	}
	sort.Float64s(rtt)
	return fmt.Sprintf("%5.0fs  calls %d  ok %d  warm %d  skipped %d  p50 %.1f ms  p99 %.1f ms",
		since.Seconds(), len(calls), ok, warm, skipped, percentile(rtt, 50), percentile(rtt, 99))
}

// loadTable formats the results: round trip per start kind, then the status counts.
func loadTable(r loadResult) string {
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "start\tcalls\tp50 ms\tp90 ms\tp99 ms\tmax ms\t")
	by := map[string][]float64{}
	var all []float64
	for _, c := range r.Calls {
		by[c.Start] = append(by[c.Start], c.RTT)
		all = append(all, c.RTT)
	}
	row := func(name string, v []float64) {
		sort.Float64s(v)
		if len(v) == 0 {
			return
		}
		fmt.Fprintf(tw, "%s\t%d\t%.1f\t%.1f\t%.1f\t%.1f\t\n", name, len(v),
			percentile(v, 50), percentile(v, 90), percentile(v, 99), v[len(v)-1])
	}
	for _, k := range []string{"warm", "restore", "boot", "-"} {
		row(k, by[k])
	}
	row("all", all)
	tw.Flush()

	status := map[string]int{}
	for _, c := range r.Calls {
		status[c.Status]++
	}
	keys := make([]string, 0, len(status))
	for k := range status {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s: %d", k, status[k]))
	}
	rate := 0.0
	if r.Elapsed > 0 {
		rate = float64(len(r.Calls)) / r.Elapsed.Seconds()
	}
	fmt.Fprintf(&b, "\nstatus  %s\nskipped %d (all --concurrency slots busy)\nrate    %.1f calls/s over %.1f s\n",
		strings.Join(parts, ", "), r.Skipped, rate, r.Elapsed.Seconds())
	return b.String()
}

// runBenchLoad handles `beevmd bench load`.
func runBenchLoad(args []string) int {
	fs := flag.NewFlagSet("bench load", flag.ExitOnError)
	fns := fs.String("fn", "", "deployed function(s) to call, comma-separated (round-robin)")
	rate := fs.Float64("rate", 20, "calls per second, over all functions")
	duration := fs.Duration("duration", time.Minute, "how long to send calls")
	conc := fs.Int("concurrency", 8, "most calls in flight at once (more = skipped and counted)")
	payload := fs.String("payload", `{"name":"load"}`, "JSON payload of every call")
	url := fs.String("url", envOr("BEEVM_URL", "http://127.0.0.1:8080"), "server address (env BEEVM_URL)")
	out := fs.String("out", "", "also write the results to <out>/load-<time>.md (e.g. bench/results)")
	_ = fs.Parse(args)

	key := os.Getenv("BEEVM_API_KEY")
	s := loadSettings{Rate: *rate, Duration: *duration, Concurrency: *conc, Payload: []byte(*payload), Report: 5 * time.Second}
	for _, f := range strings.Split(*fns, ",") {
		if f = strings.TrimSpace(f); f != "" {
			s.Fns = append(s.Fns, f)
		}
	}
	switch {
	case len(s.Fns) == 0:
		fmt.Fprintln(os.Stderr, "usage: beevmd bench load --fn NAME[,NAME...] [--rate 20] [--duration 1m] [--concurrency 8] [--payload JSON] [--url U] [--out DIR]")
		return 2
	case key == "":
		fmt.Fprintln(os.Stderr, "error: set BEEVM_API_KEY (a tenant's key; the functions must belong to that tenant)")
		return 2
	case s.Rate <= 0 || s.Rate > 10000 || s.Concurrency < 1 || s.Duration <= 0:
		fmt.Fprintln(os.Stderr, "error: --rate must be in (0, 10000], --concurrency >= 1, --duration > 0")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Printf("load: %s at %.1f calls/s for %s, at most %d at once, against %s (Ctrl-C stops early)\n",
		strings.Join(s.Fns, ","), s.Rate, s.Duration, s.Concurrency, *url)
	res := runLoadTest(ctx, apiclient.New(*url, key), s, os.Stdout)
	table := loadTable(res)
	fmt.Println()
	fmt.Print(table)

	if *out != "" {
		if err := os.MkdirAll(*out, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		path := filepath.Join(*out, "load-"+time.Now().Format("20060102-150405")+".md")
		md := fmt.Sprintf("# beevmd bench load\n\nFunctions: %s; rate %.1f/s; duration %s; concurrency %d; %s\n\nRound trip as seen by the caller (ms):\n\n```\n%s```\n",
			strings.Join(s.Fns, ", "), s.Rate, s.Duration, s.Concurrency, time.Now().Format(time.RFC3339), table)
		if err := os.WriteFile(path, []byte(md), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		fmt.Println("wrote", path)
	}
	return 0
}
