//go:build kvm

package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/om8P/BeeVM/internal/apitypes"
	"github.com/om8P/BeeVM/internal/events"
	"github.com/om8P/BeeVM/internal/pool"
	"github.com/om8P/BeeVM/internal/registry"
	"github.com/om8P/BeeVM/internal/runner"
	"github.com/om8P/BeeVM/internal/snapshot"
	"github.com/om8P/BeeVM/internal/vmm"
)

// The live event stream with a real warm pool (M11): go test -tags kvm -v -run E2EEvents ./internal/api/
func TestE2EEvents(t *testing.T) {
	hub := events.New(0)
	var w *runner.WarmRunner
	var snaps *snapshot.Manager
	var reg *registry.Registry
	c, url, vmBase := e2eServerDeps(t, func(base vmm.Config) runner.Runner {
		s, err := snapshot.New(t.TempDir(), base, t.Logf)
		if err != nil {
			t.Fatal(err)
		}
		snaps = s
		w = runner.NewWarmRunner(base, s, pool.Config{MaxVMs: 2, KeepAlive: 800 * time.Millisecond, QueueWait: time.Minute,
			OnStateChange: VMEvents(hub)}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		return w
	}, func(d *Deps) { d.Events = hub; reg = d.Registry })
	t.Cleanup(func() { _ = w.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := snaps.Ensure(ctx, 128, 1); err != nil {
		t.Fatal(err)
	}
	deployFile(t, c, "hello", buildFn(t, "hello-go-binary"), apitypes.FunctionConfig{})

	tn, _ := reg.GetTenant("alice")
	key, _, _ := reg.CreateKey(tn.ID)
	s := openSSE(t, url, key)
	if h := sseData[apitypes.Hello](t, s.next(t), apitypes.EventHello); len(h.VMs) != 0 || h.MaxVMs != 2 {
		t.Fatalf("hello = %+v", h)
	}

	for _, want := range []string{"restore", "warm"} {
		res, err := c.Invoke(ctx, "hello", []byte(`{"name":"events"}`))
		if err != nil || res.StartKind != want {
			t.Fatalf("call: %+v, %v (want %s)", res, err, want)
		}
	}

	// Collect until the VM was reaped after its keep-alive: every state of its life, in order.
	var trail []string
	var starts []string
	var vmID string
	deadline := time.After(10 * time.Second)
	for vmID == "" || !strings.HasSuffix(strings.Join(trail, " "), "destroyed") {
		select {
		case <-deadline:
			t.Fatalf("no full VM life seen; trail %v, calls %v", trail, starts)
		default:
		}
		m := s.next(t)
		switch m.event {
		case apitypes.EventVM:
			var v apitypes.VM
			if err := json.Unmarshal([]byte(m.data), &v); err != nil {
				t.Fatal(err)
			}
			if v.Function != "hello" {
				t.Errorf("vm event for %q", v.Function)
			}
			if !strings.HasPrefix(v.ID, "pending-") {
				vmID = v.ID
			}
			trail = append(trail, v.State)
		case apitypes.EventInvoke:
			var e apitypes.InvokeEvent
			if err := json.Unmarshal([]byte(m.data), &e); err != nil {
				t.Fatal(err)
			}
			if e.Status != 200 || e.Function != "hello" {
				t.Errorf("invoke event %+v", e)
			}
			if e.Start == "restore" && (e.BootMS <= 0 || e.Snapshot == "") {
				t.Errorf("restore event without cold-start timing: %+v", e)
			}
			starts = append(starts, e.Start)
		}
	}
	got := strings.Join(trail, " ")
	want := "restoring busy idle busy idle recycling destroyed"
	if got != want {
		t.Errorf("VM trail %q, want %q", got, want)
	}
	if strings.Join(starts, " ") != "restore warm" {
		t.Errorf("invoke events %v, want [restore warm]", starts)
	}
	t.Logf("vm %s: %s; calls: %v", vmID, got, starts)

	// Closing the hub ends the stream (what serve does on shutdown).
	hub.Close()
	for range s.msgs {
	}
	waitNoVMLeft(t, vmBase)
}
