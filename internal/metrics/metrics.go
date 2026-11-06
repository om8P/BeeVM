// Package metrics is BeeVM's Observability component: Prometheus metrics for call latency
// (warm, snapshot restore, boot), the warm pool, VM density and memory per VM.
//
// Prometheus works by pulling: every few seconds it fetches GET /metrics, a plain-text list of
// numbers, and stores them as time series. Three kinds of numbers are used here:
//   - a counter only goes up (calls so far); Prometheus turns it into a rate (calls per second),
//   - a gauge goes up and down (VMs right now),
//   - a histogram counts observations in buckets (how many calls took <= 1 ms, <= 2 ms, ...);
//     Prometheus estimates percentiles from them with histogram_quantile.
//
// Every distinct set of label values is its own time series, so labels only take values from a
// small, known set (start kinds, runtimes, states) or from deployed functions, never from raw
// user input such as a URL.
//
// The package uses its own registry instead of the global one, so tests (and two servers in one
// process) do not see each other's numbers.
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// LatencyBuckets are the histogram bucket bounds in seconds: fine steps around the 50 ms
// cold-start target and the ~1 ms warm path, coarse ones up to a full boot.
var LatencyBuckets = []float64{
	.0005, .001, .002, .005, .01, .02, .03, .04, .05, .06, .075, .1, .15, .2, .3, .5, 1, 2, 5,
}

// Status classes for beevm_invocations_total.
const (
	StatusOK            = "ok"             // 200
	StatusFunctionError = "function_error" // 502: the user's function failed
	StatusBusy          = "busy"           // 503: no free VM slot (or runtime missing)
	StatusBadRequest    = "bad_request"    // other 4xx
	StatusClientClosed  = "client_closed"  // 499: the caller hung up
	StatusError         = "error"          // other 5xx: BeeVM itself failed
)

// StatusClass maps an HTTP status code of /invoke to a status label.
func StatusClass(code int) string {
	switch {
	case code >= 200 && code < 300:
		return StatusOK
	case code == http.StatusBadGateway:
		return StatusFunctionError
	case code == http.StatusServiceUnavailable:
		return StatusBusy
	case code == 499:
		return StatusClientClosed
	case code >= 400 && code < 500:
		return StatusBadRequest
	default:
		return StatusError
	}
}

// Invocation is one finished /invoke call, as the API saw it.
type Invocation struct {
	Tenant   string
	Function string
	Runtime  string // binary, python, node
	Status   string // a Status* class
	Duration time.Duration

	// Filled when a VM ran the call (Start != ""): what the runner reported.
	Start      string // warm, restore, boot
	Snapshot   string // restore only: function or runtime
	BootMS     int64  // until the VM's agent was ready
	UploadMS   int64
	LoadMS     int64
	FunctionMS int64
	CodeCache  string // hit or miss ("" when the code was not fetched)
}

// Metrics holds every BeeVM metric. A nil *Metrics is valid and records nothing.
type Metrics struct {
	reg *prometheus.Registry

	invocations   *prometheus.CounterVec
	invokeSeconds *prometheus.HistogramVec
	readySeconds  *prometheus.HistogramVec
	uploadSeconds prometheus.Histogram
	loadSeconds   *prometheus.HistogramVec
	fnSeconds     *prometheus.HistogramVec
	codeFetches   *prometheus.CounterVec
	recycled      *prometheus.CounterVec
	snapBuilds    *prometheus.CounterVec
	snapSeconds   *prometheus.HistogramVec
}

// New creates the metrics, plus Go runtime and process metrics of beevmd itself.
func New() *Metrics {
	hist := func(name, help string, labels ...string) *prometheus.HistogramVec {
		return prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "beevm", Name: name, Help: help, Buckets: LatencyBuckets,
		}, labels)
	}
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		invocations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "beevm", Name: "invocations_total",
			Help: "Calls of deployed functions, by how their VM started (warm, restore, boot; none = no VM) and outcome.",
		}, []string{"tenant", "function", "runtime", "start", "status"}),
		invokeSeconds: hist("invoke_duration_seconds",
			"Whole /invoke request inside the server, for calls a VM ran, by how the VM started.", "start", "runtime"),
		readySeconds: hist("vm_ready_seconds",
			"Starting a new VM for a call until its agent answered: boot, or restore from a runtime or function snapshot (prewarmed VMs are not included).", "start", "snapshot"),
		uploadSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: "beevm", Name: "upload_seconds", Buckets: LatencyBuckets,
			Help: "Sending a function's code into a new VM (only when the code was not already inside).",
		}),
		loadSeconds: hist("load_seconds",
			"Python/Node: starting the runtime and importing the function's code in a new VM.", "runtime"),
		fnSeconds: hist("function_seconds", "The function itself, as the guest agent measured it.", "runtime"),
		codeFetches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "beevm", Name: "code_fetches_total",
			Help: "Code lookups for calls: hit = on this node, miss = downloaded from the artifact store.",
		}, []string{"cache"}),
		recycled: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "beevm", Name: "vm_recycled_total",
			Help: "Warm-pool VMs thrown away, by reason.",
		}, []string{"reason"}),
		snapBuilds: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "beevm", Name: "snapshot_builds_total",
			Help: "Snapshot builds, by kind (runtime, function) and result (ok, error).",
		}, []string{"kind", "result"}),
		snapSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "beevm", Name: "snapshot_build_seconds",
			Help:    "Time to build a snapshot, by kind.",
			Buckets: []float64{.1, .25, .5, 1, 2, 3, 5, 10, 30, 60},
		}, []string{"kind"}),
	}
	// Create the series we know will exist, at 0. Prometheus computes rate() and increase() from
	// the difference between scrapes, so a series that first appears already at 1 loses that
	// first observation; for rare events such as cold starts that would hide real data.
	for _, start := range []string{"warm", "restore", "boot"} {
		for _, rt := range []string{"binary", "python", "node"} {
			m.invokeSeconds.WithLabelValues(start, rt)
		}
	}
	for _, rs := range [][2]string{{"restore", "function"}, {"restore", "runtime"}, {"boot", ""}} {
		m.readySeconds.WithLabelValues(rs[0], rs[1])
	}
	for _, rt := range []string{"python", "node"} {
		m.loadSeconds.WithLabelValues(rt)
	}
	for _, rt := range []string{"binary", "python", "node"} {
		m.fnSeconds.WithLabelValues(rt)
	}
	for _, c := range []string{"hit", "miss"} {
		m.codeFetches.WithLabelValues(c)
	}
	for _, kind := range []string{"runtime", "function"} {
		m.snapBuilds.WithLabelValues(kind, "ok")
		m.snapBuilds.WithLabelValues(kind, "error")
		m.snapSeconds.WithLabelValues(kind)
	}
	m.reg.MustRegister(m.invocations, m.invokeSeconds, m.readySeconds, m.uploadSeconds, m.loadSeconds,
		m.fnSeconds, m.codeFetches, m.recycled, m.snapBuilds, m.snapSeconds,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}

// Registry is where extra collectors go (and where tests read values).
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

// Handler serves the metrics in Prometheus' text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{Registry: m.reg})
}

func seconds(ms int64) float64 { return float64(ms) / 1000 }

// ObserveInvoke records one /invoke call.
func (m *Metrics) ObserveInvoke(c Invocation) {
	if m == nil {
		return
	}
	start := c.Start
	if start == "" {
		start = "none"
	}
	m.invocations.WithLabelValues(c.Tenant, c.Function, c.Runtime, start, c.Status).Inc()
	if c.CodeCache != "" {
		m.codeFetches.WithLabelValues(c.CodeCache).Inc()
	}
	if c.Start == "" {
		return // no VM ran it: busy, bad request, or a server error before the VM
	}
	m.invokeSeconds.WithLabelValues(c.Start, c.Runtime).Observe(c.Duration.Seconds())
	m.fnSeconds.WithLabelValues(c.Runtime).Observe(seconds(c.FunctionMS))
	if c.Start == "warm" {
		return // the VM existed already: nothing was started, uploaded or loaded
	}
	m.readySeconds.WithLabelValues(c.Start, c.Snapshot).Observe(seconds(c.BootMS))
	if c.Snapshot != "function" { // a function snapshot has the code inside already
		m.uploadSeconds.Observe(seconds(c.UploadMS))
		if c.Runtime == "python" || c.Runtime == "node" {
			m.loadSeconds.WithLabelValues(c.Runtime).Observe(seconds(c.LoadMS))
		}
	}
}

// ObserveRecycle counts a warm-pool VM being thrown away. It is called with the pool's lock
// held, so it only increments a counter.
func (m *Metrics) ObserveRecycle(reason string) {
	if m == nil {
		return
	}
	m.recycled.WithLabelValues(reason).Inc()
}

// ObserveSnapshotBuild records one finished snapshot build.
func (m *Metrics) ObserveSnapshotBuild(kind string, d time.Duration, err error) {
	if m == nil {
		return
	}
	result := "ok"
	if err != nil {
		result = "error"
	}
	m.snapBuilds.WithLabelValues(kind, result).Inc()
	if err == nil {
		m.snapSeconds.WithLabelValues(kind).Observe(d.Seconds())
	}
}
