package vmm

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/om8P/BeeVM/internal/agentproto"
)

// resumeVM returns a "started" VM whose agent is the fake at f, and a counter of /resume calls.
func resumeVM(t *testing.T, status int) (*VM, *fakeFirecracker, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /resume", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req agentproto.ResumeRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		if status != http.StatusOK {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(agentproto.ErrorResponse{Error: "nope"})
			return
		}
		if req.UnixNano == 0 || len(req.Entropy) < 16 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(agentproto.ResumeResponse{ClockAdjustedMS: 4200})
	})
	f := startFake(t, "OK 1\n", mux)
	v := newVM(Config{ID: "t", BaseDir: t.TempDir()}, time.Millisecond)
	v.agent = newAgentClient(f.path)
	v.startedAt = time.Now()
	t.Cleanup(v.agent.Close)
	return v, f, calls
}

func TestWaitAndResumeFirstTry(t *testing.T) {
	v, _, calls := resumeVM(t, http.StatusOK)
	info, err := v.WaitAndResume(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Attempts != 1 || calls.Load() != 1 || info.ClockAdjustedMS != 4200 || info.Ready <= 0 {
		t.Errorf("got %+v, %d calls", info, calls.Load())
	}
}

func TestWaitAndResumeRetriesUntilAgentListens(t *testing.T) {
	v, f, calls := resumeVM(t, http.StatusOK)
	f.rejectFirst.Store(3)
	info, err := v.WaitAndResume(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Attempts != 4 || calls.Load() != 1 {
		t.Errorf("want 4 attempts and 1 resume, got %+v and %d", info, calls.Load())
	}
}

func TestWaitAndResumeStopsWhenAgentSaysNo(t *testing.T) {
	v, _, calls := resumeVM(t, http.StatusInternalServerError)
	info, err := v.WaitAndResume(context.Background())
	if err == nil || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("got %v", err)
	}
	if info.Attempts != 1 || calls.Load() != 1 {
		t.Errorf("an error answer must not be retried: %+v, %d calls", info, calls.Load())
	}
}

func TestWaitAndResumeGivesUpWithContext(t *testing.T) {
	v, f, _ := resumeVM(t, http.StatusOK)
	f.rejectFirst.Store(1 << 30)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := v.WaitAndResume(ctx); err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Errorf("got %v", err)
	}
}

func TestWaitAndResumeBeforeStart(t *testing.T) {
	v := newVM(Config{ID: "t", BaseDir: t.TempDir()}, time.Millisecond)
	if _, err := v.WaitAndResume(context.Background()); err == nil {
		t.Error("want error for a VM that was never started")
	}
}

func TestLoadTimeFromMetrics(t *testing.T) {
	p := filepath.Join(t.TempDir(), "metrics.json")
	data := `{"latencies_us":{"load_snapshot":0},"vsock":{}}` + "\n" +
		`{"latencies_us":{"load_snapshot":23456,"resume_vm":0},"utc_timestamp_ms":1}` + "\n"
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := loadTimeFromMetrics(p)
	if err != nil || d != 23456*time.Microsecond {
		t.Errorf("got %v, %v", d, err)
	}
	_ = os.WriteFile(p, []byte(`{"latencies_us":{"load_snapshot":0}}`+"\n"), 0o600)
	if _, err := loadTimeFromMetrics(p); err == nil {
		t.Error("want error when no load was recorded")
	}
}

func TestPinProcessRejectsBadCPU(t *testing.T) {
	if err := pinProcess(os.Getpid(), []int{-1}); err == nil {
		t.Error("want error for CPU -1")
	}
}
