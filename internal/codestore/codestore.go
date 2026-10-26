// Package codestore keeps function binaries on local disk, named by their own SHA-256
// ("content-addressed"). The same bytes are stored once, changed bytes get a new name, and a
// later milestone can use the hash as the cache key when code comes from S3.
package codestore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

// ErrTooLarge is returned by Put when the data exceeds the size limit.
var ErrTooLarge = errors.New("code too large")

// ErrNotFound is returned when no file has the given hash.
var ErrNotFound = errors.New("code not found")

var shaRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Store is a folder of binaries.
type Store struct{ dir string }

// New opens (and creates) the store folder.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create code store: %w", err)
	}
	return &Store{dir: dir}, nil
}

// Put reads r (at most max bytes) and stores it under its SHA-256. It returns the hash and size.
func (s *Store) Put(r io.Reader, max int64) (sha string, size int64, err error) {
	tmp, err := os.CreateTemp(s.dir, "upload-*")
	if err != nil {
		return "", 0, err
	}
	defer os.Remove(tmp.Name()) // gone already if the rename below worked

	h := sha256.New()
	// Read one byte past the limit so "exactly max" is fine and "more than max" is detected.
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r, max+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, err
	}
	if n > max {
		return "", 0, ErrTooLarge
	}
	if n == 0 {
		return "", 0, errors.New("empty code")
	}
	sha = hex.EncodeToString(h.Sum(nil))
	dest := filepath.Join(s.dir, sha)
	if _, err := os.Stat(dest); err == nil {
		return sha, n, nil // already stored; the temp file is removed by the defer
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return "", 0, err
	}
	if err := os.Rename(tmp.Name(), dest); err != nil {
		return "", 0, err
	}
	return sha, n, nil
}

// Path returns where the code with this hash is stored.
func (s *Store) Path(sha string) (string, error) {
	if !shaRE.MatchString(sha) {
		return "", fmt.Errorf("bad code hash %q", sha)
	}
	p := filepath.Join(s.dir, sha)
	if _, err := os.Stat(p); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", ErrNotFound
		}
		return "", err
	}
	return p, nil
}

// Delete removes the code with this hash. A missing file is not an error.
func (s *Store) Delete(sha string) error {
	if !shaRE.MatchString(sha) {
		return fmt.Errorf("bad code hash %q", sha)
	}
	err := os.Remove(filepath.Join(s.dir, sha))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
