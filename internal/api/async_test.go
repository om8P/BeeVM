package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/om8P/BeeVM/internal/apitypes"
	"github.com/om8P/BeeVM/internal/events"
	"github.com/om8P/BeeVM/internal/runner"
)

// gateRunner answers every call, after the gate opens if there is one.
type gateRunner struct{ gate chan struct{} }

func (g *gateRunner) Run(ctx context.Context, _ runner.Request) (runner.Result, error) {
	if g.gate != nil {
		select {
		case <-g.gate:
		case <-ctx.Done():
			return runner.Result{}, ctx.Err()
		}
	}
	return runner.Result{Output: json.RawMessage(`{}`), VMID: "vm1", StartKind: runner.StartWarm}, nil
}

func asyncEnv(t *testing.T, run runner.Runner, change func(*Deps)) (*env, *events.Hub) {
	t.Helper()
	hub := events.New(64)
	t.Cleanup(hub.Close)
	e := newEnvDeps(t, run, nil, func(d *Deps) {
		d.Events = hub
		if change != nil {
			change(d)
		}
	})
	return e, hub
}

func (e *env) invokeAsync(key string) *http.Response {
	e.t.Helper()
	return e.do(http.MethodPost, "/invoke/fn?async=1", key, strings.NewReader(`{}`), "application/json")
}

func TestAsyncInvokePublishesResult(t *testing.T) {
	e, hub := asyncEnv(t, &gateRunner{}, nil)
	key := e.tenant("alice")
	e.deployOK(key, "fn", elf("a"), http.StatusCreated)
	sub := hub.Subscribe(0)
	defer hub.Cancel(sub)

	resp := e.invokeAsync(key)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d, want 202", resp.StatusCode)
	}
	id := decode[apitypes.InvokeAccepted](t, resp).RequestID
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev := <-sub.C:
			if ie, ok := ev.Data.(apitypes.InvokeEvent); ok && ie.RequestID == id {
				if ie.Status != http.StatusOK || ie.VMID != "vm1" {
					t.Fatalf("event %+v", ie)
				}
				return
			}
		case <-timeout:
			t.Fatal("no invoke event")
		}
	}
}

func TestAsyncQueueFullIs429(t *testing.T) {
	g := &gateRunner{gate: make(chan struct{})}
	defer close(g.gate)
	e, _ := asyncEnv(t, g, func(d *Deps) { d.AsyncWorkers, d.AsyncQueueSize = 1, 1 })
	key := e.tenant("alice")
	e.deployOK(key, "fn", elf("a"), http.StatusCreated)

	// At most 1 running + 1 queued; a few more calls must hit the full queue.
	got429 := false
	for range 4 {
		resp := e.invokeAsync(key)
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			got429 = true
		}
	}
	if !got429 {
		t.Fatal("no 429 from a full queue")
	}
}
