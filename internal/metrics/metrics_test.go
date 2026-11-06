package metrics

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/om8P/BeeVM/internal/pool"
)

func TestStatusClass(t *testing.T) {
	for code, want := range map[int]string{
		200: StatusOK, 502: StatusFunctionError, 503: StatusBusy, 499: StatusClientClosed,
		400: StatusBadRequest, 413: StatusBadRequest, 500: StatusError,
	} {
		if got := StatusClass(code); got != want {
			t.Errorf("StatusClass(%d) = %s, want %s", code, got, want)
		}
	}
}

func TestObserveInvoke(t *testing.T) {
	m := New()
	m.ObserveInvoke(Invocation{Tenant: "a", Function: "f", Runtime: "python", Status: StatusOK,
		Duration: 60 * time.Millisecond, Start: "restore", Snapshot: "runtime", BootMS: 45, UploadMS: 10, LoadMS: 30, CodeCache: "miss"})
	m.ObserveInvoke(Invocation{Tenant: "a", Function: "f", Runtime: "python", Status: StatusOK,
		Duration: time.Millisecond, Start: "warm", CodeCache: "hit"})
	m.ObserveInvoke(Invocation{Tenant: "a", Function: "f", Runtime: "python", Status: StatusBusy})

	if v := testutil.ToFloat64(m.invocations.WithLabelValues("a", "f", "python", "none", StatusBusy)); v != 1 {
		t.Errorf("busy calls = %v", v)
	}
	got := gather(t, m)
	for _, want := range []string{
		`beevm_vm_ready_seconds_count{snapshot="runtime",start="restore"} 1`, // warm calls start no VM
		`beevm_vm_ready_seconds_count{snapshot="function",start="restore"} 0`,
		`beevm_load_seconds_count{runtime="python"} 1`,
		`beevm_invoke_duration_seconds_count{runtime="python",start="restore"} 1`,
		`beevm_invoke_duration_seconds_count{runtime="python",start="warm"} 1`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in\n%s", want, got)
		}
	}
	if v := testutil.ToFloat64(m.codeFetches.WithLabelValues("miss")); v != 1 {
		t.Errorf("misses = %v", v)
	}
}

func TestFunctionSnapshotSkipsUploadAndLoad(t *testing.T) {
	m := New()
	m.ObserveInvoke(Invocation{Runtime: "node", Status: StatusOK, Start: "restore", Snapshot: "function", BootMS: 44})
	got := gather(t, m)
	if !strings.Contains(got, `beevm_load_seconds_count{runtime="node"} 0`) {
		t.Error("load observed for a function snapshot")
	}
	if !strings.Contains(got, "beevm_upload_seconds_count 0") {
		t.Error("upload observed for a function snapshot")
	}
	if !strings.Contains(got, `beevm_vm_ready_seconds_count{snapshot="function",start="restore"} 1`) {
		t.Error("function-snapshot restore not observed")
	}
}

func TestKnownSeriesStartAtZero(t *testing.T) {
	got := gather(t, New())
	for _, want := range []string{
		`beevm_vm_ready_seconds_count{snapshot="function",start="restore"} 0`,
		`beevm_vm_ready_seconds_count{snapshot="",start="boot"} 0`,
		`beevm_invoke_duration_seconds_count{runtime="node",start="warm"} 0`,
		`beevm_code_fetches_total{cache="miss"} 0`,
		`beevm_snapshot_builds_total{kind="function",result="error"} 0`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s", want)
		}
	}
}

func TestNilMetricsIsSafe(t *testing.T) {
	var m *Metrics
	m.ObserveInvoke(Invocation{Start: "warm"})
	m.ObserveRecycle("evicted")
	m.ObserveSnapshotBuild("runtime", time.Second, nil)
	m.Register(NewPoolCollector(func() []pool.VMInfo { return nil }, 1))
}

func TestRecycleAndSnapshotBuilds(t *testing.T) {
	m := New()
	m.ObserveRecycle(pool.ReasonKeepAlive)
	m.ObserveRecycle(pool.ReasonKeepAlive)
	m.ObserveSnapshotBuild("function", 500*time.Millisecond, nil)
	m.ObserveSnapshotBuild("function", time.Second, errors.New("import failed"))
	if v := testutil.ToFloat64(m.recycled.WithLabelValues("keep_alive")); v != 2 {
		t.Errorf("recycled = %v", v)
	}
	if v := testutil.ToFloat64(m.snapBuilds.WithLabelValues("function", "error")); v != 1 {
		t.Errorf("failed builds = %v", v)
	}
	if !strings.Contains(gather(t, m), `beevm_snapshot_build_seconds_count{kind="function"} 1`) {
		t.Error("failed build was timed, or the good one was not")
	}
}

// gather returns "name{labels} value" lines (histograms as name_count), simpler to search than
// the exposition format.
func gather(t *testing.T, m *Metrics) string { return gatherReg(t, m.Registry()) }

func gatherReg(t *testing.T, reg prometheus.Gatherer) string {
	t.Helper()
	var b strings.Builder
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		for _, mm := range mf.GetMetric() {
			labels := []string{}
			for _, l := range mm.GetLabel() {
				labels = append(labels, l.GetName()+`="`+l.GetValue()+`"`)
			}
			name := mf.GetName()
			lbl := ""
			if len(labels) > 0 {
				lbl = "{" + strings.Join(labels, ",") + "}"
			}
			switch {
			case mm.Histogram != nil:
				b.WriteString(name + "_count" + lbl + " " + itoa(mm.Histogram.GetSampleCount()) + "\n")
			case mm.Gauge != nil:
				b.WriteString(name + lbl + " " + ftoa(mm.Gauge.GetValue()) + "\n")
			case mm.Counter != nil:
				b.WriteString(name + lbl + " " + ftoa(mm.Counter.GetValue()) + "\n")
			}
		}
	}
	return b.String()
}

func itoa(v uint64) string  { return strconv.FormatUint(v, 10) }
func ftoa(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

// ---------------------------------------------------------------- /proc parsing and the pool collector

const smapsRollup = `55d0c0a00000-7ffd8c1e0000 ---p 00000000 00:00 0                          [rollup]
Rss:               60212 kB
Pss:               21044 kB
Pss_Anon:           3000 kB
Shared_Clean:      40000 kB
Private_Dirty:      3000 kB
`

const meminfo = `MemTotal:       16303236 kB
MemFree:         9811944 kB
MemAvailable:   12000000 kB
`

func fakeProc(t *testing.T, pids ...int) string {
	t.Helper()
	dir := t.TempDir()
	for _, pid := range pids {
		p := filepath.Join(dir, itoa(uint64(pid)))
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "smaps_rollup"), []byte(smapsRollup), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "meminfo"), []byte(meminfo), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestReadSmapsRollupAndMeminfo(t *testing.T) {
	dir := fakeProc(t, 42)
	m, err := readSmapsRollup(filepath.Join(dir, "42", "smaps_rollup"))
	if err != nil || m.RSS != 60212*1024 || m.PSS != 21044*1024 {
		t.Errorf("smaps_rollup = %+v, %v", m, err)
	}
	total, avail, err := readMeminfo(filepath.Join(dir, "meminfo"))
	if err != nil || total != 16303236*1024 || avail != 12000000*1024 {
		t.Errorf("meminfo = %d %d %v", total, avail, err)
	}
}

func TestPoolCollector(t *testing.T) {
	vms := []pool.VMInfo{
		{ID: "a", State: pool.Idle, PID: 11},
		{ID: "b", State: pool.Idle, PID: 12},
		{ID: "c", State: pool.Busy, PID: 13},
		{ID: "pending-1", State: pool.Restoring}, // no process yet: counted, not measured
		{ID: "d", State: pool.Idle, PID: 99},     // exited a moment ago: no /proc entry
	}
	c := NewPoolCollector(func() []pool.VMInfo { return vms }, 8)
	c.Proc = fakeProc(t, 11, 12, 13)
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(c)

	want := `
# HELP beevm_vms VMs on this node, by warm-pool state.
# TYPE beevm_vms gauge
beevm_vms{state="busy"} 1
beevm_vms{state="idle"} 3
beevm_vms{state="recycling"} 0
beevm_vms{state="restoring"} 1
# HELP beevm_vms_max Most VMs this node runs at once (--max-vms).
# TYPE beevm_vms_max gauge
beevm_vms_max 8
# HELP beevm_vm_memory_measured_vms VMs whose memory could be read, per state (divide the memory sums by this).
# TYPE beevm_vm_memory_measured_vms gauge
beevm_vm_memory_measured_vms{state="busy"} 1
beevm_vm_memory_measured_vms{state="idle"} 2
beevm_vm_memory_measured_vms{state="recycling"} 0
beevm_vm_memory_measured_vms{state="restoring"} 0
# HELP beevm_node_memory_available_bytes Memory available for new VMs on this node (MemAvailable).
# TYPE beevm_node_memory_available_bytes gauge
beevm_node_memory_available_bytes 1.2288e+10
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want),
		"beevm_vms", "beevm_vms_max", "beevm_vm_memory_measured_vms", "beevm_node_memory_available_bytes"); err != nil {
		t.Error(err)
	}
	if !strings.Contains(gatherReg(t, reg), `beevm_vm_memory_pss_bytes{state="idle"} `+ftoa(2*21044*1024)) {
		t.Errorf("idle PSS sum wrong:\n%s", gatherReg(t, reg))
	}
}
