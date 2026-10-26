// Package registry is BeeVM's small database: tenants, their API keys, and their functions.
//
// It uses SQLite through modernc.org/sqlite, a pure Go driver (no C compiler needed). The
// schema version lives in PRAGMA user_version and is upgraded automatically on Open.
package registry

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// Errors returned by the registry. Compare with errors.Is.
var (
	ErrNotFound    = errors.New("not found")
	ErrExists      = errors.New("already exists")
	ErrInvalidKey  = errors.New("invalid or revoked API key")
	ErrInvalidName = errors.New("invalid name (use lowercase letters, digits and '-', 1-63 characters, starting with a letter or digit)")
	ErrAmbiguous   = errors.New("prefix matches more than one key")
	ErrInvalidSpec = errors.New("invalid function settings")
)

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidName reports whether s is allowed as a tenant or function name. These names appear in
// URLs and file names, so the rules are strict.
func ValidName(s string) bool { return nameRE.MatchString(s) }

// Registry is the database handle.
type Registry struct{ db *sql.DB }

// Open opens (and creates, and upgrades) the database file at path.
func Open(path string) (*Registry, error) {
	// WAL lets `beevmd tenant add` write while `beevmd serve` is running.
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("open registry: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Registry{db: db}, nil
}

// Close closes the database.
func (r *Registry) Close() error { return r.db.Close() }

// migrations[i] upgrades the database from version i to i+1. Never edit an existing entry;
// add a new one at the end.
var migrations = []string{
	`CREATE TABLE tenants (
		id         INTEGER PRIMARY KEY,
		name       TEXT NOT NULL UNIQUE,
		created_at INTEGER NOT NULL
	);
	CREATE TABLE api_keys (
		id         INTEGER PRIMARY KEY,
		tenant_id  INTEGER NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
		key_hash   TEXT NOT NULL UNIQUE,
		prefix     TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		revoked_at INTEGER
	);
	CREATE TABLE functions (
		id          INTEGER PRIMARY KEY,
		tenant_id   INTEGER NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
		name        TEXT NOT NULL,
		runtime     TEXT NOT NULL DEFAULT 'binary',
		code_sha256 TEXT NOT NULL,
		code_size   INTEGER NOT NULL,
		memory_mib  INTEGER NOT NULL,
		vcpus       INTEGER NOT NULL,
		timeout_ms  INTEGER NOT NULL,
		created_at  INTEGER NOT NULL,
		updated_at  INTEGER NOT NULL,
		UNIQUE (tenant_id, name)
	);`,
}

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this program understands (%d)", version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply schema step %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// ---------------------------------------------------------------- tenants

// Tenant is one customer.
type Tenant struct {
	ID        int64
	Name      string
	CreatedAt time.Time
}

// CreateTenant adds a tenant.
func (r *Registry) CreateTenant(name string) (Tenant, error) {
	if !ValidName(name) {
		return Tenant{}, ErrInvalidName
	}
	now := time.Now()
	res, err := r.db.Exec("INSERT INTO tenants(name, created_at) VALUES(?, ?)", name, now.Unix())
	if isUnique(err) {
		return Tenant{}, fmt.Errorf("tenant %q: %w", name, ErrExists)
	}
	if err != nil {
		return Tenant{}, err
	}
	id, _ := res.LastInsertId()
	return Tenant{ID: id, Name: name, CreatedAt: time.Unix(now.Unix(), 0)}, nil
}

// GetTenant finds a tenant by name.
func (r *Registry) GetTenant(name string) (Tenant, error) {
	var t Tenant
	var created int64
	err := r.db.QueryRow("SELECT id, name, created_at FROM tenants WHERE name = ?", name).Scan(&t.ID, &t.Name, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Tenant{}, fmt.Errorf("tenant %q: %w", name, ErrNotFound)
	}
	t.CreatedAt = time.Unix(created, 0)
	return t, err
}

// ListTenants returns all tenants, oldest first.
func (r *Registry) ListTenants() ([]Tenant, error) {
	rows, err := r.db.Query("SELECT id, name, created_at FROM tenants ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tenant
	for rows.Next() {
		var t Tenant
		var created int64
		if err := rows.Scan(&t.ID, &t.Name, &created); err != nil {
			return nil, err
		}
		t.CreatedAt = time.Unix(created, 0)
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- API keys

// APIKey describes a stored key. The key itself is never stored, only its hash.
type APIKey struct {
	ID        int64
	TenantID  int64
	Tenant    string
	Prefix    string // first characters of the key, enough to recognise it
	CreatedAt time.Time
	Revoked   bool
}

// keyPrefixLen: "bvm_" plus 8 random characters.
const keyPrefixLen = 12

func hashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// newKey makes a random key: "bvm_" followed by 43 URL-safe characters (256 random bits).
func newKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "bvm_" + base64.RawURLEncoding.EncodeToString(b), nil
}

// CreateKey makes a new API key for a tenant and returns it in plain text. This is the only
// time the plain key exists: store it somewhere safe.
func (r *Registry) CreateKey(tenantID int64) (string, APIKey, error) {
	key, err := newKey()
	if err != nil {
		return "", APIKey{}, err
	}
	now := time.Now()
	res, err := r.db.Exec(
		"INSERT INTO api_keys(tenant_id, key_hash, prefix, created_at) VALUES(?, ?, ?, ?)",
		tenantID, hashKey(key), key[:keyPrefixLen], now.Unix())
	if err != nil {
		return "", APIKey{}, err
	}
	id, _ := res.LastInsertId()
	return key, APIKey{ID: id, TenantID: tenantID, Prefix: key[:keyPrefixLen], CreatedAt: time.Unix(now.Unix(), 0)}, nil
}

// Authenticate finds the tenant a key belongs to. Unknown and revoked keys both give
// ErrInvalidKey, so a caller cannot tell them apart.
func (r *Registry) Authenticate(key string) (Tenant, error) {
	var t Tenant
	var created int64
	err := r.db.QueryRow(`
		SELECT t.id, t.name, t.created_at
		FROM api_keys k JOIN tenants t ON t.id = k.tenant_id
		WHERE k.key_hash = ? AND k.revoked_at IS NULL`, hashKey(key)).Scan(&t.ID, &t.Name, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Tenant{}, ErrInvalidKey
	}
	t.CreatedAt = time.Unix(created, 0)
	return t, err
}

// ListKeys returns the keys of one tenant (or of everyone when tenant is empty).
func (r *Registry) ListKeys(tenant string) ([]APIKey, error) {
	q := `SELECT k.id, k.tenant_id, t.name, k.prefix, k.created_at, k.revoked_at IS NOT NULL
	      FROM api_keys k JOIN tenants t ON t.id = k.tenant_id`
	var args []any
	if tenant != "" {
		q += " WHERE t.name = ?"
		args = append(args, tenant)
	}
	rows, err := r.db.Query(q+" ORDER BY k.id", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		var k APIKey
		var created int64
		if err := rows.Scan(&k.ID, &k.TenantID, &k.Tenant, &k.Prefix, &created, &k.Revoked); err != nil {
			return nil, err
		}
		k.CreatedAt = time.Unix(created, 0)
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeKey disables the one active key that starts with prefix.
func (r *Registry) RevokeKey(prefix string) error {
	if len(prefix) < 6 {
		return fmt.Errorf("prefix %q is too short (give at least 6 characters)", prefix)
	}
	rows, err := r.db.Query("SELECT id FROM api_keys WHERE revoked_at IS NULL AND prefix LIKE ? ESCAPE '\\'", escapeLike(prefix)+"%")
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	switch len(ids) {
	case 0:
		return fmt.Errorf("no active key with prefix %q: %w", prefix, ErrNotFound)
	case 1:
		_, err := r.db.Exec("UPDATE api_keys SET revoked_at = ? WHERE id = ?", time.Now().Unix(), ids[0])
		return err
	default:
		return ErrAmbiguous
	}
}

func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	return strings.ReplaceAll(s, `_`, `\_`)
}
