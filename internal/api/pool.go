package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/om8P/BeeVM/internal/apitypes"
	"github.com/om8P/BeeVM/internal/registry"
	"github.com/om8P/BeeVM/internal/runner"
)

// drain throws away a function's warm VMs, if the runner has a warm pool.
func (s *Server) drain(functionID int64) {
	if d, ok := s.Runner.(runner.Drainer); ok {
		d.Drain(functionID)
	}
}

// prewarm starts idle VMs for a function so its next calls are warm.
//
//	POST /api/v1/functions/{name}/prewarm?count=N   (N defaults to 1)
//	200 {"requested":N,"started":n}    404 no such function
//	400 bad count                      501 this server runs without a warm pool
//	503 no free VM slot
func (s *Server) prewarm(w http.ResponseWriter, r *http.Request) {
	pw, ok := s.Runner.(runner.Prewarmer)
	if !ok {
		writeError(w, http.StatusNotImplemented, "this server runs without a warm pool (start it with --runner warm)")
		return
	}
	limit := 64
	if m, ok := s.Runner.(interface{ MaxVMs() int }); ok {
		limit = m.MaxVMs()
	}
	count := 1
	if q := r.URL.Query().Get("count"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n < 1 || n > limit {
			writeError(w, http.StatusBadRequest, "count must be between 1 and "+strconv.Itoa(limit))
			return
		}
		count = n
	}

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
	codePath, _, err := s.Code.Ensure(r.Context(), f.CodeSHA256)
	if err != nil {
		s.Logger.Error("cannot get code for prewarm", "function", f.Name, "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	started, err := pw.Prewarm(r.Context(), runner.Request{
		CodePath: codePath, MemoryMiB: f.MemoryMiB, VCPUs: f.VCPUs, TimeoutMS: f.TimeoutMS,
		TenantID: tenant.ID, FunctionID: f.ID, CodeSHA256: f.CodeSHA256,
	}, count)
	switch {
	case errors.Is(err, runner.ErrBusy):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	case errors.Is(err, context.Canceled) && r.Context().Err() != nil:
		w.WriteHeader(statusClientClosed)
		return
	case err != nil && started == 0:
		s.Logger.Error("prewarm", "function", f.Name, "error", err)
		writeError(w, http.StatusInternalServerError, "internal error starting VMs")
		return
	case err != nil:
		s.Logger.Warn("prewarm started only some VMs", "function", f.Name, "started", started, "error", err)
	}
	writeJSON(w, http.StatusOK, apitypes.PrewarmResult{Requested: count, Started: started})
}

// listVMs shows the caller's VMs (never another tenant's). Without a warm pool there are none
// between calls, so the list is empty.
func (s *Server) listVMs(w http.ResponseWriter, r *http.Request) {
	out := apitypes.VMList{VMs: []apitypes.VM{}}
	lister, ok := s.Runner.(runner.VMLister)
	if !ok {
		writeJSON(w, http.StatusOK, out)
		return
	}
	tenant := tenantFrom(r.Context())
	fns, err := s.Registry.ListFunctions(tenant.ID)
	if err != nil {
		s.Logger.Error("list functions", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	names := map[int64]string{}
	for _, f := range fns {
		names[f.ID] = f.Name
	}
	for _, v := range lister.VMs(tenant.ID) {
		out.VMs = append(out.VMs, apitypes.VM{ID: v.ID, Function: names[v.FunctionID], State: v.State.String(),
			Since: v.Since, LastUsed: v.LastUsed, Calls: v.Calls})
	}
	writeJSON(w, http.StatusOK, out)
}
