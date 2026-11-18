package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/om8P/BeeVM/internal/apitypes"
	"github.com/om8P/BeeVM/internal/registry"
	"github.com/om8P/BeeVM/internal/runner"
)

// statusClientClosed is the (non-standard but widely used) status logged when the caller hung
// up before we could answer. Nobody reads the response, so we only record it.
const statusClientClosed = 499

// invoke runs a tenant's function. The request body is the payload (JSON); the response body
// is the function's JSON output.
//
//	200 output of the function         404 no such function
//	400 payload is not valid JSON      413 payload too large
//	502 the function failed            503 no free VM slot, retry shortly
//	500 BeeVM itself failed
//
// With ?async=1 (or X-BeeVM-Invocation-Type: Event) the call is queued instead (see async.go):
//
//	202 apitypes.InvokeAccepted        429 queue full, retry shortly
func (s *Server) invoke(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	f, err := s.Registry.GetFunction(tenant.ID, chi.URLParam(r, "name"))
	if errors.Is(err, registry.ErrNotFound) {
		writeError(w, http.StatusNotFound, "function not found")
		return
	}
	if err != nil {
		s.Logger.Error("get function", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	info := infoFrom(r.Context())
	info.function, info.runtime = f.Name, runtimeName(f)

	payload, ok := s.readPayload(w, r)
	if !ok {
		return
	}
	if isAsync(r) {
		s.enqueue(w, r, tenant, f, payload)
		return
	}

	res, err := s.runCall(r.Context(), tenant.ID, f, payload, info)
	switch {
	case errors.Is(err, runner.ErrBusy):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	case errors.Is(err, runner.ErrRuntimeUnavailable):
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	case errors.Is(err, context.Canceled) && r.Context().Err() != nil:
		w.WriteHeader(statusClientClosed)
		return
	case isCodeError(err):
		s.Logger.Error("cannot get code for deployed function", "function", f.Name, "sha", f.CodeSHA256, "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	case err != nil:
		// The details (paths, VM internals) go to the log, not to the caller.
		s.Logger.Error("run function", "function", f.Name, "tenant", tenant.Name, "vm", res.VMID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal error running the function")
		return
	}

	h := w.Header()
	h.Set(apitypes.HeaderVMID, res.VMID)
	h.Set(apitypes.HeaderBootMS, strconv.FormatInt(res.BootMS, 10))
	h.Set(apitypes.HeaderFunctionMS, strconv.FormatInt(res.FunctionMS, 10))
	h.Set(apitypes.HeaderTotalMS, strconv.FormatInt(res.TotalMS, 10))
	h.Set(apitypes.HeaderCodeCache, info.cache)
	h.Set(apitypes.HeaderFetchMS, strconv.FormatInt(info.fetchMS, 10))
	h.Set(apitypes.HeaderStart, res.StartKind)
	h.Set(apitypes.HeaderUploadMS, strconv.FormatInt(res.UploadMS, 10))
	if res.Snapshot != "" {
		h.Set(apitypes.HeaderSnapshot, res.Snapshot)
	}
	h.Set(apitypes.HeaderLoadMS, strconv.FormatInt(res.LoadMS, 10))
	if strings.EqualFold(r.Header.Get(apitypes.HeaderLogType), apitypes.LogTypeTail) {
		setLogTail(h, res.Stderr)
	}

	if res.FunctionError != "" {
		code := res.ExitCode
		writeJSON(w, http.StatusBadGateway, apitypes.Error{Error: res.FunctionError, ExitCode: &code, Stderr: res.Stderr})
		return
	}
	h.Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(res.Output)
}

// runtimeName is the function's runtime as logs, metrics and events show it.
func runtimeName(f registry.Function) string {
	if f.Runtime == "" {
		return "binary"
	}
	return f.Runtime
}

// readPayload reads and checks the request body (the function's JSON input). On a bad body it
// has already answered and returns false.
func (s *Server) readPayload(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, s.MaxPayloadBytes)
	payload, err := io.ReadAll(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload larger than the limit")
		} else {
			writeError(w, http.StatusBadRequest, "cannot read payload: "+err.Error())
		}
		return nil, false
	}
	if len(bytes.TrimSpace(payload)) == 0 {
		payload = []byte("null") // an empty body is a call with no input
	}
	if !json.Valid(payload) {
		writeError(w, http.StatusBadRequest, "payload is not valid JSON")
		return nil, false
	}
	return payload, true
}

// codeError is a failure to get the function's code (cache or artifact store), not to run it.
type codeError struct{ err error }

func (e codeError) Error() string { return "get code: " + e.err.Error() }
func (e codeError) Unwrap() error { return e.err }

func isCodeError(err error) bool {
	var c codeError
	return errors.As(err, &c)
}

// runCall runs one call of f: get the code (local cache first, else the artifact store), then
// the runner. It fills info (cache, fetch time, VM, result) for the log, metrics and events.
// Used by the synchronous handler and by the async workers.
func (s *Server) runCall(ctx context.Context, tenantID int64, f registry.Function, payload []byte, info *reqInfo) (runner.Result, error) {
	fetchStart := time.Now()
	codePath, cacheHit, err := s.Code.Ensure(ctx, f.CodeSHA256)
	info.fetchMS = time.Since(fetchStart).Milliseconds()
	info.cache = "miss"
	if cacheHit {
		info.cache, info.fetchMS = "hit", 0
	}
	if err != nil {
		return runner.Result{}, codeError{err}
	}
	req := functionRequest(tenantID, f, codePath)
	req.Payload = payload
	res, err := s.Runner.Run(ctx, req)
	info.vmID, info.start, info.result = res.VMID, res.StartKind, &res
	return res, err
}

// setLogTail puts the end of a function's log into the response headers (see
// apitypes.HeaderLogResult). The cut is moved forward to the start of a UTF-8 character, so the
// decoded text never begins with half a character.
func setLogTail(h http.Header, log string) {
	h.Set(apitypes.HeaderLogBytes, strconv.Itoa(len(log)))
	if log == "" {
		return
	}
	tail := logTail(log, apitypes.MaxLogTailBytes)
	h.Set(apitypes.HeaderLogResult, base64.StdEncoding.EncodeToString([]byte(tail)))
}

// logTail returns at most max bytes from the end of s, starting at a character boundary.
func logTail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	start := len(s) - max
	for i := 0; i < utf8.UTFMax-1 && start < len(s) && !utf8.RuneStart(s[start]); i++ {
		start++
	}
	return s[start:]
}
