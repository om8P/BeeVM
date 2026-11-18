package api

// Async invoke, like AWS Lambda's "Event" invocation type.
//
// A synchronous call holds its HTTP connection until the function answers. An async call is
// put in an in-memory queue and answered at once with 202 and a request id; a fixed set of
// worker goroutines takes calls from the queue and runs them through the same path as a
// synchronous call (warm pool and all). The result is not returned to the caller: it is
// published as an "invoke" event (GET /api/v1/events) carrying the same request id, and it is
// counted in the metrics like any call.
//
// Why: callers that cannot keep many connections open (a browser allows ~6 per server) can
// still start many calls at once, and a burst larger than the node's VM slots waits in the
// queue instead of failing with 503 after the pool's QueueWait.
//
// Limits, so one tenant cannot fill the node's memory or starve the others:
//   - the queue holds at most AsyncQueueSize calls (all tenants), one tenant at most
//     AsyncPerTenant of them; beyond that the call is refused with 429;
//   - a call that waited longer than AsyncMaxAge is dropped (event with status 504);
//   - while no VM slot is free (runner.ErrBusy) a worker retries the call until its max age.
//
// Unlike Lambda the queue is not durable: calls still queued when the server stops are lost
// (logged), and function failures are not retried (Lambda retries twice).

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/om8P/BeeVM/internal/apitypes"
	"github.com/om8P/BeeVM/internal/registry"
	"github.com/om8P/BeeVM/internal/runner"
)

// Defaults for the Deps.Async* settings.
const (
	DefaultAsyncQueueSize = 1000
	DefaultAsyncPerTenant = 250
	DefaultAsyncMaxAge    = 5 * time.Minute
	defaultAsyncWorkers   = 4 // without a warm pool (no VM limit to follow)
	asyncBusyRetry        = 200 * time.Millisecond
)

// asyncJob is one queued call.
type asyncJob struct {
	id       string
	tenantID int64
	tenant   string
	fn       registry.Function
	payload  []byte
	queued   time.Time
}

// asyncQueue is the queue and its workers. Workers start with the first queued call, so a
// server that never gets one starts no goroutines.
type asyncQueue struct {
	jobs         chan asyncJob
	perTenantMax int
	maxAge       time.Duration
	workers      int

	mu        sync.Mutex
	perTenant map[int64]int // queued (not yet started) calls per tenant
	closed    bool

	start  sync.Once
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// newAsyncQueue returns nil when async invoke is off (size < 0).
func newAsyncQueue(size, perTenant, workers int, maxAge time.Duration) *asyncQueue {
	if size < 0 {
		return nil
	}
	if size == 0 {
		size = DefaultAsyncQueueSize
	}
	if perTenant <= 0 || perTenant > size {
		perTenant = min(DefaultAsyncPerTenant, size)
	}
	if maxAge <= 0 {
		maxAge = DefaultAsyncMaxAge
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &asyncQueue{
		jobs: make(chan asyncJob, size), perTenantMax: perTenant, maxAge: maxAge, workers: workers,
		perTenant: map[int64]int{}, ctx: ctx, cancel: cancel,
	}
}

// Why push refused a call.
var (
	errQueueFull   = errors.New("async queue full, try again shortly")
	errTenantFull  = errors.New("too many of your async calls are waiting, try again shortly")
	errQueueClosed = errors.New("server is shutting down")
)

// push adds a call without waiting. It returns how many calls of this tenant are queued now.
func (q *asyncQueue) push(j asyncJob) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return 0, errQueueClosed
	}
	if q.perTenant[j.tenantID] >= q.perTenantMax {
		return 0, errTenantFull
	}
	select {
	case q.jobs <- j:
	default:
		return 0, errQueueFull
	}
	q.perTenant[j.tenantID]++
	return q.perTenant[j.tenantID], nil
}

// taken is called by a worker when it takes a call off the queue.
func (q *asyncQueue) taken(j asyncJob) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.perTenant[j.tenantID]--; q.perTenant[j.tenantID] <= 0 {
		delete(q.perTenant, j.tenantID)
	}
}

// isAsync reports whether the caller asked for an async call.
func isAsync(r *http.Request) bool {
	if v := r.URL.Query().Get("async"); v != "" {
		on, err := strconv.ParseBool(v)
		return err == nil && on
	}
	return strings.EqualFold(r.Header.Get(apitypes.HeaderInvocationType), apitypes.InvocationEvent)
}

// enqueue answers an async invoke: 202 with the request id, or 429/503/501.
func (s *Server) enqueue(w http.ResponseWriter, r *http.Request, tenant registry.Tenant, f registry.Function, payload []byte) {
	infoFrom(r.Context()).enqueued = true // the call itself is counted when it finishes
	q := s.async
	if q == nil {
		writeError(w, http.StatusNotImplemented, "async invoke is turned off on this server")
		return
	}
	id := middleware.GetReqID(r.Context())
	if id == "" {
		id = strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	n, err := q.push(asyncJob{id: id, tenantID: tenant.ID, tenant: tenant.Name, fn: f, payload: payload, queued: time.Now()})
	switch {
	case errors.Is(err, errQueueClosed):
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	case err != nil:
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	q.start.Do(func() { s.startAsyncWorkers(q) })
	writeJSON(w, http.StatusAccepted, apitypes.InvokeAccepted{RequestID: id, Queued: n})
}

func (s *Server) startAsyncWorkers(q *asyncQueue) {
	n := q.workers
	if n <= 0 {
		n = s.maxVMs() // one worker per VM slot: more could only wait for a slot
	}
	if n <= 0 {
		n = defaultAsyncWorkers
	}
	for range n {
		q.wg.Add(1)
		go func() {
			defer q.wg.Done()
			for {
				select {
				case <-q.ctx.Done():
					return
				case j := <-q.jobs:
					q.taken(j)
					s.runAsync(q, j)
				}
			}
		}()
	}
}

// runAsync runs one queued call and reports it (log, metrics, event).
func (s *Server) runAsync(q *asyncQueue, j asyncJob) {
	info := &reqInfo{tenant: j.tenant, tenantID: j.tenantID, function: j.fn.Name, runtime: runtimeName(j.fn),
		requestID: j.id}
	info.queued = time.Since(j.queued)

	status := http.StatusOK
	for {
		if time.Since(j.queued) > q.maxAge {
			status, info.failure = http.StatusGatewayTimeout, "expired in the async queue after "+q.maxAge.String()
			break
		}
		res, err := s.runCall(q.ctx, j.tenantID, j.fn, j.payload, info)
		if errors.Is(err, runner.ErrBusy) && q.ctx.Err() == nil {
			// No VM slot within the pool's wait: keep the call and try again shortly.
			select {
			case <-time.After(asyncBusyRetry):
				continue
			case <-q.ctx.Done():
			}
		}
		switch {
		case err == nil && res.FunctionError != "":
			status, info.failure = http.StatusBadGateway, res.FunctionError
		case err == nil:
		case q.ctx.Err() != nil:
			status, info.failure = statusClientClosed, "server shutting down"
		case errors.Is(err, runner.ErrRuntimeUnavailable):
			status, info.failure = http.StatusServiceUnavailable, err.Error()
		default:
			s.Logger.Error("async run function", "request_id", j.id, "function", j.fn.Name, "tenant", j.tenant, "error", err)
			status, info.failure = http.StatusInternalServerError, "internal error running the function"
		}
		break
	}

	took := time.Since(j.queued)
	attrs := []any{"request_id", j.id, "tenant", j.tenant, "function", j.fn.Name, "runtime", info.runtime,
		"status", status, "queued_ms", info.queued.Milliseconds(), "duration_ms", took.Milliseconds(), "vm", info.vmID}
	if info.start != "" {
		attrs = append(attrs, "start", info.start)
	}
	if info.failure != "" {
		attrs = append(attrs, "error", info.failure)
	}
	s.Logger.Info("async invoke", attrs...)
	s.observeInvoke(info, status, took)
	s.publishInvoke(info, status, took)
}

// Close stops the async workers: calls still queued are dropped (and logged), running ones
// are cancelled. Call it after the HTTP server has stopped taking requests.
func (s *Server) Close() {
	q := s.async
	if q == nil {
		return
	}
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.cancel()
	q.wg.Wait()
	if n := len(q.jobs); n > 0 {
		s.Logger.Warn("dropped queued async calls at shutdown", "calls", n)
	}
}
