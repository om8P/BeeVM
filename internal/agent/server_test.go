//go:build linux

package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/OmMhatre-99/BeeVM/internal/agentproto"
)

// testfnPath is the compiled examples/functions/testfn, built once for all tests.
var testfnPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "agent-test")
	if err != nil {
		panic(err)
	}
	testfnPath = filepath.Join(dir, "testfn")
	goBin := filepath.Join(runtime.GOROOT(), "bin", "go")
	cmd := exec.Command(goBin, "build", "-o", testfnPath, "../../examples/functions/testfn")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building testfn: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// newTestServer starts the agent on a local HTTP test server.
func newTestServer(t *testing.T) (*httptest.Server, *Server) {
	t.Helper()
	s := New(filepath.Join(t.TempDir(), "run", "function"), "test")
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, s
}

func upload(t *testing.T, ts *httptest.Server, path string) *http.Response {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPut, ts.URL+agentproto.PathFunction, bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func invoke(t *testing.T, ts *httptest.Server, payload string, timeoutMS int64) (int, agentproto.InvokeResponse) {
	t.Helper()
	req := fmt.Sprintf(`{"payload":%s,"timeout_ms":%d}`, payload, timeoutMS)
	resp, err := http.Post(ts.URL+agentproto.PathInvoke, "application/json", strings.NewReader(req))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out agentproto.InvokeResponse
	data, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(data, &out)
	return resp.StatusCode, out
}

func TestHealth(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + agentproto.PathHealth)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var h agentproto.HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || h.Status != "ok" || h.AgentVersion != "test" {
		t.Errorf("got %d %+v", resp.StatusCode, h)
	}
}

func TestInvokeBeforeUploadIs409(t *testing.T) {
	ts, _ := newTestServer(t)
	status, _ := invoke(t, ts, `{}`, 1000)
	if status != http.StatusConflict {
		t.Errorf("want 409, got %d", status)
	}
}

func TestUploadAndEcho(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := upload(t, ts, testfnPath)
	defer resp.Body.Close()
	var up agentproto.UploadResponse
	_ = json.NewDecoder(resp.Body).Decode(&up)
	if resp.StatusCode != 200 || up.Size == 0 || len(up.SHA256) != 64 {
		t.Fatalf("upload: %d %+v", resp.StatusCode, up)
	}

	status, out := invoke(t, ts, `{"mode":"echo","n":42}`, 5000)
	if status != 200 || out.Error != "" || out.ExitCode != 0 {
		t.Fatalf("invoke: %d %+v", status, out)
	}
	var got struct {
		Echo struct {
			N int `json:"n"`
		} `json:"echo"`
	}
	if err := json.Unmarshal(out.Output, &got); err != nil || got.Echo.N != 42 {
		t.Errorf("output %s (%v)", out.Output, err)
	}
}

func TestEmptyUploadIs400(t *testing.T) {
	ts, _ := newTestServer(t)
	req, _ := http.NewRequest(http.MethodPut, ts.URL+agentproto.PathFunction, strings.NewReader(""))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("want 400, got %d", resp.StatusCode)
	}
}

func TestFailingFunction(t *testing.T) {
	ts, _ := newTestServer(t)
	upload(t, ts, testfnPath).Body.Close()
	status, out := invoke(t, ts, `{"mode":"fail"}`, 5000)
	if status != 200 {
		t.Fatalf("status %d", status)
	}
	if out.ExitCode != 3 || !strings.Contains(out.Error, "code 3") || !strings.Contains(out.Stderr, "boom") {
		t.Errorf("got %+v", out)
	}
}

func TestGarbageOutput(t *testing.T) {
	ts, _ := newTestServer(t)
	upload(t, ts, testfnPath).Body.Close()
	_, out := invoke(t, ts, `{"mode":"garbage"}`, 5000)
	if !strings.Contains(out.Error, "not valid JSON") || !strings.Contains(out.Stdout, "this is not json") {
		t.Errorf("got %+v", out)
	}
}

func TestOutputTooBig(t *testing.T) {
	ts, _ := newTestServer(t)
	upload(t, ts, testfnPath).Body.Close()
	_, out := invoke(t, ts, fmt.Sprintf(`{"mode":"big","bytes":%d}`, agentproto.MaxStdoutBytes+1024), 20000)
	if !strings.Contains(out.Error, "output larger than") {
		t.Errorf("got error %q", out.Error)
	}
}

func TestPayloadTooBigIs413(t *testing.T) {
	ts, _ := newTestServer(t)
	upload(t, ts, testfnPath).Body.Close()
	big := `"` + strings.Repeat("a", agentproto.MaxPayloadBytes+100) + `"`
	status, _ := invoke(t, ts, big, 5000)
	if status != http.StatusRequestEntityTooLarge {
		t.Errorf("want 413, got %d", status)
	}
}

func TestBadTimeoutIs400(t *testing.T) {
	ts, _ := newTestServer(t)
	upload(t, ts, testfnPath).Body.Close()
	status, _ := invoke(t, ts, `{}`, agentproto.MaxTimeout.Milliseconds()+1)
	if status != http.StatusBadRequest {
		t.Errorf("want 400, got %d", status)
	}
}

func TestTimeoutKillsWholeProcessGroup(t *testing.T) {
	ts, _ := newTestServer(t)
	upload(t, ts, testfnPath).Body.Close()
	pidfile := filepath.Join(t.TempDir(), "child.pid")

	start := time.Now()
	_, out := invoke(t, ts, fmt.Sprintf(`{"mode":"sleep","ms":30000,"pidfile":%q}`, pidfile), 300)
	took := time.Since(start)
	if !strings.Contains(out.Error, "timeout") {
		t.Errorf("want timeout error, got %+v", out)
	}
	if took > 5*time.Second {
		t.Errorf("timeout took %v; the function's child kept the pipes open", took)
	}

	data, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatalf("child never started: %v", err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	// The child must be gone (it can take a moment to disappear from the process table).
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if syscall.Kill(pid, 0) == nil {
		// A killed child that has not been reaped is a zombie; accept that, reject a live process.
		if b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); !strings.Contains(string(b), ") Z ") {
			t.Errorf("background child %d is still running after the timeout", pid)
		}
	}

	// The agent must still work afterwards.
	if _, out := invoke(t, ts, `{"mode":"echo"}`, 5000); out.Error != "" {
		t.Errorf("agent broken after timeout: %+v", out)
	}
}

func TestConcurrentInvokesRunOneAtATime(t *testing.T) {
	ts, _ := newTestServer(t)
	upload(t, ts, testfnPath).Body.Close()

	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, out := invoke(t, ts, `{"mode":"sleep","ms":200}`, 5000); out.Error != "" {
				t.Errorf("invoke failed: %+v", out)
			}
		}()
	}
	wg.Wait()
	if took := time.Since(start); took < 550*time.Millisecond {
		t.Errorf("3 x 200 ms calls finished in %v; they ran in parallel", took)
	}
}

func TestLimitedAndTailBuffers(t *testing.T) {
	lb := limitedBuffer{max: 5}
	_, _ = lb.Write([]byte("abc"))
	_, _ = lb.Write([]byte("defgh"))
	if lb.buf.String() != "abcde" || !lb.exceeded {
		t.Errorf("limited: %q exceeded=%v", lb.buf.String(), lb.exceeded)
	}
	tb := tailBuffer{max: 4}
	for _, s := range []string{"12", "34", "56", "78", "9"} {
		_, _ = tb.Write([]byte(s))
	}
	if tb.String() != "6789" {
		t.Errorf("tail: %q", tb.String())
	}
}
