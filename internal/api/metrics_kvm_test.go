//go:build kvm

package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/om8P/BeeVM/internal/apitypes"
	"github.com/om8P/BeeVM/internal/metrics"
	"github.com/om8P/BeeVM/internal/pool"
	"github.com/om8P/BeeVM/internal/runner"
	"github.com/om8P/BeeVM/internal/snapshot"
	"github.com/om8P/BeeVM/internal/vmm"
)

// End to end with metrics: go test -tags kvm -v -run E2EMetrics ./internal/api/
//
// A real warm-pool server with metrics on: one call restores a VM, the next is warm; /metrics
// must count both, show one idle VM, and read its memory from /proc (PSS > 0 and <= RSS).
func TestE2EMetrics(t *testing.T) {
	m := metrics.New()
	var w *runner.WarmRunner
	var snaps *snapshot.Manager
	c, url, vmBase := e2eServerDeps(t, func(base vmm.Config) runner.Runner {
		s, err := snapshot.New(t.TempDir(), base, t.Logf)
		if err != nil {
			t.Fatal(err)
		}
		s.SetObserver(m.ObserveSnapshotBuild)
		snaps = s
		w = runner.NewWarmRunner(base, s, pool.Config{MaxVMs: 2, KeepAlive: time.Minute, QueueWait: time.Minute,
			OnRecycle: m.ObserveRecycle}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		m.Register(metrics.NewPoolCollector(func() []pool.VMInfo { return w.VMs(0) }, w.MaxVMs()))
		return w
	}, func(d *Deps) { d.Metrics = m })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := snaps.Ensure(ctx, 128, 1); err != nil {
		t.Fatal(err)
	}

	deployFile(t, c, "hello", buildFn(t, "hello-go-binary"), apitypes.FunctionConfig{})
	for i, want := range []string{"restore", "warm"} {
		res, err := c.Invoke(ctx, "hello", []byte(`{"name":"metrics"}`))
		if err != nil {
			t.Fatal(err)
		}
		if res.StartKind != want {
			t.Fatalf("call %d started %q, want %q", i, res.StartKind, want)
		}
	}

	body := scrapeURL(t, url)
	for _, want := range []string{
		`beevm_invocations_total{function="hello",runtime="binary",start="restore",status="ok",tenant="alice"} 1`,
		`beevm_invocations_total{function="hello",runtime="binary",start="warm",status="ok",tenant="alice"} 1`,
		`beevm_vms{state="idle"} 1`,
		`beevm_vms_max 2`,
		`beevm_vm_memory_measured_vms{state="idle"} 1`,
		`beevm_snapshot_builds_total{kind="runtime",result="ok"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics is missing %s", want)
		}
	}
	// The function snapshot is built in the background after the deploy, so the first VM came
	// from it or from the runtime snapshot, whichever was ready.
	if !regexp.MustCompile(`(?m)^beevm_vm_ready_seconds_count\{snapshot="(runtime|function)",start="restore"\} 1$`).MatchString(body) {
		t.Error("/metrics has no restored VM in beevm_vm_ready_seconds")
	}
	rss, pss := value(t, body, `beevm_vm_memory_rss_bytes{state="idle"}`), value(t, body, `beevm_vm_memory_pss_bytes{state="idle"}`)
	t.Logf("idle VM: RSS %.1f MiB, PSS %.1f MiB", rss/(1<<20), pss/(1<<20))
	if pss <= 0 || pss > rss {
		t.Errorf("PSS %.0f, RSS %.0f: want 0 < PSS <= RSS", pss, rss)
	}
	if value(t, body, "beevm_node_memory_available_bytes") <= 0 {
		t.Error("no node memory")
	}

	_ = w.Close()
	if !strings.Contains(scrapeURL(t, url), `beevm_vm_recycled_total{reason="closing"} 1`) {
		t.Error("closing the pool was not counted")
	}
	noVMLeft(t, vmBase)
}

func scrapeURL(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// value finds "<series> <number>" in a /metrics body.
func value(t *testing.T, body, series string) float64 {
	t.Helper()
	m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(series) + ` (\S+)$`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no %s in /metrics", series)
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
