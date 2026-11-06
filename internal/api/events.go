package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/om8P/BeeVM/internal/apitypes"
	"github.com/om8P/BeeVM/internal/events"
	"github.com/om8P/BeeVM/internal/metrics"
	"github.com/om8P/BeeVM/internal/pool"
	"github.com/om8P/BeeVM/internal/runner"
)

// pingEvery is how often an idle event stream sends a comment line, so proxies and browsers
// do not close it as dead.
var pingEvery = 15 * time.Second

// VMEvents returns a pool.Config.OnStateChange hook that publishes every VM state change to
// hub (as events.TypeVM, with the pool.StateChange as data). A nil hub gives nil (no hook).
func VMEvents(hub *events.Hub) func(pool.StateChange) {
	if hub == nil {
		return nil
	}
	return func(c pool.StateChange) {
		hub.Publish(events.Event{Type: events.TypeVM, TenantID: c.VM.TenantID, Data: c})
	}
}

// publishInvoke sends a finished call to the event stream (if there is one).
func (s *Server) publishInvoke(info *reqInfo, status int, d time.Duration) {
	if s.Events == nil || info.tenantID == 0 {
		return
	}
	ev := apitypes.InvokeEvent{
		Function: info.function, Runtime: info.runtime, Status: status, VMID: info.vmID,
		DurationMS: float64(d.Microseconds()) / 1000, At: time.Now().UTC(),
	}
	if res := info.result; res != nil {
		ev.Start, ev.Snapshot = res.StartKind, res.Snapshot
		ev.BootMS, ev.UploadMS, ev.LoadMS, ev.FunctionMS = res.BootMS, res.UploadMS, res.LoadMS, res.FunctionMS
	}
	s.Events.Publish(events.Event{Type: events.TypeInvoke, TenantID: info.tenantID, Data: ev})
}

// streamEvents is a Server-Sent Events stream of the caller's live changes.
//
//	GET /api/v1/events    (Accept: text/event-stream)
//
// SSE is plain HTTP: the answer never ends, and each message is a few "field: value" lines
// followed by an empty line. Messages: "hello" (the caller's VMs right now; first, and again
// whenever the stream fell behind and dropped events), "vm" (one VM changed state), "invoke"
// (a call finished). Lines starting with ":" are comments (keep-alive pings).
func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	rc := http.NewResponseController(w)

	// Subscribe before reading the current state, so nothing that happens in between is lost.
	// (An event may repeat what hello already shows; applying it again changes nothing.)
	sub := s.Events.Subscribe(tenant.ID)
	defer s.Events.Cancel(sub)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // tell proxies like nginx not to buffer the stream
	w.WriteHeader(http.StatusOK)

	names := map[int64]string{} // function ID -> name, for this tenant
	loadNames := func() {
		fns, err := s.Registry.ListFunctions(tenant.ID)
		if err != nil {
			s.Logger.Warn("event stream: list functions", "error", err)
			return
		}
		for _, f := range fns {
			names[f.ID] = f.Name
		}
	}
	nameOf := func(id int64) string {
		if n, ok := names[id]; ok {
			return n
		}
		loadNames() // a function deployed after the stream started
		return names[id]
	}

	send := func(event string, data any) bool {
		b, err := json.Marshal(data)
		if err != nil {
			s.Logger.Error("event stream: encode", "event", event, "error", err)
			return true
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return false // the client went away
		}
		return rc.Flush() == nil
	}
	hello := func() bool {
		loadNames()
		out := apitypes.Hello{VMs: []apitypes.VM{}}
		if l, ok := s.Runner.(runner.VMLister); ok {
			for _, v := range l.VMs(tenant.ID) {
				out.VMs = append(out.VMs, toAPIVM(v, nameOf(v.FunctionID)))
			}
		}
		out.MaxVMs = s.maxVMs()
		return send(apitypes.EventHello, out)
	}

	if !hello() {
		return
	}
	ping := time.NewTicker(pingEvery)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		case ev, ok := <-sub.C:
			if !ok {
				return // the server is shutting down
			}
			okSend := true
			switch d := ev.Data.(type) {
			case pool.StateChange:
				vm := toAPIVM(d.VM, nameOf(d.VM.FunctionID))
				vm.PrevID = d.PrevID
				okSend = send(apitypes.EventVM, vm)
			case apitypes.InvokeEvent:
				okSend = send(apitypes.EventInvoke, d)
			}
			// Fell behind and events were dropped: send the full picture again.
			if okSend && s.Events.Lagged(sub) {
				okSend = hello()
			}
			if !okSend {
				return
			}
		}
	}
}

// toAPIVM converts a pool VM for the API.
func toAPIVM(v pool.VMInfo, function string) apitypes.VM {
	return apitypes.VM{ID: v.ID, Function: function, State: v.State.String(),
		Since: v.Since, LastUsed: v.LastUsed, Calls: v.Calls, Addr: v.Addr}
}

// maxVMs is the node's VM limit, or 0 without a warm pool.
func (s *Server) maxVMs() int {
	if m, ok := s.Runner.(interface{ MaxVMs() int }); ok {
		return m.MaxVMs()
	}
	return 0
}

// readVMMemory and readNodeMemory are variables so tests can fake /proc.
var (
	readVMMemory   = metrics.ReadProcessMemory
	readNodeMemory = metrics.ReadNodeMemory
)

// nodeInfo shows how full this node is: VM counts per state (all tenants, numbers only) and
// memory, for the dashboard's density counter.
//
//	GET /api/v1/node
func (s *Server) nodeInfo(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	out := apitypes.Node{MaxVMs: s.maxVMs(), States: map[string]int{}}
	for _, st := range []pool.State{pool.Restoring, pool.Idle, pool.Busy, pool.Recycling} {
		out.States[st.String()] = 0
	}
	if l, ok := s.Runner.(runner.VMLister); ok {
		for _, v := range l.VMs(0) {
			out.VMs++
			if v.TenantID == tenant.ID {
				out.TenantVMs++
			}
			out.States[v.State.String()]++
			if v.PID <= 0 {
				continue
			}
			// A VM can exit between listing and reading: then it is just not measured.
			if m, err := readVMMemory(v.PID); err == nil && m.RSS > 0 {
				out.MeasuredVMs++
				out.PSSBytes += m.PSS
				out.RSSBytes += m.RSS
			}
		}
	}
	if total, avail, err := readNodeMemory(); err == nil {
		out.MemTotalBytes, out.MemAvailableBytes = total, avail
	}
	writeJSON(w, http.StatusOK, out)
}
