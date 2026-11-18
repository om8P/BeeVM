package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/om8P/BeeVM/internal/apiclient"
	"github.com/om8P/BeeVM/internal/apitypes"
)

// fakeDensity pretends to be a node: every VM costs `cost` bytes, `cap` VMs fit in the pool.
type fakeDensity struct {
	avail, cost uint64
	cap         int
	vms         int
	asked       map[string]int
	failInvoke  bool
}

func (f *fakeDensity) Prewarm(_ context.Context, name string, n int) (apitypes.PrewarmResult, error) {
	if f.asked == nil {
		f.asked = map[string]int{}
	}
	f.asked[name] += n
	started := min(n, f.cap-f.vms)
	f.vms += started
	f.avail -= uint64(started) * f.cost
	return apitypes.PrewarmResult{Requested: n, Started: started}, nil
}

func (f *fakeDensity) Node(context.Context) (apitypes.Node, error) {
	return apitypes.Node{VMs: f.vms, States: map[string]int{"idle": f.vms}, MeasuredVMs: f.vms,
		PSSBytes: uint64(f.vms) * f.cost / 2, RSSBytes: uint64(f.vms) * f.cost, MemTotalBytes: 8 << 30, MemAvailableBytes: f.avail}, nil
}

func (f *fakeDensity) Invoke(context.Context, string, []byte) (apiclient.InvokeResult, error) {
	if f.failInvoke {
		return apiclient.InvokeResult{}, errors.New("boom")
	}
	return apiclient.InvokeResult{StartKind: "warm"}, nil
}

func settings(fns ...string) densitySettings {
	return densitySettings{Fns: fns, Step: 10, Max: 100, Reserve: 1 << 30, Probe: 4, Payload: []byte(`{}`)}
}

func TestSplitStepsSpreadsEvenly(t *testing.T) {
	got := splitSteps(10, 3)
	if got[0] != 4 || got[1] != 3 || got[2] != 3 {
		t.Errorf("splitSteps(10,3) = %v, want [4 3 3]", got)
	}
}

func TestDensityStopsAtMax(t *testing.T) {
	f := &fakeDensity{avail: 4 << 30, cost: 1 << 20, cap: 1000}
	r, err := runDensity(context.Background(), f, settings("a", "b"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Steps) != 10 || r.Steps[9].VMs != 100 || !strings.HasPrefix(r.Stop, "reached --max") {
		t.Errorf("steps %d, last %+v, stop %q", len(r.Steps), r.Steps[len(r.Steps)-1], r.Stop)
	}
	if f.asked["a"] != 50 || f.asked["b"] != 50 {
		t.Errorf("functions asked %v, want 50 each", f.asked)
	}
	if s := r.Steps[9]; s.HostPerVM != float64(1<<20) || s.PSSPerVM != float64(1<<19) || s.RSSPerVM != float64(1<<20) {
		t.Errorf("per-VM numbers %+v", s)
	}
}

func TestDensityStopsBeforeTheReserve(t *testing.T) {
	// 1.5 GiB available, 1 GiB reserve, 10 MiB per VM: room for 51 VMs.
	f := &fakeDensity{avail: 1<<30 + 512<<20, cost: 10 << 20, cap: 1000}
	s := settings("a")
	s.Max = 1000
	r, err := runDensity(context.Background(), f, s, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if r.Stop != "memory reserve reached" {
		t.Errorf("stop = %q", r.Stop)
	}
	last := r.Steps[len(r.Steps)-1]
	if last.Available < s.Reserve || last.VMs < 45 || last.VMs > 51 {
		t.Errorf("last step %+v: want memory kept above the reserve and about 51 VMs", last)
	}
	if r.Extrapolated != 51 {
		t.Errorf("extrapolated %d, want 51", r.Extrapolated)
	}
}

func TestDensityStopsWhenThePoolIsFull(t *testing.T) {
	f := &fakeDensity{avail: 4 << 30, cost: 1 << 20, cap: 25}
	r, err := runDensity(context.Background(), f, settings("a"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Stop, "only 5 of 10") || r.Steps[len(r.Steps)-1].VMs != 25 {
		t.Errorf("stop %q, steps %+v", r.Stop, r.Steps)
	}
}

func TestDensityCountsFailedProbes(t *testing.T) {
	f := &fakeDensity{avail: 4 << 30, cost: 1 << 20, cap: 1000, failInvoke: true}
	s := settings("a")
	s.Max = 10
	r, _ := runDensity(context.Background(), f, s, io.Discard)
	if r.Steps[0].ProbeFail != 4 || r.Steps[0].ProbeP50 != 0 {
		t.Errorf("step %+v, want 4 failed probes and no latency", r.Steps[0])
	}
}

func TestDensityInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &fakeDensity{avail: 4 << 30, cost: 1 << 20, cap: 1000}
	r, _ := runDensity(ctx, f, settings("a"), io.Discard)
	if r.Stop != "interrupted" || len(r.Steps) != 0 {
		t.Errorf("stop %q steps %d", r.Stop, len(r.Steps))
	}
}

func TestWriteDensity(t *testing.T) {
	f := &fakeDensity{avail: 4 << 30, cost: 1 << 20, cap: 1000}
	s := settings("a")
	s.Max, s.Settle = 20, time.Millisecond
	r, _ := runDensity(context.Background(), f, s, io.Discard)
	dir := t.TempDir()
	csvPath, mdPath, err := writeDensity(dir, "one-fn", r, s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(csvPath, "density-one-fn-") || !strings.HasSuffix(mdPath, ".md") {
		t.Errorf("paths %s %s", csvPath, mdPath)
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]uint64{"1GiB": 1 << 30, "512MiB": 512 << 20, "4096": 4096} {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := parseSize("lots"); err == nil {
		t.Error("parseSize(lots) should fail")
	}
}
