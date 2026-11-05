package main

import (
	"os"
	"strings"
	"testing"
)

func TestPercentileNearestRank(t *testing.T) {
	v := make([]float64, 100)
	for i := range v {
		v[i] = float64(i + 1) // 1..100
	}
	for p, want := range map[float64]float64{50: 50, 90: 90, 99: 99, 100: 100, 0: 1} {
		if got := percentile(v, p); got != want {
			t.Errorf("p%v = %v, want %v", p, got, want)
		}
	}
	if got := percentile([]float64{7}, 99); got != 7 {
		t.Errorf("single value: %v", got)
	}
	if got := percentile(nil, 50); got != 0 {
		t.Errorf("empty: %v", got)
	}
}

func TestBenchTableShowsRestoreOnlyColumnsForRestore(t *testing.T) {
	out := benchTable([]benchRun{
		{Mode: "boot", Ready: 900, First: 990},
		{Mode: "restore", Spawn: 1, Load: 20, Ready: 35, First: 110},
	})
	loadRows := 0
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) > 1 && f[1] == "load" {
			loadRows++
			if f[0] != "restore" {
				t.Errorf("load row for mode %s", f[0])
			}
		}
	}
	if loadRows != 1 {
		t.Errorf("%d load rows, want 1 (restore only):\n%s", loadRows, out)
	}
}

func TestBenchTableWarmShowsOnlyFirstResponse(t *testing.T) {
	rows := tableRows(benchTable([]benchRun{
		{Mode: "restore", Load: 20, Agent: 10, Ready: 31, Upload: 20, Invoke: 15, First: 70},
		{Mode: "warm", First: 4},
	}))
	if !rows["warm/first response"] {
		t.Errorf("warm first response missing: %v", rows)
	}
	for _, r := range []string{"warm/vm ready", "warm/upload", "warm/invoke", "warm/load"} {
		if rows[r] {
			t.Errorf("row %s should not exist for warm", r)
		}
	}
	if !rows["restore/upload"] || !rows["restore/vm ready"] {
		t.Errorf("restore rows missing: %v", rows)
	}
}

func tableRows(out string) map[string]bool {
	rows := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) > 2 {
			name := strings.Join(f[1:len(f)-5], " ") // metric names can contain spaces
			rows[f[0]+"/"+name] = true
		}
	}
	return rows
}

func TestBenchTableLeavesOutUnusedOptionalRows(t *testing.T) {
	rows := tableRows(benchTable([]benchRun{{Mode: "restore", Load: 20, Agent: 10, Attempts: 1, Ready: 31}}))
	for _, want := range []string{"restore/load", "restore/agent", "restore/attempts (n)", "restore/vm ready"} {
		if !rows[want] {
			t.Errorf("missing row %s in %v", want, rows)
		}
	}
	for _, unwanted := range []string{"restore/fc load", "restore/connect"} {
		if rows[unwanted] {
			t.Errorf("row %s shown although no run filled it", unwanted)
		}
	}
	rows = tableRows(benchTable([]benchRun{{Mode: "restore", FCLoad: 18, Connect: 5}}))
	if !rows["restore/fc load"] || !rows["restore/connect"] || rows["restore/attempts (n)"] {
		t.Errorf("optional rows wrong: %v", rows)
	}
}

func TestParseCPUs(t *testing.T) {
	if got, err := parseCPUs(""); err != nil || got != nil {
		t.Errorf("empty: %v, %v", got, err)
	}
	if got, err := parseCPUs("0"); err != nil || len(got) != 1 || got[0] != 0 {
		t.Errorf("0: %v, %v", got, err)
	}
	for _, bad := range []string{"x", "-1", "1,,2", "100000"} {
		if _, err := parseCPUs(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestDescribeSettings(t *testing.T) {
	s := describeSettings(benchSettings{Probe: "health", ColdCache: true, Pin: []int{3}})
	for _, want := range []string{"probe health", "cold", "CPUs [3]"} {
		if !strings.Contains(s, want) {
			t.Errorf("%q missing %q", s, want)
		}
	}
	if s := describeSettings(benchSettings{Probe: "resume"}); !strings.Contains(s, "warm") || strings.Contains(s, "gap") {
		t.Errorf("defaults: %q", s)
	}
}

func TestProcessFaultsOfThisProcess(t *testing.T) {
	a := processFaults(os.Getpid())
	_ = make([]byte, 8<<20)[4<<20] // touch fresh memory: faults must not go down
	if a < 0 || processFaults(os.Getpid()) < a {
		t.Errorf("faults: %d", a)
	}
	if processFaults(-5) != -1 {
		t.Error("want -1 for a missing process")
	}
}

func TestBenchTableFunctionMode(t *testing.T) {
	rows := tableRows(benchTable([]benchRun{
		{Mode: "restore", Load: 20, Agent: 10, Ready: 31, Upload: 20, Import: 25, Invoke: 15, First: 95},
		{Mode: "function", Load: 20, Agent: 10, Ready: 31, Invoke: 3, First: 35},
		{Mode: "boot", Ready: 900, Upload: 20, First: 950},
	}))
	for _, r := range []string{"function/load", "function/vm ready", "function/invoke", "function/first response", "restore/import"} {
		if !rows[r] {
			t.Errorf("row %s missing: %v", r, rows)
		}
	}
	// The code is already inside a function snapshot; a binary has nothing to import.
	for _, r := range []string{"function/upload", "function/import", "boot/import"} {
		if rows[r] {
			t.Errorf("row %s should not exist", r)
		}
	}
}
