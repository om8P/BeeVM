package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"

	"github.com/go-chi/chi/v5"

	"github.com/om8P/BeeVM/internal/agentproto"
	"github.com/om8P/BeeVM/internal/apitypes"
	"github.com/om8P/BeeVM/internal/codestore"
	"github.com/om8P/BeeVM/internal/pkg"
	"github.com/om8P/BeeVM/internal/registry"
	"github.com/om8P/BeeVM/internal/runner"
)

// isELF reports whether the file starts with the ELF magic bytes (a Linux executable).
func isELF(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	magic := make([]byte, 4)
	if _, err := io.ReadFull(f, magic); err != nil {
		return false
	}
	return bytes.Equal(magic, []byte("\x7fELF"))
}

// checkCode checks uploaded code for its runtime and returns a message for the caller, or ""
// when it is fine: binaries must be Linux executables, Python and Node.js code must be a zip
// package that follows the package rules and contains the handler's file.
func (s *Server) checkCode(path, runtime, handler string) string {
	if !pkg.ValidRuntime(runtime) {
		return "runtime must be binary, python or node"
	}
	if c, ok := s.Runner.(runner.RuntimeChecker); ok && !c.HasRuntime(runtime) {
		return "runtime " + runtime + " is not available on this server (its image is missing)"
	}
	if !pkg.IsPackaged(runtime) {
		if handler != "" {
			return "binaries have no handler"
		}
		if !isELF(path) {
			return "code is not a Linux executable (ELF); build a static binary for Linux x86-64, or deploy Python/Node.js code with runtime python or node"
		}
		return ""
	}
	if err := pkg.Check(path, runtime, handler); err != nil {
		return err.Error()
	}
	return ""
}

// functionRequest describes a call of f for the runner (without a payload).
func functionRequest(tenantID int64, f registry.Function, codePath string) runner.Request {
	return runner.Request{
		CodePath: codePath, Runtime: f.Runtime, Handler: f.Handler,
		MemoryMiB: f.MemoryMiB, VCPUs: f.VCPUs, TimeoutMS: f.TimeoutMS,
		TenantID: tenantID, FunctionID: f.ID, CodeSHA256: f.CodeSHA256,
	}
}

// prepare starts the background build of a function's snapshot, if the runner can do that.
func (s *Server) prepare(req runner.Request) {
	if p, ok := s.Runner.(runner.Preparer); ok {
		p.Prepare(req)
	}
}

// forget deletes a function's snapshots, if the runner has any.
func (s *Server) forget(functionID int64) {
	if f, ok := s.Runner.(runner.SnapshotForgetter); ok {
		f.ForgetFunction(functionID)
	}
}

// releaseCode deletes stored code that no function uses any more.
func (s *Server) releaseCode(sha string) {
	if sha == "" {
		return
	}
	if inUse, err := s.Registry.CodeInUse(sha); err == nil && !inUse {
		_ = s.Code.Delete(sha)
	}
}

// putFunction deploys (creates or replaces) a function.
// The body is multipart/form-data: a "code" file and an optional "config" JSON field.
func (s *Server) putFunction(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	name := chi.URLParam(r, "name")
	if !registry.ValidName(name) {
		writeError(w, http.StatusBadRequest, registry.ErrInvalidName.Error())
		return
	}

	// A little more than the code limit: the multipart framing and the config field.
	r.Body = http.MaxBytesReader(w, r.Body, s.MaxUploadBytes+(1<<20))
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "expected multipart/form-data with a \"code\" file")
		return
	}

	var (
		hash     string
		codeSize int64
		cfg      apitypes.FunctionConfig
	)
	haveCode := false
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			s.uploadError(w, err, hash)
			return
		}
		switch part.FormName() {
		case "code":
			if haveCode {
				s.releaseCode(hash)
				writeError(w, http.StatusBadRequest, "more than one \"code\" part")
				return
			}
			hash, codeSize, err = s.Code.Put(part, s.MaxUploadBytes)
			if err != nil {
				s.uploadError(w, err, "")
				return
			}
			haveCode = true
		case "config":
			dec := json.NewDecoder(io.LimitReader(part, 4096))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&cfg); err != nil {
				s.releaseCode(hash)
				writeError(w, http.StatusBadRequest, "bad \"config\": "+err.Error())
				return
			}
		}
	}
	if !haveCode {
		writeError(w, http.StatusBadRequest, "missing \"code\" file")
		return
	}

	path, err := s.Code.Path(hash)
	if err != nil {
		s.Logger.Error("stored code vanished", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	runtime := cfg.Runtime
	if runtime == "" {
		runtime = agentproto.RuntimeBinary
	}
	if msg := s.checkCode(path, runtime, cfg.Handler); msg != "" {
		s.releaseCode(hash)
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	// Upload to the artifact store BEFORE registering, so a registered function always has its
	// code there. (No-op without an artifact store.)
	if err := s.Code.Publish(r.Context(), hash); err != nil {
		s.releaseCode(hash)
		s.Logger.Error("publish code to artifact store", "error", err)
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "artifact store unavailable, try again shortly")
		return
	}

	f, created, oldSHA, err := s.Registry.PutFunction(tenant.ID, name, registry.Spec{
		Runtime: runtime, Handler: cfg.Handler,
		CodeSHA256: hash, CodeSize: codeSize,
		MemoryMiB: cfg.MemoryMiB, VCPUs: cfg.VCPUs, TimeoutMS: cfg.TimeoutMS,
	})
	if err != nil {
		s.releaseCode(hash)
		if errors.Is(err, registry.ErrInvalidSpec) || errors.Is(err, registry.ErrInvalidName) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.Logger.Error("put function", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if oldSHA != hash {
		s.releaseCode(oldSHA)
	}
	if !created {
		s.drain(f.ID)  // warm VMs still hold the old code or settings
		s.forget(f.ID) // so do its snapshots
	}
	// Build the function snapshot in the background, so the next calls restore with the code
	// already inside. The deploy answer does not wait for it.
	s.prepare(functionRequest(tenant.ID, f, path))
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, toAPIFunction(f))
}

// uploadError answers for a failed upload and removes code that was just stored.
func (s *Server) uploadError(w http.ResponseWriter, err error, storedSHA string) {
	s.releaseCode(storedSHA)
	var tooBig *http.MaxBytesError
	switch {
	case errors.Is(err, codestore.ErrTooLarge) || errors.As(err, &tooBig):
		writeError(w, http.StatusRequestEntityTooLarge, "code larger than the upload limit")
	default:
		writeError(w, http.StatusBadRequest, "bad upload: "+err.Error())
	}
}

func (s *Server) listFunctions(w http.ResponseWriter, r *http.Request) {
	list, err := s.Registry.ListFunctions(tenantFrom(r.Context()).ID)
	if err != nil {
		s.Logger.Error("list functions", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := apitypes.FunctionList{Functions: make([]apitypes.Function, 0, len(list))}
	for _, f := range list {
		out.Functions = append(out.Functions, toAPIFunction(f))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getFunction(w http.ResponseWriter, r *http.Request) {
	f, err := s.Registry.GetFunction(tenantFrom(r.Context()).ID, chi.URLParam(r, "name"))
	if errors.Is(err, registry.ErrNotFound) {
		writeError(w, http.StatusNotFound, "function not found")
		return
	}
	if err != nil {
		s.Logger.Error("get function", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, toAPIFunction(f))
}

func (s *Server) deleteFunction(w http.ResponseWriter, r *http.Request) {
	tenantID, name := tenantFrom(r.Context()).ID, chi.URLParam(r, "name")
	if f, err := s.Registry.GetFunction(tenantID, name); err == nil {
		// After the delete, so no new call can start a VM for it (deferred calls run last first).
		defer s.forget(f.ID)
		defer s.drain(f.ID)
	}
	sha, err := s.Registry.DeleteFunction(tenantID, name)
	if errors.Is(err, registry.ErrNotFound) {
		writeError(w, http.StatusNotFound, "function not found")
		return
	}
	if err != nil {
		s.Logger.Error("delete function", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.releaseCode(sha)
	w.WriteHeader(http.StatusNoContent)
}
