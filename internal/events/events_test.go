package events

import (
	"sync"
	"testing"
	"time"
)

func recv(t *testing.T, s *Subscription) Event {
	t.Helper()
	select {
	case e, ok := <-s.C:
		if !ok {
			t.Fatal("channel closed")
		}
		return e
	case <-time.After(time.Second):
		t.Fatal("no event")
	}
	return Event{}
}

func empty(t *testing.T, s *Subscription) {
	t.Helper()
	select {
	case e, ok := <-s.C:
		if ok {
			t.Fatalf("unexpected event %+v", e)
		}
	default:
	}
}

func TestTenantFilter(t *testing.T) {
	h := New(0)
	a, b, all := h.Subscribe(1), h.Subscribe(2), h.Subscribe(0)
	h.Publish(Event{Type: TypeVM, TenantID: 1, Data: "a"})
	h.Publish(Event{Type: TypeInvoke, TenantID: 2, Data: "b"})

	if e := recv(t, a); e.Data != "a" {
		t.Errorf("tenant 1 got %+v", e)
	}
	empty(t, a)
	if e := recv(t, b); e.Data != "b" || e.Type != TypeInvoke {
		t.Errorf("tenant 2 got %+v", e)
	}
	empty(t, b)
	if recv(t, all).Data != "a" || recv(t, all).Data != "b" {
		t.Error("subscriber for all tenants missed events")
	}
}

func TestSlowSubscriberLagsWithoutBlocking(t *testing.T) {
	h := New(2)
	s := h.Subscribe(1)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			h.Publish(Event{TenantID: 1, Data: i})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish blocked on a full subscriber")
	}
	if !h.Lagged(s) {
		t.Error("want lagged")
	}
	if h.Lagged(s) {
		t.Error("Lagged should clear the flag")
	}
	if recv(t, s).Data != 0 || recv(t, s).Data != 1 {
		t.Error("the first events should be kept")
	}
}

func TestCancelAndClose(t *testing.T) {
	h := New(0)
	s := h.Subscribe(1)
	h.Cancel(s)
	h.Cancel(s) // twice is fine
	if _, ok := <-s.C; ok {
		t.Error("cancelled channel should be closed")
	}
	h.Publish(Event{TenantID: 1}) // no panic on a cancelled subscriber

	s2 := h.Subscribe(1)
	if h.Subscribers() != 1 {
		t.Errorf("subscribers = %d", h.Subscribers())
	}
	h.Close()
	if _, ok := <-s2.C; ok {
		t.Error("Close should close every channel")
	}
	h.Cancel(s2) // after close: fine
	s3 := h.Subscribe(1)
	if _, ok := <-s3.C; ok {
		t.Error("Subscribe after Close should give a closed channel")
	}
	var nilHub *Hub
	nilHub.Publish(Event{}) // a nil hub is "events off"
}

func TestConcurrentUse(t *testing.T) {
	h := New(8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				h.Publish(Event{TenantID: int64(j % 3)})
			}
		}()
		go func() {
			defer wg.Done()
			s := h.Subscribe(int64(i % 3))
			for j := 0; j < 20; j++ {
				select {
				case <-s.C:
				default:
				}
				h.Lagged(s)
			}
			h.Cancel(s)
		}()
	}
	wg.Wait()
	h.Close()
}
