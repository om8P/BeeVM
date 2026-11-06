//go:build linux

// Package agent is the HTTP server that runs inside every MicroVM. It receives a function
// binary and JSON payloads, runs the function, and returns its JSON output.
//
// It knows nothing about vsock: cmd/beevm-agent gives it a listener. That keeps it testable
// with an ordinary HTTP test server.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/om8P/BeeVM/internal/agentproto"
)

// Server serves the agent protocol.
type Server struct {
	funcPath string
	version  string
	started  time.Time

	// Changing the clock and the kernel's random state; replaced in unit tests.
	setClock   func(time.Time) error
	addEntropy func([]byte) error

	// Python/Node.js: where packages are unpacked, how to start each runtime's shim (replaced
	// in unit tests), the running shim and the handler it has imported ("" = none).
	codeDir  string
	shimCmds map[string][]string
	shim     *shim
	loaded   string

	sb *sandbox // limits for the function's processes (nil when the agent is not root)

	// mu makes uploads and invocations run one at a time: a FaaS VM serves one request
	// at a time, and an upload must not replace the binary while it is running.
	mu sync.Mutex
}

// New creates a Server that stores a binary function at funcPath and unpacks packages into
// the "code" folder next to it (agentproto.CodeDir in the guest).
func New(funcPath, version string) *Server {
	return &Server{funcPath: funcPath, version: version, started: time.Now(),
		setClock: setSystemClock, addEntropy: addKernelEntropy,
		codeDir: filepath.Join(filepath.Dir(funcPath), "code"), shimCmds: defaultShims, sb: newSandbox()}
}

// Handler returns the HTTP handler for the agent's endpoints.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+agentproto.PathHealth, s.handleHealth)
	mux.HandleFunc("PUT "+agentproto.PathFunction, s.handleUpload)
	mux.HandleFunc("POST "+agentproto.PathInvoke, s.handleInvoke)
	mux.HandleFunc("POST "+agentproto.PathResume, s.handleResume)
	mux.HandleFunc("PUT "+agentproto.PathPackage, s.handlePackage)
	mux.HandleFunc("POST "+agentproto.PathLoad, s.handleLoad)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(w, status, agentproto.ErrorResponse{Error: fmt.Sprintf(format, args...)})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, agentproto.HealthResponse{
		Status:       "ok",
		AgentVersion: s.version,
		UptimeMS:     time.Since(s.started).Milliseconds(),
	})
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, agentproto.MaxUploadBytes)
	s.mu.Lock()
	defer s.mu.Unlock()

	// Write to a temp file and rename, so a half-uploaded binary is never executable.
	tmp, sum, n, ok := s.receive(w, r, "function")
	if !ok {
		return
	}
	defer os.Remove(tmp) // no-op after a successful rename
	if err := os.Chmod(tmp, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "chmod: %v", err)
		return
	}
	if err := os.Rename(tmp, s.funcPath); err != nil {
		writeError(w, http.StatusInternalServerError, "install function: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, agentproto.UploadResponse{SHA256: sum, Size: n})
}

func (s *Server) handleInvoke(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, agentproto.MaxPayloadBytes+4096)
	var req agentproto.InvokeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload larger than %d bytes", agentproto.MaxPayloadBytes)
		} else {
			writeError(w, http.StatusBadRequest, "bad request: %v", err)
		}
		return
	}
	if len(req.Payload) > agentproto.MaxPayloadBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "payload larger than %d bytes", agentproto.MaxPayloadBytes)
		return
	}
	if len(req.Payload) == 0 {
		req.Payload = json.RawMessage("null")
	}
	timeout := agentproto.DefaultTimeout
	if req.TimeoutMS != 0 {
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
	}
	if timeout <= 0 || timeout > agentproto.MaxTimeout {
		writeError(w, http.StatusBadRequest, "timeout_ms must be between 1 and %d", agentproto.MaxTimeout.Milliseconds())
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.shim != nil { // a Python/Node.js runtime is running: the call goes to its handler
		if s.loaded == "" {
			writeError(w, http.StatusConflict, "no handler loaded yet")
			return
		}
		writeJSON(w, http.StatusOK, s.invokeShim(req.Payload, timeout))
		return
	}
	if _, err := os.Stat(s.funcPath); err != nil {
		writeError(w, http.StatusConflict, "no function uploaded yet")
		return
	}
	writeJSON(w, http.StatusOK, s.run(req.Payload, timeout))
}

// run executes the function once: payload on stdin, JSON on stdout, logs on stderr.
func (s *Server) run(payload []byte, timeout time.Duration) agentproto.InvokeResponse {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var stdout limitedBuffer
	stdout.max = agentproto.MaxStdoutBytes
	var stderr tailBuffer
	stderr.max = agentproto.MaxStderrBytes

	cmd := exec.CommandContext(ctx, s.funcPath)
	cmd.Dir = os.TempDir()
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin:/sbin", "HOME=" + os.TempDir()}
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Own process group, so a timeout can kill the function AND anything it started.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	s.sb.apply(cmd) // not as root
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	// If a stray child keeps the output pipes open, do not wait for it forever.
	cmd.WaitDelay = 2 * time.Second

	start := time.Now()
	resp := agentproto.InvokeResponse{ExitCode: -1}
	if err := cmd.Start(); err != nil {
		resp.Error = "cannot start function: " + err.Error()
		return resp
	}
	pgid := cmd.Process.Pid
	s.sb.started(pgid)
	waitErr := cmd.Wait()
	_ = syscall.Kill(-pgid, syscall.SIGKILL) // clean up children the function left behind
	s.sb.sweep(s.shimGroup())                // and those that left its process group (setsid)
	resp.DurationMS = time.Since(start).Milliseconds()
	resp.Stderr = stderr.String()
	if cmd.ProcessState != nil {
		resp.ExitCode = cmd.ProcessState.ExitCode()
	}

	var exitErr *exec.ExitError
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		resp.Error = fmt.Sprintf("timeout after %s", timeout)
	case errors.As(waitErr, &exitErr):
		if resp.ExitCode == -1 {
			resp.Error = "function was killed: " + exitErr.Error()
		} else {
			resp.Error = fmt.Sprintf("function exited with code %d", resp.ExitCode)
		}
	case waitErr != nil && !errors.Is(waitErr, exec.ErrWaitDelay):
		resp.Error = "run function: " + waitErr.Error()
	case stdout.exceeded:
		resp.Error = fmt.Sprintf("output larger than %d bytes", agentproto.MaxStdoutBytes)
	default:
		out := bytes.TrimSpace(stdout.buf.Bytes())
		if json.Valid(out) {
			resp.Output = json.RawMessage(append([]byte(nil), out...))
		} else {
			resp.Error = "function output is not valid JSON"
			resp.Stdout = firstBytes(out, 4096)
		}
	}
	return resp
}

func firstBytes(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
	return string(b)
}

// limitedBuffer keeps the first max bytes and drops the rest. It reports every write as
// successful so the function is not killed by a broken pipe; exceeded records the overflow.
type limitedBuffer struct {
	max      int
	buf      bytes.Buffer
	exceeded bool
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	room := l.max - l.buf.Len()
	if len(p) > room {
		l.exceeded = true
		p2 := p
		if room > 0 {
			l.buf.Write(p2[:room])
		}
		return len(p), nil
	}
	l.buf.Write(p)
	return len(p), nil
}

// tailBuffer keeps the last max bytes written.
type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > 2*t.max { // trim occasionally, not on every write
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	b := t.buf
	if len(b) > t.max {
		b = b[len(b)-t.max:]
	}
	return string(b)
}
