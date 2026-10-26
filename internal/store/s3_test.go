package store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

// newTestS3 starts an in-process fake S3 server and returns a client for it. If
// BEEVM_TEST_S3_ENDPOINT is set (a real MinIO), that is used instead; see `make test-s3`.
func newTestS3(t *testing.T) *S3 {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", envOr("BEEVM_TEST_S3_KEY", "test"))
	t.Setenv("AWS_SECRET_ACCESS_KEY", envOr("BEEVM_TEST_S3_SECRET", "testtesttest"))
	endpoint := os.Getenv("BEEVM_TEST_S3_ENDPOINT")
	if endpoint == "" {
		ts := httptest.NewServer(gofakes3.New(s3mem.New()).Server())
		t.Cleanup(ts.Close)
		endpoint = ts.URL
	}
	s, err := NewS3(context.Background(), S3Config{
		Bucket: "beevm-test", Endpoint: endpoint, PathStyle: true, Prefix: "t",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureBucket(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func TestS3PutGetExistsDelete(t *testing.T) {
	s := newTestS3(t)
	ctx := context.Background()
	data := []byte("hello artifact")

	if ok, err := s.Exists(ctx, "functions/a"); err != nil || ok {
		t.Fatalf("Exists before put = %v, %v", ok, err)
	}
	if err := s.Put(ctx, "functions/a", bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Exists(ctx, "functions/a"); err != nil || !ok {
		t.Fatalf("Exists after put = %v, %v", ok, err)
	}
	rc, err := s.Get(ctx, "functions/a")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data) {
		t.Fatalf("got %q", got)
	}
	if err := s.Delete(ctx, "functions/a"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Exists(ctx, "functions/a"); ok {
		t.Fatal("still exists after delete")
	}
	if err := s.Delete(ctx, "functions/a"); err != nil {
		t.Fatalf("deleting a missing key should not fail: %v", err)
	}
}

func TestS3GetMissing(t *testing.T) {
	s := newTestS3(t)
	if _, err := s.Get(context.Background(), "functions/nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestS3EnsureBucketTwice(t *testing.T) {
	s := newTestS3(t)
	if err := s.EnsureBucket(context.Background()); err != nil {
		t.Fatalf("second EnsureBucket: %v", err)
	}
}

func TestS3NeedsBucket(t *testing.T) {
	if _, err := NewS3(context.Background(), S3Config{}); err == nil {
		t.Fatal("want error for empty bucket")
	}
}

func TestMemBasics(t *testing.T) {
	m := NewMem()
	ctx := context.Background()
	_ = m.Put(ctx, "k", bytes.NewReader([]byte("x")), 1)
	if ok, _ := m.Exists(ctx, "k"); !ok {
		t.Fatal("missing")
	}
	m.Fail = errors.New("down")
	if _, err := m.Get(ctx, "k"); err == nil {
		t.Fatal("want failure")
	}
}
