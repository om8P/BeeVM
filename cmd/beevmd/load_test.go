package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/om8P/BeeVM/internal/apiclient"
	"github.com/om8P/BeeVM/internal/apitypes"
)

// fakeServer answers /invoke/<fn>: "slow" takes 200 ms, "busy" is 503, the rest are warm.
func fakeServer(t *testing.T, inFlight, peak *atomic.Int32) *httptest.Server {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		switch strings.TrimPrefix(r.URL.Path, "/invoke/") {
		case "slow":
			time.Sleep(200 * time.Millisecond)
		case "busy":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(503)
			w.Write([]byte(`{"error":"too many VMs"}`))
			return
		}
		w.Header().Set(apitypes.HeaderStart, "warm")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestLoadRateAndRoundRobin(t *testing.T) {
	var inFlight, peak atomic.Int32
	ts := fakeServer(t, &inFlight, &peak)
	s := loadSettings{Fns: []string{"a", "busy"}, Rate: 100, Duration: 500 * time.Millisecond, Concurrency: 4, Payload: []byte(`{}`)}
	r := runLoadTest(context.Background(), apiclient.New(ts.URL, "k"), s, nil)

	if n := len(r.Calls); n < 35 || n > 55 {
		t.Errorf("%d calls in 0.5 s at 100/s", n)
	}
	warm, busy := 0, 0
	for _, c := range r.Calls {
		switch {
		case c.Fn == "a" && c.Start == "warm" && c.Status == "200":
			warm++
		case c.Fn == "busy" && c.Start == "-" && c.Status == "503":
			busy++
		default:
			t.Errorf("unexpected call %+v", c)
		}
	}
	if d := warm - busy; d < -1 || d > 1 {
		t.Errorf("not round-robin: %d warm, %d busy", warm, busy)
	}
	table := loadTable(r)
	for _, want := range []string{"warm", "200: ", "503: "} {
		if !strings.Contains(table, want) {
			t.Errorf("table is missing %q:\n%s", want, table)
		}
	}
}

func TestLoadConcurrencyLimitSkips(t *testing.T) {
	var inFlight, peak atomic.Int32
	ts := fakeServer(t, &inFlight, &peak)
	s := loadSettings{Fns: []string{"slow"}, Rate: 100, Duration: 300 * time.Millisecond, Concurrency: 2}
	r := runLoadTest(context.Background(), apiclient.New(ts.URL, "k"), s, nil)
	if peak.Load() > 2 {
		t.Errorf("%d calls at once, limit 2", peak.Load())
	}
	if r.Skipped < 10 {
		t.Errorf("skipped %d, want most ticks skipped while 2 slow calls run", r.Skipped)
	}
	if len(r.Calls) == 0 || inFlight.Load() != 0 {
		t.Errorf("calls %d, still in flight %d (should wait for the last calls)", len(r.Calls), inFlight.Load())
	}
}

func TestLoadStopsOnCancel(t *testing.T) {
	var inFlight, peak atomic.Int32
	ts := fakeServer(t, &inFlight, &peak)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	runLoadTest(ctx, apiclient.New(ts.URL, "k"), loadSettings{Fns: []string{"a"}, Rate: 50, Duration: time.Hour, Concurrency: 2}, nil)
	if time.Since(t0) > 2*time.Second {
		t.Error("did not stop when the context ended")
	}
}
