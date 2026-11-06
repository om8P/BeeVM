//go:build linux

package agent

// Python and Node.js functions run inside a long-lived "shim" process (guest/shims/*): the
// interpreter starts once, imports the user's package once, and then answers one call after
// another. That is what makes runtime and function snapshots useful: a snapshot taken after
// the import contains the loaded interpreter and module, so a restored VM skips both.
//
// The agent and the shim talk newline-delimited JSON over two pipes that the shim sees as
// file descriptors 3 (requests) and 4 (answers). stdout/stderr stay free for the user's
// prints, so those can never break the protocol.

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/om8P/BeeVM/internal/agentproto"
	"github.com/om8P/BeeVM/internal/pkg"
)

// defaultShims are the commands that start each runtime's shim inside the guest image.
// python3 -I: isolated mode (ignores PYTHON* variables and the user site folder),
// -B: no .pyc files (the code folder is RAM anyway), -u: unbuffered output.
var defaultShims = map[string][]string{
	agentproto.RuntimePython: {"python3", "-I", "-B", "-u", "/usr/lib/beevm/shim.py"},
	agentproto.RuntimeNode:   {"node", "/usr/lib/beevm/shim.js"},
}

// maxAnswer bounds one answer line from the shim: the output plus logs and an error text.
const maxAnswer = agentproto.MaxStdoutBytes + 1<<20

var (
	errShimTimeout = errors.New("shim timeout")
	errAnswerBig   = errors.New("answer too big")
)

// shim is one running runtime process.
type shim struct {
	runtime string
	cmd     *exec.Cmd
	in      *os.File      // our end of the request pipe (the shim's fd 3)
	outFile *os.File      // our end of the answer pipe (the shim's fd 4)
	out     *bufio.Reader // reads answers from outFile
	stderr  *syncTail     // the shim's own stdout/stderr (crashes, warnings)
	exited  chan struct{} // closed once the process has exited
	waitErr error         // valid after exited is closed
}

func startShim(runtime string, argv []string, sb *sandbox) (*shim, error) {
	reqR, reqW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	ansR, ansW, err := os.Pipe()
	if err != nil {
		reqR.Close()
		reqW.Close()
		return nil, err
	}
	tail := &syncTail{tailBuffer: tailBuffer{max: agentproto.MaxStderrBytes}}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = os.TempDir()
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin:/sbin", "HOME=" + os.TempDir()}
	cmd.ExtraFiles = []*os.File{reqR, ansW} // become fd 3 and fd 4 in the child
	cmd.Stdout = tail
	cmd.Stderr = tail
	// Own process group, so stopping the shim also stops anything the function started.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	sb.apply(cmd) // the user's code runs inside the shim: not as root
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		reqR.Close()
		reqW.Close()
		ansR.Close()
		ansW.Close()
		return nil, err
	}
	sb.started(cmd.Process.Pid)
	// The child has its own copies; ours must be closed, or reads would never see EOF.
	reqR.Close()
	ansW.Close()
	s := &shim{runtime: runtime, cmd: cmd, in: reqW, outFile: ansR, out: bufio.NewReaderSize(ansR, 64<<10),
		stderr: tail, exited: make(chan struct{})}
	go func() {
		s.waitErr = cmd.Wait()
		close(s.exited)
	}()
	return s, nil
}

// roundTrip sends one request line and waits up to timeout for one answer line.
func (s *shim) roundTrip(req any, timeout time.Duration) ([]byte, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	type result struct {
		line []byte
		err  error
	}
	// Writing can block too (a payload bigger than the pipe buffer while the shim is busy),
	// so both the write and the read happen in the goroutine; the timer covers both.
	done := make(chan result, 1)
	go func() {
		if _, err := s.in.Write(data); err != nil {
			done <- result{err: err}
			return
		}
		line, err := readLine(s.out, maxAnswer)
		done <- result{line, err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.line, r.err
	case <-timer.C:
		return nil, errShimTimeout
	}
}

// readLine reads one '\n'-terminated line of at most max bytes. A longer line is read to its
// end and dropped (so the next answer starts at a line boundary) and errAnswerBig is returned.
func readLine(r *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	tooBig := false
	for {
		chunk, err := r.ReadSlice('\n')
		if !tooBig {
			if len(line)+len(chunk) > max {
				tooBig, line = true, nil
			} else {
				line = append(line, chunk...)
			}
		}
		switch {
		case err == nil:
			if tooBig {
				return nil, errAnswerBig
			}
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			return nil, err
		}
	}
}

// shimGroup is the running shim's process group (0 if none). The caller holds s.mu.
func (s *Server) shimGroup() int {
	if s.shim == nil || s.shim.cmd.Process == nil {
		return 0
	}
	select {
	case <-s.shim.exited:
		return 0
	default:
		return s.shim.cmd.Process.Pid
	}
}

// kill stops the shim's whole process group and waits briefly for it to be gone.
func (s *shim) kill() {
	_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
	s.in.Close()
	s.outFile.Close()
	select {
	case <-s.exited:
	case <-time.After(3 * time.Second):
	}
}

// failure turns a roundTrip error into the error text callers see. Texts starting with
// "timeout after" or "runtime exited" make the host throw the VM away.
func (s *shim) failure(err error, timeout time.Duration) string {
	switch {
	case errors.Is(err, errShimTimeout):
		return fmt.Sprintf("timeout after %s", timeout)
	case errors.Is(err, errAnswerBig):
		return fmt.Sprintf("output larger than %d bytes", agentproto.MaxStdoutBytes)
	}
	// EOF or a broken pipe: the process died. Give Wait a moment to collect its status.
	select {
	case <-s.exited:
		return fmt.Sprintf("runtime exited: %v", s.waitErr)
	case <-time.After(time.Second):
		return fmt.Sprintf("runtime exited: %v", err)
	}
}

// syncTail is a tailBuffer that may be written by exec's copy goroutine and read by us.
type syncTail struct {
	mu sync.Mutex
	tailBuffer
}

func (t *syncTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.tailBuffer.Write(p)
}

func (t *syncTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.tailBuffer.String()
}

// Messages to the shim, and its answer.
type shimLoad struct {
	Op      string `json:"op"`
	Dir     string `json:"dir"`
	Handler string `json:"handler"`
}

type shimInvoke struct {
	Op      string          `json:"op"`
	Payload json.RawMessage `json:"payload"`
}

type shimAnswer struct {
	OK     bool            `json:"ok"`
	Output json.RawMessage `json:"output"`
	Error  string          `json:"error"`
	Logs   string          `json:"logs"`
}

// stopShim kills the running shim, if any. The caller holds s.mu.
func (s *Server) stopShim() {
	if s.shim != nil {
		s.shim.kill()
		s.shim = nil
	}
	s.loaded = ""
}

// handlePackage receives a zip package and unpacks it into the code folder, replacing the
// previous one. A runtime that already imported the old code is stopped.
func (s *Server) handlePackage(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, agentproto.MaxUploadBytes)
	s.mu.Lock()
	defer s.mu.Unlock()

	tmp, sum, n, ok := s.receive(w, r, "package")
	if !ok {
		return
	}
	defer os.Remove(tmp)
	next := s.codeDir + ".new"
	_ = os.RemoveAll(next)
	if _, err := pkg.Unpack(tmp, next); err != nil {
		_ = os.RemoveAll(next)
		status := http.StatusInternalServerError
		if errors.Is(err, pkg.ErrInvalid) {
			status = http.StatusBadRequest
		}
		writeError(w, status, "unpack: %v", err)
		return
	}
	if s.loaded != "" {
		s.stopShim() // its modules came from the old code
	}
	_ = os.RemoveAll(s.codeDir)
	if err := os.Rename(next, s.codeDir); err != nil {
		writeError(w, http.StatusInternalServerError, "install package: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, agentproto.UploadResponse{SHA256: sum, Size: n})
}

// handleLoad starts the runtime's shim (if needed) and imports the handler (if given).
func (s *Server) handleLoad(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req agentproto.LoadRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad request: %v", err)
		return
	}
	argv, ok := s.shimCmds[req.Runtime]
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown runtime %q", req.Runtime)
		return
	}
	if err := pkg.ValidateHandler(req.Runtime, req.Handler); err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	timeout := agentproto.DefaultLoadTimeout
	if req.TimeoutMS != 0 {
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
	}
	if timeout <= 0 || timeout > agentproto.MaxTimeout {
		writeError(w, http.StatusBadRequest, "timeout_ms must be between 1 and %d", agentproto.MaxTimeout.Milliseconds())
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if req.Handler != "" {
		if _, err := os.Stat(s.codeDir); err != nil {
			writeError(w, http.StatusConflict, "no package uploaded yet")
			return
		}
	}
	// A different runtime, or a second handler in the same interpreter (old and new modules
	// could mix): start from a fresh process.
	if s.shim != nil && (s.shim.runtime != req.Runtime || (req.Handler != "" && s.loaded != "")) {
		s.stopShim()
	}
	start := time.Now()
	if s.shim == nil {
		sh, err := startShim(req.Runtime, argv, s.sb)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "cannot start the %s runtime: %v", req.Runtime, err)
			return
		}
		s.shim = sh
	}
	line, err := s.shim.roundTrip(shimLoad{Op: "load", Dir: s.codeDir, Handler: req.Handler}, timeout)
	if err != nil {
		msg := s.shim.failure(err, timeout)
		if out := strings.TrimSpace(s.shim.stderr.String()); out != "" {
			msg += "\n" + out
		}
		s.stopShim()
		writeError(w, http.StatusUnprocessableEntity, "load %s: %s", req.Handler, msg)
		return
	}
	var ans shimAnswer
	if err := json.Unmarshal(line, &ans); err != nil {
		s.stopShim()
		writeError(w, http.StatusInternalServerError, "runtime sent a bad answer: %v", err)
		return
	}
	if !ans.OK {
		s.stopShim() // a half-imported module is not worth keeping
		writeError(w, http.StatusUnprocessableEntity, "%s", joinLogs(ans.Logs, ans.Error))
		return
	}
	s.loaded = req.Handler
	writeJSON(w, http.StatusOK, agentproto.LoadResponse{LoadMS: time.Since(start).Milliseconds(), Logs: ans.Logs})
}

// invokeShim calls the loaded handler once. The caller holds s.mu.
func (s *Server) invokeShim(payload json.RawMessage, timeout time.Duration) agentproto.InvokeResponse {
	start := time.Now()
	resp := agentproto.InvokeResponse{ExitCode: -1}
	line, err := s.shim.roundTrip(shimInvoke{Op: "invoke", Payload: payload}, timeout)
	resp.DurationMS = time.Since(start).Milliseconds()
	// Processes the call started outside the shim's process group (setsid) do not survive it.
	defer func() { s.sb.sweep(s.shimGroup()) }()
	if errors.Is(err, errAnswerBig) {
		resp.Error = s.shim.failure(err, timeout) // the shim is still fine: keep it
		return resp
	}
	if err != nil {
		resp.Error = s.shim.failure(err, timeout)
		resp.Stderr = s.shim.stderr.String()
		s.stopShim()
		return resp
	}
	var ans shimAnswer
	if err := json.Unmarshal(line, &ans); err != nil {
		resp.Error = "runtime exited: bad answer: " + err.Error()
		s.stopShim()
		return resp
	}
	resp.Stderr = ans.Logs
	if !ans.OK {
		resp.ExitCode = 1
		resp.Error = "function raised an error: " + summary(s.shim.runtime, ans.Error)
		resp.Stderr = joinLogs(ans.Logs, ans.Error)
		return resp
	}
	resp.ExitCode = 0
	if len(ans.Output) == 0 {
		ans.Output = json.RawMessage("null")
	}
	resp.Output = ans.Output
	return resp
}

// summary picks the line that says what went wrong: Python prints it last
// ("ZeroDivisionError: division by zero"), Node.js first ("Error: boom").
func summary(runtime, text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	pick := lines[0]
	if runtime == agentproto.RuntimePython {
		pick = lines[len(lines)-1]
	}
	pick = strings.TrimSpace(pick)
	if len(pick) > 300 {
		pick = pick[:300] + "..."
	}
	return pick
}

// joinLogs puts the function's prints and the error text together, keeping the last
// MaxStderrBytes (the error is at the end, so it survives).
func joinLogs(logs, errText string) string {
	out := errText
	if logs != "" {
		out = strings.TrimRight(logs, "\n") + "\n" + errText
	}
	if len(out) > agentproto.MaxStderrBytes {
		out = out[len(out)-agentproto.MaxStderrBytes:]
	}
	return out
}

// receive copies the request body into a temp file next to the function, hashing it.
// On failure it has already written the error reply and ok is false.
func (s *Server) receive(w http.ResponseWriter, r *http.Request, what string) (tmp, sum string, n int64, ok bool) {
	dir := filepath.Dir(s.funcPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "create %s: %v", dir, err)
		return "", "", 0, false
	}
	f, err := os.CreateTemp(dir, "upload-*")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create temp file: %v", err)
		return "", "", 0, false
	}
	hash := sha256.New()
	n, err = io.Copy(io.MultiWriter(f, hash), r.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	fail := func(status int, format string, args ...any) (string, string, int64, bool) {
		os.Remove(f.Name())
		writeError(w, status, format, args...)
		return "", "", 0, false
	}
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return fail(http.StatusRequestEntityTooLarge, "%s larger than %d bytes", what, agentproto.MaxUploadBytes)
		}
		return fail(http.StatusBadRequest, "read upload: %v", err)
	}
	if n == 0 {
		return fail(http.StatusBadRequest, "empty upload")
	}
	return f.Name(), hex.EncodeToString(hash.Sum(nil)), n, true
}
