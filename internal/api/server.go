// Package api is BeeVM's public HTTP API: deploy and manage functions, and invoke them.
//
// It depends on the registry (who and what), the code store (function binaries) and the
// Runner interface (how a function actually runs), so tests can replace the Runner with a
// fake and need no VM.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/om8P/BeeVM/internal/agentproto"
	"github.com/om8P/BeeVM/internal/apitypes"
	"github.com/om8P/BeeVM/internal/codestore"
	"github.com/om8P/BeeVM/internal/registry"
	"github.com/om8P/BeeVM/internal/runner"
)

// Deps are the things the server needs. MaxUploadBytes and MaxPayloadBytes default to the
// limits the guest agent enforces.
type Deps struct {
	Registry        *registry.Registry
	Code            *codestore.Store
	Runner          runner.Runner
	Logger          *slog.Logger
	MaxUploadBytes  int64
	MaxPayloadBytes int64
}

// Server serves the API.
type Server struct{ Deps }

// New creates a Server.
func New(d Deps) *Server {
	if d.MaxUploadBytes == 0 {
		d.MaxUploadBytes = agentproto.MaxUploadBytes
	}
	if d.MaxPayloadBytes == 0 {
		d.MaxPayloadBytes = agentproto.MaxPayloadBytes
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &Server{d}
}

// Handler returns the HTTP handler with all routes.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(s.logRequests, s.recoverPanics)

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	r.Group(func(r chi.Router) {
		r.Use(s.authenticate)
		r.Route("/api/v1/functions", func(r chi.Router) {
			r.Get("/", s.listFunctions)
			r.Put("/{name}", s.putFunction)
			r.Get("/{name}", s.getFunction)
			r.Delete("/{name}", s.deleteFunction)
		})
		r.Post("/invoke/{name}", s.invoke)
	})
	return r
}

// ---------------------------------------------------------------- helpers

type ctxKey int

const (
	tenantKey ctxKey = iota
	infoKey
)

// reqInfo is filled in as a request moves along, and logged at the end.
type reqInfo struct {
	tenant string
	vmID   string
}

func tenantFrom(ctx context.Context) registry.Tenant {
	t, _ := ctx.Value(tenantKey).(registry.Tenant)
	return t
}

func infoFrom(ctx context.Context) *reqInfo {
	i, _ := ctx.Value(infoKey).(*reqInfo)
	if i == nil {
		i = &reqInfo{} // not behind the logging middleware (should not happen)
	}
	return i
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, apitypes.Error{Error: msg})
}

func toAPIFunction(f registry.Function) apitypes.Function {
	return apitypes.Function{
		Name: f.Name, Runtime: f.Runtime, CodeSHA256: f.CodeSHA256, CodeSize: f.CodeSize,
		MemoryMiB: f.MemoryMiB, VCPUs: f.VCPUs, TimeoutMS: f.TimeoutMS,
		CreatedAt: f.CreatedAt, UpdatedAt: f.UpdatedAt,
	}
}

// ---------------------------------------------------------------- middleware

// logRequests writes one JSON log line per request.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := &reqInfo{}
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		start := time.Now()
		next.ServeHTTP(ww, r.WithContext(context.WithValue(r.Context(), infoKey, info)))
		status := ww.Status()
		if status == 0 {
			status = http.StatusOK
		}
		s.Logger.Info("request",
			"method", r.Method, "path", r.URL.Path, "tenant", info.tenant,
			"status", status, "duration_ms", time.Since(start).Milliseconds(), "vm", info.vmID)
	})
}

// recoverPanics turns a crash inside a handler into a clean 500 instead of a dropped connection.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.Logger.Error("panic in handler", "panic", v, "stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// authenticate checks "Authorization: Bearer <key>" and puts the tenant in the request context.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if len(h) <= 7 || !strings.EqualFold(h[:7], "bearer ") {
			unauthorized(w)
			return
		}
		tenant, err := s.Registry.Authenticate(strings.TrimSpace(h[7:]))
		if errors.Is(err, registry.ErrInvalidKey) {
			unauthorized(w)
			return
		}
		if err != nil {
			s.Logger.Error("authenticate", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		infoFrom(r.Context()).tenant = tenant.Name
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tenantKey, tenant)))
	})
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="beevm"`)
	writeError(w, http.StatusUnauthorized, "missing or invalid API key")
}
