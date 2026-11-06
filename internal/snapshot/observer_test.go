package snapshot

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestObserverSeesBuilds(t *testing.T) {
	m, _ := newTestManager(t)
	type obs struct {
		kind string
		d    time.Duration
		err  error
	}
	var mu sync.Mutex
	var seen []obs
	m.SetObserver(func(kind string, d time.Duration, err error) {
		mu.Lock()
		seen = append(seen, obs{kind, d, err})
		mu.Unlock()
	})

	if _, err := m.Ensure(context.Background(), 128, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Ensure(context.Background(), 128, 1); err != nil { // reused: no build
		t.Fatal(err)
	}
	m.build = func(context.Context, buildJob, string) error { return errors.New("vm did not start") }
	if _, err := m.Ensure(context.Background(), 256, 1); err == nil {
		t.Fatal("want an error")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("observer called %d times, want 2: %+v", len(seen), seen)
	}
	if seen[0].kind != KindRuntime || seen[0].err != nil || seen[0].d < 20*time.Millisecond {
		t.Errorf("first build: %+v", seen[0])
	}
	if seen[1].err == nil {
		t.Errorf("failed build reported without error: %+v", seen[1])
	}
}
