package codestore

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/om8P/BeeVM/internal/store"
)

func newRemote(t *testing.T) (*Store, *store.Mem) {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := store.NewMem()
	return s.WithRemote(m), m
}

func put(t *testing.T, s *Store, data string) string {
	t.Helper()
	sha, _, err := s.Put(strings.NewReader(data), 1000)
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

func TestPublishUploadsOnce(t *testing.T) {
	s, m := newRemote(t)
	sha := put(t, s, "code")
	for i := 0; i < 3; i++ {
		if err := s.Publish(context.Background(), sha); err != nil {
			t.Fatal(err)
		}
	}
	if !m.Has("functions/"+sha) || m.Puts != 1 {
		t.Errorf("has=%v puts=%d, want uploaded exactly once", m.Has("functions/"+sha), m.Puts)
	}
}

func TestPublishWithoutRemoteIsNoop(t *testing.T) {
	s, _ := New(t.TempDir())
	sha := put(t, s, "code")
	if err := s.Publish(context.Background(), sha); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureDownloadsThenHits(t *testing.T) {
	s, m := newRemote(t)
	sha := put(t, s, "function bytes")
	_ = s.Publish(context.Background(), sha)
	_ = os.Remove(mustPath(t, s, sha)) // empty the cache

	p, hit, err := s.Ensure(context.Background(), sha)
	if err != nil || hit {
		t.Fatalf("first Ensure: hit=%v err=%v, want a miss", hit, err)
	}
	if got, _ := os.ReadFile(p); string(got) != "function bytes" {
		t.Errorf("downloaded %q", got)
	}
	if _, hit, err = s.Ensure(context.Background(), sha); err != nil || !hit {
		t.Fatalf("second Ensure: hit=%v err=%v, want a hit", hit, err)
	}
	if m.Gets != 1 {
		t.Errorf("%d downloads, want 1", m.Gets)
	}
}

func TestEnsureRejectsCorruptDownload(t *testing.T) {
	s, m := newRemote(t)
	sha := put(t, s, "good")
	_ = os.Remove(mustPath(t, s, sha))
	m.Set("functions/"+sha, []byte("tampered"))

	if _, _, err := s.Ensure(context.Background(), sha); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("want ErrCorrupt, got %v", err)
	}
	if _, err := s.Path(sha); !errors.Is(err, ErrNotFound) {
		t.Error("corrupt bytes must not stay in the cache")
	}
	if left, _ := os.ReadDir(s.dir); len(left) != 0 {
		t.Errorf("temp files left behind: %v", left)
	}
}

func TestEnsureMissingEverywhere(t *testing.T) {
	s, _ := newRemote(t)
	sha := strings.Repeat("a", 64)
	if _, _, err := s.Ensure(context.Background(), sha); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestEnsureWithoutRemoteMissing(t *testing.T) {
	s, _ := New(t.TempDir())
	if _, _, err := s.Ensure(context.Background(), strings.Repeat("b", 64)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestEnsureRemoteDown(t *testing.T) {
	s, m := newRemote(t)
	sha := put(t, s, "x")
	_ = s.Publish(context.Background(), sha)
	_ = os.Remove(mustPath(t, s, sha))
	m.Fail = errors.New("connection refused")
	if _, _, err := s.Ensure(context.Background(), sha); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("want a plain remote error, got %v", err)
	}
}

func TestEnsureParallelMissesShareOneDownload(t *testing.T) {
	s, m := newRemote(t)
	sha := put(t, s, strings.Repeat("z", 500))
	_ = s.Publish(context.Background(), sha)
	_ = os.Remove(mustPath(t, s, sha))

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := s.Ensure(context.Background(), sha); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	// singleflight shares calls that overlap in time; a straggler that arrives after the
	// download finished is a cache hit. Either way the object is fetched very few times.
	if m.Gets < 1 || m.Gets > 2 {
		t.Errorf("%d downloads for 10 parallel calls, want 1 (2 at most)", m.Gets)
	}
}

func TestDeleteRemovesRemoteToo(t *testing.T) {
	s, m := newRemote(t)
	sha := put(t, s, "bye")
	_ = s.Publish(context.Background(), sha)
	if err := s.Delete(sha); err != nil {
		t.Fatal(err)
	}
	if m.Has("functions/" + sha) {
		t.Error("remote copy still there")
	}
	if _, err := s.Path(sha); !errors.Is(err, ErrNotFound) {
		t.Error("local copy still there")
	}
}

func TestDeleteRemoteFailureStillRemovesLocal(t *testing.T) {
	s, m := newRemote(t)
	sha := put(t, s, "bye")
	m.Fail = errors.New("down")
	if err := s.Delete(sha); err == nil {
		t.Error("want the remote error reported")
	}
	if _, err := s.Path(sha); !errors.Is(err, ErrNotFound) {
		t.Error("local copy should be gone anyway")
	}
}

func mustPath(t *testing.T, s *Store, sha string) string {
	t.Helper()
	p, err := s.Path(sha)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
