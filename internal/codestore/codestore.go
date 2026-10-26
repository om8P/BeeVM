// Package codestore keeps function binaries on local disk, named by their own SHA-256
// ("content-addressed"). The same bytes are stored once, changed bytes get a new name, and a
// later milestone can use the hash as the cache key when code comes from S3.
package codestore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/om8P/BeeVM/internal/store"
)

// ErrTooLarge is returned by Put when the data exceeds the size limit.
var ErrTooLarge = errors.New("code too large")

// ErrNotFound is returned when no file has the given hash.
var ErrNotFound = errors.New("code not found")

var shaRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ErrCorrupt is returned when downloaded bytes do not match the hash they were stored under.
var ErrCorrupt = errors.New("downloaded code does not match its hash")

// keyPrefix is the "folder" inside the bucket where function code lives.
const keyPrefix = "functions/"

// Store is a folder of binaries. With a remote set (WithRemote) the folder is a cache of an
// artifact store such as S3: the remote is the source of truth, and a missing local file is
// downloaded on first use. Without a remote it is simply local storage.
type Store struct {
	dir    string
	remote store.Blobs
	group  singleflight.Group // makes parallel downloads of the same hash share one download
}

// New opens (and creates) the store folder.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create code store: %w", err)
	}
	return &Store{dir: dir}, nil
}

// WithRemote makes r the source of truth for this store and returns s for chaining.
func (s *Store) WithRemote(r store.Blobs) *Store {
	s.remote = r
	return s
}

// HasRemote reports whether an artifact store is configured.
func (s *Store) HasRemote() bool { return s.remote != nil }

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

// Delete removes the code with this hash from the cache and from the remote. A missing file
// is not an error. If the remote delete fails the local copy is still removed and the error
// is returned (the leftover object is harmless: nothing refers to it).
func (s *Store) Delete(sha string) error {
	if !shaRE.MatchString(sha) {
		return fmt.Errorf("bad code hash %q", sha)
	}
	err := os.Remove(filepath.Join(s.dir, sha))
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if s.remote != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if rerr := s.remote.Delete(ctx, keyPrefix+sha); rerr != nil && err == nil {
			err = rerr
		}
	}
	return err
}

// Publish makes sure the remote has the code with this hash, uploading it from the local
// folder if not. Without a remote it does nothing. Identical code is uploaded only once.
func (s *Store) Publish(ctx context.Context, sha string) error {
	if s.remote == nil {
		return nil
	}
	path, err := s.Path(sha)
	if err != nil {
		return err
	}
	exists, err := s.remote.Exists(ctx, keyPrefix+sha)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	return s.remote.Put(ctx, keyPrefix+sha, f, st.Size())
}

// Ensure returns the local path of the code with this hash, downloading it from the remote
// first if it is not cached. hit is true when no download was needed.
//
// The hash is the file's name AND its checksum, so a cached file can never be out of date,
// and a download is checked against the hash before it is accepted.
func (s *Store) Ensure(ctx context.Context, sha string) (path string, hit bool, err error) {
	path, err = s.Path(sha)
	if err == nil {
		return path, true, nil
	}
	if !errors.Is(err, ErrNotFound) || s.remote == nil {
		return "", false, err
	}
	// Many calls to the same cold function arrive together; only one of them downloads.
	// The shared download must not die when the first caller hangs up, so it gets its own context.
	_, err, _ = s.group.Do(sha, func() (any, error) {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()
		return nil, s.download(dctx, sha)
	})
	if err != nil {
		return "", false, err
	}
	path, err = s.Path(sha)
	return path, false, err
}

// download fetches one object into a temp file, checks its hash and moves it into place.
func (s *Store) download(ctx context.Context, sha string) error {
	if _, err := s.Path(sha); err == nil {
		return nil // another download finished just before we got here
	}
	rc, err := s.remote.Get(ctx, keyPrefix+sha)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("code %s is not in the artifact store: %w", sha, ErrNotFound)
		}
		return err
	}
	defer rc.Close()
	tmp, err := os.CreateTemp(s.dir, "download-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), rc)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != sha {
		return ErrCorrupt
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(s.dir, sha))
}
