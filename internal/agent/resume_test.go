//go:build linux

package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/om8P/BeeVM/internal/agentproto"
)

// fakeSystem replaces the clock and entropy calls, so the test machine is not touched.
func fakeSystem(s *Server) (clock *time.Time, entropy *[]byte) {
	clock, entropy = new(time.Time), new([]byte)
	s.setClock = func(t time.Time) error { *clock = t; return nil }
	s.addEntropy = func(b []byte) error { *entropy = append([]byte(nil), b...); return nil }
	return clock, entropy
}

func postResume(t *testing.T, url string, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url+agentproto.PathResume, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestResumeSetsClockAndEntropy(t *testing.T) {
	ts, s := newTestServer(t)
	clock, entropy := fakeSystem(s)
	want := time.Now().Add(5 * time.Second).Truncate(time.Microsecond)
	body, _ := json.Marshal(agentproto.ResumeRequest{UnixNano: want.UnixNano(), Entropy: bytes.Repeat([]byte{7}, 64)})
	resp := postResume(t, ts.URL, string(body))
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var out agentproto.ResumeResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if !clock.Equal(want) {
		t.Errorf("clock set to %v, want %v", *clock, want)
	}
	if len(*entropy) != 64 {
		t.Errorf("entropy %d bytes, want 64", len(*entropy))
	}
	if out.ClockAdjustedMS < 4000 || out.ClockAdjustedMS > 6000 {
		t.Errorf("clock_adjusted_ms = %d, want about 5000", out.ClockAdjustedMS)
	}
}

func TestResumeRejectsBadRequests(t *testing.T) {
	ts, s := newTestServer(t)
	fakeSystem(s)
	for name, body := range map[string]string{
		"not json":      `{`,
		"no time":       `{"entropy":"AAAAAAAAAAAAAAAAAAAAAA=="}`,
		"short entropy": `{"unix_nano":1,"entropy":"AAAA"}`,
		"unknown field": `{"unix_nano":1,"entropy":"AAAAAAAAAAAAAAAAAAAAAA==","x":1}`,
	} {
		resp := postResume(t, ts.URL, body)
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("%s: status %d, want 400", name, resp.StatusCode)
		}
	}
}

func TestResumeReportsSystemFailure(t *testing.T) {
	ts, s := newTestServer(t)
	fakeSystem(s)
	s.setClock = func(time.Time) error { return errors.New("operation not permitted") }
	body, _ := json.Marshal(agentproto.ResumeRequest{UnixNano: time.Now().UnixNano(), Entropy: make([]byte, 32)})
	resp := postResume(t, ts.URL, string(body))
	resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Fatalf("status %d, want 500", resp.StatusCode)
	}
}
