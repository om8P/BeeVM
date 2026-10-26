package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"

	"github.com/go-chi/chi/v5"

	"github.com/om8P/BeeVM/internal/apitypes"
	"github.com/om8P/BeeVM/internal/codestore"
	"github.com/om8P/BeeVM/internal/registry"
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
	if !isELF(path) {
		s.releaseCode(hash)
		writeError(w, http.StatusBadRequest, "code is not a Linux executable (ELF); build a static binary for Linux x86-64")
		return
	}

	f, created, oldSHA, err := s.Registry.PutFunction(tenant.ID, name, registry.Spec{
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
	sha, err := s.Registry.DeleteFunction(tenantFrom(r.Context()).ID, chi.URLParam(r, "name"))
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
