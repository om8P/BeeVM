package codestore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestPutStoresUnderItsHash(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("some function code")
	sha, size, err := s.Put(bytes.NewReader(data), 1000)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if sha != hex.EncodeToString(sum[:]) || size != int64(len(data)) {
		t.Errorf("got %s, %d", sha, size)
	}
	p, err := s.Path(sha)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); !bytes.Equal(got, data) {
		t.Error("stored bytes differ")
	}
}

func TestSameBytesStoredOnce(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	a, _, _ := s.Put(strings.NewReader("same"), 100)
	b, _, _ := s.Put(strings.NewReader("same"), 100)
	if a != b {
		t.Fatal("hashes differ")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("want 1 file (and no leftover temp files), got %d", len(entries))
	}
}

func TestPutSizeLimit(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	if _, _, err := s.Put(strings.NewReader("12345"), 5); err != nil {
		t.Errorf("exactly at the limit should work: %v", err)
	}
	if _, _, err := s.Put(strings.NewReader("123456"), 5); !errors.Is(err, ErrTooLarge) {
		t.Errorf("want ErrTooLarge, got %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("a rejected upload must leave nothing behind, got %d files", len(entries))
	}
}

func TestPutEmptyRejected(t *testing.T) {
	s, _ := New(t.TempDir())
	if _, _, err := s.Put(strings.NewReader(""), 100); err == nil {
		t.Error("empty code should be rejected")
	}
}

func TestPathAndDelete(t *testing.T) {
	s, _ := New(t.TempDir())
	sha, _, _ := s.Put(strings.NewReader("x"), 10)

	if _, err := s.Path("../etc/passwd"); err == nil {
		t.Error("a path-like hash must be rejected")
	}
	if _, err := s.Path(strings.Repeat("a", 64)); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
	if err := s.Delete(sha); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Path(sha); !errors.Is(err, ErrNotFound) {
		t.Error("code should be gone")
	}
	if err := s.Delete(sha); err != nil {
		t.Errorf("deleting twice must not fail: %v", err)
	}
	if err := s.Delete("not-a-hash"); err == nil {
		t.Error("bad hash must be rejected")
	}
}
