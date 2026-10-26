// Package store is BeeVM's artifact store: a bucket of named blobs (function binaries now,
// snapshot backups later). The Blobs interface hides which server is behind it, so the rest
// of BeeVM does not care whether it talks to MinIO on a laptop or to AWS S3 in the cloud.
package store

import (
	"context"
	"errors"
	"io"
)

// ErrNotFound is returned when a key does not exist.
var ErrNotFound = errors.New("object not found")

// Blobs is a flat key -> bytes store. Keys look like "functions/<sha256>".
type Blobs interface {
	// Put uploads body (size bytes) under key, replacing any existing object.
	// body must be seekable: the client may need to read it twice (checksum, then upload).
	Put(ctx context.Context, key string, body io.ReadSeeker, size int64) error
	// Get opens the object. The caller closes it. Missing key gives ErrNotFound.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Exists reports whether the key is there (a cheap HEAD request, no download).
	Exists(ctx context.Context, key string) (bool, error)
	// Delete removes the object. A missing key is not an error.
	Delete(ctx context.Context, key string) error
}
