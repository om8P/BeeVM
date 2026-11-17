package api

import (
	"context"

	"github.com/om8P/BeeVM/internal/runner"
)

// PrepareSnapshots asks the runner to build the function snapshot of every deployed function
// that has none, so the first call after a server restart (or after `make images` made the
// old snapshots stale) is as fast as any other. The runner skips functions whose snapshot
// exists and builds the others in the background, one at a time. Code that is only in the
// artifact store is fetched first. Returns how many functions were checked. serve calls it
// once at start; ctx ends it early.
func (s *Server) PrepareSnapshots(ctx context.Context) (int, error) {
	if _, ok := s.Runner.(runner.Preparer); !ok {
		return 0, nil
	}
	tenants, err := s.Registry.ListTenants()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, t := range tenants {
		fns, err := s.Registry.ListFunctions(t.ID)
		if err != nil {
			return n, err
		}
		for _, f := range fns {
			if err := ctx.Err(); err != nil {
				return n, err
			}
			path, _, err := s.Code.Ensure(ctx, f.CodeSHA256)
			if err != nil {
				s.Logger.Warn("cannot get code to prepare a function snapshot", "tenant", t.Name, "function", f.Name, "error", err)
				continue
			}
			s.prepare(functionRequest(t.ID, f, path))
			n++
		}
	}
	return n, nil
}
