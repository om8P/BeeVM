// Package events is a small publish/subscribe hub for live updates (M11): the warm pool and the
// API publish what happens (a VM changed state, a call finished), and every open event stream
// (GET /api/v1/events) receives the events of its own tenant.
//
// Publish never blocks. It is called with the pool's lock held, so a slow browser must never be
// able to stop the pool. Each subscriber has a buffer; when it is full, events are dropped and
// the subscriber is told it lagged, so it can reload the full state.
package events

import "sync"

// Event types.
const (
	TypeVM     = "vm"     // Data: a VM's new state
	TypeInvoke = "invoke" // Data: a finished call
)

// Event is one thing that happened, for one tenant.
type Event struct {
	Type     string
	TenantID int64
	Data     any
}

// DefaultBuffer is how many events a subscriber may fall behind before it lags.
const DefaultBuffer = 256

// Hub hands published events to subscribers.
type Hub struct {
	mu     sync.Mutex
	subs   map[*Subscription]struct{}
	closed bool
	buffer int
}

// New creates a hub. buffer <= 0 means DefaultBuffer.
func New(buffer int) *Hub {
	if buffer <= 0 {
		buffer = DefaultBuffer
	}
	return &Hub{subs: map[*Subscription]struct{}{}, buffer: buffer}
}

// Subscription is one listener. Read events from C; C is closed when the subscription is
// cancelled or the hub closes.
type Subscription struct {
	C        <-chan Event
	ch       chan Event
	tenantID int64
	lagged   bool
	done     bool
}

// Subscribe starts listening to one tenant's events (tenantID <= 0: every tenant).
func (h *Hub) Subscribe(tenantID int64) *Subscription {
	ch := make(chan Event, h.buffer)
	s := &Subscription{C: ch, ch: ch, tenantID: tenantID}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		s.done = true
		close(ch)
		return s
	}
	h.subs[s] = struct{}{}
	return s
}

// Cancel stops a subscription and closes its channel. Calling it twice is fine.
func (h *Hub) Cancel(s *Subscription) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.endLocked(s)
}

func (h *Hub) endLocked(s *Subscription) {
	if s.done {
		return
	}
	s.done = true
	delete(h.subs, s)
	close(s.ch)
}

// Lagged reports (and clears) whether events were dropped for s since the last call.
func (h *Hub) Lagged(s *Subscription) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	l := s.lagged
	s.lagged = false
	return l
}

// Publish hands e to every matching subscriber without waiting.
func (h *Hub) Publish(e Event) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		if s.tenantID > 0 && s.tenantID != e.TenantID {
			continue
		}
		select {
		case s.ch <- e:
		default:
			s.lagged = true // full: drop it, the reader reloads
		}
	}
}

// Subscribers counts open subscriptions (for tests and logs).
func (h *Hub) Subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// Close ends every subscription; later Subscribe calls get a closed channel. Used on server
// shutdown so open event streams end instead of keeping the server waiting.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for s := range h.subs {
		h.endLocked(s)
	}
}
