package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

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

	r.Body = http.MaxBytesReader(w, r.Body, s.MaxPayloadBytes)
	payload, err := io.ReadAll(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload larger than the limit")
		} else {
			writeError(w, http.StatusBadRequest, "cannot read payload: "+err.Error())
		}
		return
	}
	if len(bytes.TrimSpace(payload)) == 0 {
		payload = []byte("null") // an empty body is a call with no input
	}
	if !json.Valid(payload) {
		writeError(w, http.StatusBadRequest, "payload is not valid JSON")
		return
	}

	codePath, err := s.Code.Path(f.CodeSHA256)
	if err != nil {
		s.Logger.Error("code missing for deployed function", "function", f.Name, "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	res, err := s.Runner.Run(r.Context(), runner.Request{
		CodePath: codePath, Payload: payload,
		MemoryMiB: f.MemoryMiB, VCPUs: f.VCPUs, TimeoutMS: f.TimeoutMS,
	})
	infoFrom(r.Context()).vmID = res.VMID
	switch {
	case errors.Is(err, runner.ErrBusy):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	case errors.Is(err, context.Canceled) && r.Context().Err() != nil:
		w.WriteHeader(statusClientClosed)
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

	if res.FunctionError != "" {
		code := res.ExitCode
		writeJSON(w, http.StatusBadGateway, apitypes.Error{Error: res.FunctionError, ExitCode: &code, Stderr: res.Stderr})
		return
	}
	h.Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(res.Output)
}
