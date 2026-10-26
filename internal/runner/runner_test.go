package runner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/om8P/BeeVM/internal/vmm"
)

// These tests need no VM: they only exercise the slot logic, which runs before a VM is created.

func TestBusyWhenAllSlotsAreTaken(t *testing.T) {
	c := NewColdRunner(vmm.Config{}, 1, 50*time.Millisecond)
	c.slots <- struct{}{} // pretend one VM is running

	start := time.Now()
	_, err := c.Run(context.Background(), Request{})
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
	if took := time.Since(start); took < 40*time.Millisecond || took > time.Second {
		t.Errorf("should wait about queueWait (50 ms), waited %v", took)
	}
}

func TestWaitingCallStopsWhenTheCallerGoesAway(t *testing.T) {
	c := NewColdRunner(vmm.Config{}, 1, time.Minute)
	c.slots <- struct{}{}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Run(ctx, Request{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the context error, got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Error("a caller that hung up must not keep waiting for a slot")
	}
}

func TestAtLeastOneSlot(t *testing.T) {
	c := NewColdRunner(vmm.Config{}, 0, time.Second)
	if cap(c.slots) != 1 {
		t.Errorf("maxVMs 0 should become 1, got %d", cap(c.slots))
	}
}

func TestRunnerForcesAgentMode(t *testing.T) {
	c := NewColdRunner(vmm.Config{ExtraBootArgs: "something else"}, 1, time.Second)
	if c.base.ExtraBootArgs != "beevm.mode=agent" {
		t.Errorf("boot args %q", c.base.ExtraBootArgs)
	}
}
