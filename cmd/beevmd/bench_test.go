package main

import (
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
