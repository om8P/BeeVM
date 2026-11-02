package vmm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

// pinProcess limits every thread of a process to the given host CPUs (its "CPU affinity").
// Threads it creates afterwards inherit the limit.
func pinProcess(pid int, cpus []int) error {
	var set unix.CPUSet
	for _, c := range cpus {
		if c < 0 || c >= 1024 {
			return fmt.Errorf("pin firecracker: bad CPU number %d", c)
		}
		set.Set(c)
	}
	tids := []int{pid}
	if entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid)); err == nil {
		tids = tids[:0]
		for _, e := range entries {
			if tid, err := strconv.Atoi(e.Name()); err == nil {
				tids = append(tids, tid)
			}
		}
	}
	for _, tid := range tids {
		if err := unix.SchedSetaffinity(tid, &set); err != nil {
			return fmt.Errorf("pin firecracker to CPUs %v: %w", cpus, err)
		}
	}
	return nil
}

// FirecrackerLoadTime asks Firecracker how long its snapshot load took, measured inside
// Firecracker (without our HTTP request around it). It only works for a VM restored with
// Config.FirecrackerMetrics. Call it after measuring everything else: it costs a request.
func (v *VM) FirecrackerLoadTime(ctx context.Context) (time.Duration, error) {
	if !v.cfg.FirecrackerMetrics {
		return 0, errors.New("firecracker metrics are not enabled for this vm")
	}
	api := newFCAPI(v.cfg.SocketPath())
	defer api.close()
	if err := api.put(ctx, "/actions", map[string]string{"action_type": "FlushMetrics"}); err != nil {
		return 0, err
	}
	return loadTimeFromMetrics(v.cfg.MetricsPath())
}

// loadTimeFromMetrics reads Firecracker's metrics file (one JSON object per line, one line
// per flush) and returns the snapshot load latency it reported.
func loadTimeFromMetrics(path string) (time.Duration, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var m struct {
			Latencies struct {
				LoadSnapshot int64 `json:"load_snapshot"`
			} `json:"latencies_us"`
		}
		if json.Unmarshal(sc.Bytes(), &m) == nil && m.Latencies.LoadSnapshot > 0 {
			return time.Duration(m.Latencies.LoadSnapshot) * time.Microsecond, nil
		}
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, errors.New("firecracker metrics have no load_snapshot latency")
}
