package registry

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Limits for a function's settings. They match what the guest agent can enforce.
const (
	MinMemoryMiB, MaxMemoryMiB = 64, 1024
	MinVCPUs, MaxVCPUs         = 1, 4
	MinTimeoutMS, MaxTimeoutMS = 1, 60000

	DefaultMemoryMiB = 128
	DefaultVCPUs     = 1
	DefaultTimeoutMS = 10000
)

// Function is a deployed function.
type Function struct {
	ID         int64
	TenantID   int64
	Name       string
	Runtime    string // "binary" for now
	CodeSHA256 string
	CodeSize   int64
	MemoryMiB  int64
	VCPUs      int64
	TimeoutMS  int64
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Spec is what a deploy provides. Zero MemoryMiB, VCPUs or TimeoutMS mean "use the default".
type Spec struct {
	CodeSHA256 string
	CodeSize   int64
	MemoryMiB  int64
	VCPUs      int64
	TimeoutMS  int64
}

// withDefaults fills empty settings and checks the ranges.
func (s Spec) withDefaults() (Spec, error) {
	if s.MemoryMiB == 0 {
		s.MemoryMiB = DefaultMemoryMiB
	}
	if s.VCPUs == 0 {
		s.VCPUs = DefaultVCPUs
	}
	if s.TimeoutMS == 0 {
		s.TimeoutMS = DefaultTimeoutMS
	}
	switch {
	case s.MemoryMiB < MinMemoryMiB || s.MemoryMiB > MaxMemoryMiB:
		return s, fmt.Errorf("%w: memory_mib must be %d-%d", ErrInvalidSpec, MinMemoryMiB, MaxMemoryMiB)
	case s.VCPUs < MinVCPUs || s.VCPUs > MaxVCPUs:
		return s, fmt.Errorf("%w: vcpus must be %d-%d", ErrInvalidSpec, MinVCPUs, MaxVCPUs)
	case s.TimeoutMS < MinTimeoutMS || s.TimeoutMS > MaxTimeoutMS:
		return s, fmt.Errorf("%w: timeout_ms must be %d-%d", ErrInvalidSpec, MinTimeoutMS, MaxTimeoutMS)
	case len(s.CodeSHA256) != 64:
		return s, fmt.Errorf("%w: code hash missing", ErrInvalidSpec)
	}
	return s, nil
}

const functionColumns = "id, tenant_id, name, runtime, code_sha256, code_size, memory_mib, vcpus, timeout_ms, created_at, updated_at"

type scanner interface{ Scan(dest ...any) error }

func scanFunction(s scanner) (Function, error) {
	var f Function
	var created, updated int64
	err := s.Scan(&f.ID, &f.TenantID, &f.Name, &f.Runtime, &f.CodeSHA256, &f.CodeSize,
		&f.MemoryMiB, &f.VCPUs, &f.TimeoutMS, &created, &updated)
	f.CreatedAt, f.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
	return f, err
}

// PutFunction creates or replaces a tenant's function. It returns the stored function,
// whether it was newly created, and the code hash it replaced (empty if new), so the caller
// can delete the old code if nothing else uses it.
func (r *Registry) PutFunction(tenantID int64, name string, spec Spec) (f Function, created bool, oldSHA string, err error) {
	if !ValidName(name) {
		return Function{}, false, "", ErrInvalidName
	}
	spec, err = spec.withDefaults()
	if err != nil {
		return Function{}, false, "", err
	}
	tx, err := r.db.Begin()
	if err != nil {
		return Function{}, false, "", err
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	row := tx.QueryRow("SELECT "+functionColumns+" FROM functions WHERE tenant_id = ? AND name = ?", tenantID, name)
	existing, err := scanFunction(row)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		created = true
		_, err = tx.Exec(`INSERT INTO functions(tenant_id, name, runtime, code_sha256, code_size, memory_mib, vcpus, timeout_ms, created_at, updated_at)
			VALUES(?, ?, 'binary', ?, ?, ?, ?, ?, ?, ?)`,
			tenantID, name, spec.CodeSHA256, spec.CodeSize, spec.MemoryMiB, spec.VCPUs, spec.TimeoutMS, now, now)
	case err != nil:
		return Function{}, false, "", err
	default:
		oldSHA = existing.CodeSHA256
		_, err = tx.Exec(`UPDATE functions SET code_sha256 = ?, code_size = ?, memory_mib = ?, vcpus = ?, timeout_ms = ?, updated_at = ?
			WHERE id = ?`, spec.CodeSHA256, spec.CodeSize, spec.MemoryMiB, spec.VCPUs, spec.TimeoutMS, now, existing.ID)
	}
	if err != nil {
		return Function{}, false, "", err
	}
	f, err = scanFunction(tx.QueryRow("SELECT "+functionColumns+" FROM functions WHERE tenant_id = ? AND name = ?", tenantID, name))
	if err != nil {
		return Function{}, false, "", err
	}
	if err := tx.Commit(); err != nil {
		return Function{}, false, "", err
	}
	return f, created, oldSHA, nil
}

// GetFunction finds one of a tenant's functions.
func (r *Registry) GetFunction(tenantID int64, name string) (Function, error) {
	f, err := scanFunction(r.db.QueryRow("SELECT "+functionColumns+" FROM functions WHERE tenant_id = ? AND name = ?", tenantID, name))
	if errors.Is(err, sql.ErrNoRows) {
		return Function{}, fmt.Errorf("function %q: %w", name, ErrNotFound)
	}
	return f, err
}

// ListFunctions returns a tenant's functions, sorted by name.
func (r *Registry) ListFunctions(tenantID int64) ([]Function, error) {
	rows, err := r.db.Query("SELECT "+functionColumns+" FROM functions WHERE tenant_id = ? ORDER BY name", tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Function
	for rows.Next() {
		f, err := scanFunction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// DeleteFunction removes a function and returns its code hash.
func (r *Registry) DeleteFunction(tenantID int64, name string) (string, error) {
	f, err := r.GetFunction(tenantID, name)
	if err != nil {
		return "", err
	}
	if _, err := r.db.Exec("DELETE FROM functions WHERE id = ?", f.ID); err != nil {
		return "", err
	}
	return f.CodeSHA256, nil
}

// CodeInUse reports whether any function still uses the code with this hash.
func (r *Registry) CodeInUse(sha string) (bool, error) {
	var n int
	err := r.db.QueryRow("SELECT COUNT(*) FROM functions WHERE code_sha256 = ?", sha).Scan(&n)
	return n > 0, err
}
