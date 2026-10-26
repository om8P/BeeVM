package store

import (
	"bytes"
	"context"
	"io"
	"sync"
)

// Mem is an in-memory Blobs for tests. It counts calls and can be told to fail.
type Mem struct {
	mu      sync.Mutex
	objects map[string][]byte
	Gets    int   // number of Get calls
	Puts    int   // number of Put calls
	Fail    error // if set, every call returns this error (simulates the store being down)
}

// NewMem returns an empty store.
func NewMem() *Mem { return &Mem{objects: map[string][]byte{}} }

// Set stores bytes directly (also lets tests plant corrupted data).
func (m *Mem) Set(key string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = append([]byte(nil), data...)
}

// Has reports whether the key exists, ignoring Fail.
func (m *Mem) Has(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.objects[key]
	return ok
}

func (m *Mem) Put(_ context.Context, key string, body io.ReadSeeker, _ int64) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Puts++
	if m.Fail != nil {
		return m.Fail
	}
	m.objects[key] = data
	return nil
}

func (m *Mem) Get(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Gets++
	if m.Fail != nil {
		return nil, m.Fail
	}
	data, ok := m.objects[key]
	if !ok {
		return nil, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (m *Mem) Exists(_ context.Context, key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return false, m.Fail
	}
	_, ok := m.objects[key]
	return ok, nil
}

func (m *Mem) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	delete(m.objects, key)
	return nil
}
