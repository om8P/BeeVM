package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/om8P/BeeVM/internal/apitypes"
	"github.com/om8P/BeeVM/internal/snapshot"
)

// SnapshotStore is what the snapshot routes need from the Snapshot Manager (an interface, so
// tests can use a fake). The manager reads snapshots from disk on every use, so one removed
// here is simply built again the next time it is needed.
type SnapshotStore interface {
	List() ([]snapshot.Info, error)
	Remove(key string) error
}

// Two kinds of snapshot, two rules:
//   - a function snapshot belongs to one tenant's function: its tenant may delete it. Its next
//     cold start restores the runtime snapshot and imports the code (a few tens of ms slower),
//     and that call builds the function snapshot again in the background.
//   - a runtime snapshot is shared by every tenant on the node: deleting it makes everyone's
//     next cold start of that runtime wait for a rebuild (2-3 s). Tenants may only do that when
//     the operator allows it (serve --allow-runtime-snapshot-delete); otherwise it is
//     `beevmd snapshot rm` on the node.

// listSnapshots answers GET /api/v1/snapshots: every runtime snapshot of the node and the
// caller's own function snapshots (other tenants' are never shown).
//
//	200 apitypes.SnapshotList       501 this server keeps no snapshots (--runner cold)
func (s *Server) listSnapshots(w http.ResponseWriter, r *http.Request) {
	if s.Snapshots == nil {
		writeError(w, http.StatusNotImplemented, "this server keeps no snapshots (it runs with --runner cold)")
		return
	}
	tenant := tenantFrom(r.Context())
	list, err := s.Snapshots.List()
	if err != nil {
		s.Logger.Error("list snapshots", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	names, err := s.functionNames(tenant.ID)
	if err != nil {
		s.Logger.Error("list functions", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := apitypes.SnapshotList{
		Runtime: []apitypes.Snapshot{}, Function: []apitypes.Snapshot{},
		RuntimeDeleteAllowed: s.AllowRuntimeSnapshotDelete,
	}
	for _, info := range list {
		a := toAPISnapshot(info)
		if !info.IsFunction() {
			out.Runtime = append(out.Runtime, a)
			continue
		}
		if info.Function == nil || info.Function.TenantID != tenant.ID {
			continue
		}
		a.Function = names[info.Function.FunctionID]
		out.Function = append(out.Function, a)
	}
	writeJSON(w, http.StatusOK, out)
}

// deleteSnapshot answers DELETE /api/v1/snapshots/{key}.
//
//	204 removed      403 a runtime snapshot and the server does not allow it
//	404 no such snapshot, or a function snapshot of another tenant (never 403: other tenants'
//	    snapshots must not even be confirmed to exist)
//	501 this server keeps no snapshots
func (s *Server) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	if s.Snapshots == nil {
		writeError(w, http.StatusNotImplemented, "this server keeps no snapshots (it runs with --runner cold)")
		return
	}
	tenant := tenantFrom(r.Context())
	key := chi.URLParam(r, "key")
	info, ok, err := s.findSnapshot(key)
	if err != nil {
		s.Logger.Error("list snapshots", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !ok || (info.IsFunction() && (info.Function == nil || info.Function.TenantID != tenant.ID)) {
		writeError(w, http.StatusNotFound, "snapshot not found")
		return
	}
	if !info.IsFunction() && !s.AllowRuntimeSnapshotDelete {
		writeError(w, http.StatusForbidden, "runtime snapshots are shared by every tenant on this node; the operator can "+
			"remove them with beevmd snapshot rm, or allow it here with beevmd serve --allow-runtime-snapshot-delete")
		return
	}
	if err := s.Snapshots.Remove(key); err != nil {
		if errors.Is(err, snapshot.ErrNotFound) {
			writeError(w, http.StatusNotFound, "snapshot not found")
			return
		}
		s.Logger.Error("remove snapshot", "key", key, "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.Logger.Info("snapshot removed", "key", key, "kind", kindOf(info), "runtime", info.RuntimeName(), "tenant", tenant.Name)
	w.WriteHeader(http.StatusNoContent)
}

// findSnapshot looks a key up in the list (the list is the only place a snapshot's tenant is
// known before removing it).
func (s *Server) findSnapshot(key string) (snapshot.Info, bool, error) {
	if key == "" || strings.ContainsAny(key, `/\.`) {
		return snapshot.Info{}, false, nil
	}
	list, err := s.Snapshots.List()
	if err != nil {
		return snapshot.Info{}, false, err
	}
	for _, info := range list {
		if info.Key == key {
			return info, true, nil
		}
	}
	return snapshot.Info{}, false, nil
}

// functionNames maps a tenant's function IDs to their names.
func (s *Server) functionNames(tenantID int64) (map[int64]string, error) {
	fns, err := s.Registry.ListFunctions(tenantID)
	if err != nil {
		return nil, err
	}
	names := make(map[int64]string, len(fns))
	for _, f := range fns {
		names[f.ID] = f.Name
	}
	return names, nil
}

func kindOf(info snapshot.Info) string {
	if info.IsFunction() {
		return snapshot.KindFunction
	}
	return snapshot.KindRuntime
}

func toAPISnapshot(info snapshot.Info) apitypes.Snapshot {
	return apitypes.Snapshot{
		Key: info.Key, Kind: kindOf(info), Runtime: info.RuntimeName(),
		MemoryMiB: info.Inputs.MemMiB, VCPUs: info.Inputs.VCPUs, Isolation: info.Inputs.Isolation,
		SizeBytes: info.MemBytes, BuildMS: info.BuildMS, CreatedAt: info.CreatedAt, Current: info.Current,
	}
}
