package api

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/om8P/BeeVM/internal/apitypes"
	"github.com/om8P/BeeVM/internal/events"
	"github.com/om8P/BeeVM/internal/metrics"
	"github.com/om8P/BeeVM/internal/pool"
	"github.com/om8P/BeeVM/internal/runner"
)

// sseMsg is one Server-Sent Events message.
type sseMsg struct {
	event string
	data  string
}

// sseStream reads messages from an open event stream in the background.
type sseStream struct {
	resp *http.Response
	msgs chan sseMsg
}

func (e *env) openEvents(key string) *sseStream {
	e.t.Helper()
	return openSSE(e.t, e.ts.URL, key)
}

// openSSE opens GET /api/v1/events on the server at baseURL.
func openSSE(t *testing.T, baseURL, key string) *sseStream {
	t.Helper()
	req, _ := http.NewRequest("GET", baseURL+"/api/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("events status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	s := &sseStream{resp: resp, msgs: make(chan sseMsg, 256)}
	t.Cleanup(func() { resp.Body.Close() })
	go func() {
		defer close(s.msgs)
		sc := bufio.NewScanner(resp.Body)
		var m sseMsg
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if m.event != "" {
					s.msgs <- m
				}
				m = sseMsg{}
			case strings.HasPrefix(line, ":"): // comment
			case strings.HasPrefix(line, "event: "):
				m.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				m.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return s
}

func (s *sseStream) next(t *testing.T) sseMsg {
	t.Helper()
	select {
	case m, ok := <-s.msgs:
		if !ok {
			t.Fatal("stream ended")
		}
		return m
	case <-time.After(10 * time.Second):
		t.Fatal("no event")
	}
	return sseMsg{}
}

func sseData[T any](t *testing.T, m sseMsg, event string) T {
	t.Helper()
	if m.event != event {
		t.Fatalf("got event %q (%s), want %q", m.event, m.data, event)
	}
	var v T
	if err := json.Unmarshal([]byte(m.data), &v); err != nil {
		t.Fatalf("decode %s: %v", m.data, err)
	}
	return v
}

func newEventsEnv(t *testing.T) (*env, *fakePoolRunner, *events.Hub) {
	fr := &fakeRunner{result: runner.Result{Output: json.RawMessage(`{}`), VMID: "vm-1", StartKind: runner.StartRestore,
		Snapshot: "function", BootMS: 45, FunctionMS: 3}}
	pr := &fakePoolRunner{fakeRunner: fr, vms: map[int64][]pool.VMInfo{}}
	hub := events.New(0)
	e := newEnvDeps(t, pr, fr, func(d *Deps) { d.Events = hub })
	return e, pr, hub
}

func TestEventsNeedAKeyAndAHub(t *testing.T) {
	e, _, _ := newEventsEnv(t)
	status(t, e.do("GET", "/api/v1/events", "", nil, ""), 401)
	status(t, e.do("GET", "/api/v1/events", "bvm_wrong", nil, ""), 401)

	off := newEnv(t) // no hub: no stream
	status(t, off.do("GET", "/api/v1/events", off.tenant("alice"), nil, ""), 404)
}

func TestEventsHelloVMAndInvoke(t *testing.T) {
	e, pr, hub := newEventsEnv(t)
	alice, bob := e.tenant("alice"), e.tenant("bob")
	status(t, e.deploy(alice, "hello", elf("v1"), ""), 201)
	ta, _ := e.reg.GetTenant("alice")
	tb, _ := e.reg.GetTenant("bob")
	f, _ := e.reg.GetFunction(ta.ID, "hello")
	pr.vms[ta.ID] = []pool.VMInfo{{ID: "vm-a", TenantID: ta.ID, FunctionID: f.ID, State: pool.Idle, Calls: 2}}

	as, bs := e.openEvents(alice), e.openEvents(bob)
	h := sseData[apitypes.Hello](t, as.next(t), apitypes.EventHello)
	if len(h.VMs) != 1 || h.VMs[0].ID != "vm-a" || h.VMs[0].Function != "hello" || h.VMs[0].State != "idle" || h.MaxVMs != 4 {
		t.Errorf("hello = %+v", h)
	}
	if hb := sseData[apitypes.Hello](t, bs.next(t), apitypes.EventHello); len(hb.VMs) != 0 {
		t.Errorf("bob's hello shows %+v", hb.VMs)
	}

	// A pool change for alice, reported through the hook serve installs.
	hook := VMEvents(hub)
	hook(pool.StateChange{VM: pool.VMInfo{ID: "vm-9", TenantID: ta.ID, FunctionID: f.ID, State: pool.Busy, Calls: 1},
		From: pool.Restoring, PrevID: "pending-3"})
	vm := sseData[apitypes.VM](t, as.next(t), apitypes.EventVM)
	if vm.ID != "vm-9" || vm.PrevID != "pending-3" || vm.State != "busy" || vm.Function != "hello" {
		t.Errorf("vm event = %+v", vm)
	}

	// A call: the invoke event carries the timings.
	status(t, e.do("POST", "/invoke/hello", alice, strings.NewReader(`{}`), ""), 200)
	inv := sseData[apitypes.InvokeEvent](t, as.next(t), apitypes.EventInvoke)
	if inv.Function != "hello" || inv.Status != 200 || inv.Start != "restore" || inv.Snapshot != "function" ||
		inv.BootMS != 45 || inv.FunctionMS != 3 || inv.VMID != "vm-1" || inv.Runtime != "binary" || inv.DurationMS <= 0 {
		t.Errorf("invoke event = %+v", inv)
	}
	// A call of an unknown function is not an event (only deployed functions are).
	status(t, e.do("POST", "/invoke/nope", alice, nil, ""), 404)

	// Bob saw none of it: his next message is his own.
	hook(pool.StateChange{VM: pool.VMInfo{ID: "vm-b", TenantID: tb.ID, FunctionID: 999, State: pool.Idle}, From: pool.Restoring})
	if vb := sseData[apitypes.VM](t, bs.next(t), apitypes.EventVM); vb.ID != "vm-b" || vb.Function != "" {
		t.Errorf("bob got %+v", vb)
	}
	select {
	case m := <-as.msgs:
		t.Errorf("alice got an extra message %+v", m)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestEventsFunctionDeployedAfterHello(t *testing.T) {
	e, _, hub := newEventsEnv(t)
	alice := e.tenant("alice")
	s := e.openEvents(alice)
	sseData[apitypes.Hello](t, s.next(t), apitypes.EventHello)
	status(t, e.deploy(alice, "late", elf("v1"), ""), 201)
	ta, _ := e.reg.GetTenant("alice")
	f, _ := e.reg.GetFunction(ta.ID, "late")
	VMEvents(hub)(pool.StateChange{VM: pool.VMInfo{ID: "vm-1", TenantID: ta.ID, FunctionID: f.ID, State: pool.Restoring}})
	if vm := sseData[apitypes.VM](t, s.next(t), apitypes.EventVM); vm.Function != "late" {
		t.Errorf("name of a new function = %q", vm.Function)
	}
}

func TestEventsEndWhenTheHubCloses(t *testing.T) {
	e, _, hub := newEventsEnv(t)
	s := e.openEvents(e.tenant("alice"))
	s.next(t) // hello
	hub.Close()
	select {
	case _, ok := <-s.msgs:
		if ok {
			t.Error("unexpected message")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not end after hub.Close")
	}
	waitUntil(t, func() bool { return hub.Subscribers() == 0 })
}

func TestEventsClientGoingAwayUnsubscribes(t *testing.T) {
	e, _, hub := newEventsEnv(t)
	s := e.openEvents(e.tenant("alice"))
	s.next(t)
	if hub.Subscribers() != 1 {
		t.Fatalf("subscribers = %d", hub.Subscribers())
	}
	s.resp.Body.Close()
	waitUntil(t, func() bool { return hub.Subscribers() == 0 })
}

func TestVMEventsWithoutHub(t *testing.T) {
	if VMEvents(nil) != nil {
		t.Error("no hub should give no hook")
	}
}

func TestNodeInfo(t *testing.T) {
	e, pr, _ := newEventsEnv(t)
	key := e.tenant("alice")
	ta, _ := e.reg.GetTenant("alice")
	pr.vms[0] = []pool.VMInfo{
		{ID: "a", TenantID: ta.ID, State: pool.Idle, PID: 11}, {ID: "b", TenantID: ta.ID, State: pool.Idle, PID: 12},
		{ID: "c", State: pool.Busy, PID: 13}, {ID: "d", State: pool.Restoring}, // no PID yet
		{ID: "e", State: pool.Idle, PID: 99}, // exited: not measured
	}
	oldVM, oldNode := readVMMemory, readNodeMemory
	t.Cleanup(func() { readVMMemory, readNodeMemory = oldVM, oldNode })
	readVMMemory = func(pid int) (metrics.ProcessMemory, error) {
		if pid == 99 {
			return metrics.ProcessMemory{}, http.ErrMissingFile
		}
		return metrics.ProcessMemory{RSS: 20 << 20, PSS: 6 << 20}, nil
	}
	readNodeMemory = func() (uint64, uint64, error) { return 8 << 30, 5 << 30, nil }

	n := decode[apitypes.Node](t, e.do("GET", "/api/v1/node", key, nil, ""))
	if n.MaxVMs != 4 || n.VMs != 5 || n.TenantVMs != 2 || n.States["idle"] != 3 || n.States["busy"] != 1 || n.States["restoring"] != 1 ||
		n.States["recycling"] != 0 || n.MeasuredVMs != 3 || n.PSSBytes != 18<<20 || n.RSSBytes != 60<<20 ||
		n.MemTotalBytes != 8<<30 || n.MemAvailableBytes != 5<<30 {
		t.Errorf("node = %+v", n)
	}
	status(t, e.do("GET", "/api/v1/node", "", nil, ""), 401)

	// Without a warm pool: zeros, not an error.
	plain := newEnv(t)
	n = decode[apitypes.Node](t, plain.do("GET", "/api/v1/node", plain.tenant("bob"), nil, ""))
	if n.MaxVMs != 0 || n.VMs != 0 || len(n.States) != 4 {
		t.Errorf("node without pool = %+v", n)
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
