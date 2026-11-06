package metrics

import (
	"path/filepath"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/om8P/BeeVM/internal/pool"
)

// poolStates are the states reported by beevm_vms (Destroyed VMs are not listed by the pool).
var poolStates = []pool.State{pool.Restoring, pool.Idle, pool.Busy, pool.Recycling}

// PoolCollector reports the warm pool and the node's density at scrape time. Instead of keeping
// gauges up to date on every change, it asks the pool for its VM list whenever Prometheus
// scrapes, and reads each Firecracker process's memory from /proc right then.
type PoolCollector struct {
	VMs    func() []pool.VMInfo // all VMs of the node (pool.Stats(0))
	MaxVMs int
	Proc   string // "/proc" (tests use a fake folder)

	vms, max, rss, pss, measured, memTotal, memAvail *prometheus.Desc
}

// NewPoolCollector creates a collector for a pool.
func NewPoolCollector(vms func() []pool.VMInfo, maxVMs int) *PoolCollector {
	d := func(name, help string, labels ...string) *prometheus.Desc {
		return prometheus.NewDesc("beevm_"+name, help, labels, nil)
	}
	return &PoolCollector{
		VMs: vms, MaxVMs: maxVMs, Proc: "/proc",
		vms:      d("vms", "VMs on this node, by warm-pool state.", "state"),
		max:      d("vms_max", "Most VMs this node runs at once (--max-vms)."),
		rss:      d("vm_memory_rss_bytes", "Resident memory of the VMs' Firecracker processes, summed per state (shared pages counted in every VM).", "state"),
		pss:      d("vm_memory_pss_bytes", "Proportional memory of the VMs' Firecracker processes, summed per state (shared snapshot pages split between VMs).", "state"),
		measured: d("vm_memory_measured_vms", "VMs whose memory could be read, per state (divide the memory sums by this).", "state"),
		memTotal: d("node_memory_total_bytes", "Memory of this node (MemTotal)."),
		memAvail: d("node_memory_available_bytes", "Memory available for new VMs on this node (MemAvailable)."),
	}
}

// Register adds the pool collector to m. Safe on a nil m.
func (m *Metrics) Register(c prometheus.Collector) {
	if m == nil {
		return
	}
	m.reg.MustRegister(c)
}

// Describe implements prometheus.Collector.
func (c *PoolCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.vms, c.max, c.rss, c.pss, c.measured, c.memTotal, c.memAvail} {
		ch <- d
	}
}

// Collect implements prometheus.Collector.
func (c *PoolCollector) Collect(ch chan<- prometheus.Metric) {
	type sums struct {
		n, measured int
		rss, pss    uint64
	}
	by := map[pool.State]*sums{}
	for _, s := range poolStates {
		by[s] = &sums{}
	}
	for _, v := range c.VMs() {
		s, ok := by[v.State]
		if !ok {
			continue
		}
		s.n++
		if v.PID <= 0 {
			continue // still being created, or a fake VM
		}
		// A VM can exit between listing and reading: then it is just not measured.
		mem, err := readSmapsRollup(filepath.Join(c.Proc, strconv.Itoa(v.PID), "smaps_rollup"))
		if err != nil || mem.RSS == 0 {
			continue
		}
		s.measured++
		s.rss += mem.RSS
		s.pss += mem.PSS
	}
	for _, st := range poolStates {
		s, label := by[st], st.String()
		ch <- prometheus.MustNewConstMetric(c.vms, prometheus.GaugeValue, float64(s.n), label)
		ch <- prometheus.MustNewConstMetric(c.rss, prometheus.GaugeValue, float64(s.rss), label)
		ch <- prometheus.MustNewConstMetric(c.pss, prometheus.GaugeValue, float64(s.pss), label)
		ch <- prometheus.MustNewConstMetric(c.measured, prometheus.GaugeValue, float64(s.measured), label)
	}
	ch <- prometheus.MustNewConstMetric(c.max, prometheus.GaugeValue, float64(c.MaxVMs))
	if total, avail, err := readMeminfo(filepath.Join(c.Proc, "meminfo")); err == nil && total > 0 {
		ch <- prometheus.MustNewConstMetric(c.memTotal, prometheus.GaugeValue, float64(total))
		ch <- prometheus.MustNewConstMetric(c.memAvail, prometheus.GaugeValue, float64(avail))
	}
}
