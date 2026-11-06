package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func get(t *testing.T, h http.Handler, path string) (int, string, http.Header) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	b, _ := io.ReadAll(rec.Body)
	return rec.Code, string(b), rec.Header()
}

func TestWithUI(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<app>"), 0o644)
	os.MkdirAll(filepath.Join(dir, "assets"), 0o755)
	os.WriteFile(filepath.Join(dir, "assets", "app-abc.js"), []byte("js"), 0o644)

	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "api "+r.URL.Path) })
	h, ok := withUI(api, dir)
	if !ok {
		t.Fatal("ui not mounted")
	}
	for path, want := range map[string]string{
		"/ui/":                  "<app>",
		"/ui/vms":               "<app>", // an app page, not a file
		"/ui/assets/app-abc.js": "js",
		"/api/v1/vms":           "api /api/v1/vms",
		"/invoke/x":             "api /invoke/x",
	} {
		code, body, _ := get(t, h, path)
		if code != 200 || body != want {
			t.Errorf("%s: %d %q, want %q", path, code, body, want)
		}
	}
	if code, _, _ := get(t, h, "/ui/assets/nope.js"); code != 404 {
		t.Errorf("missing asset: %d, want 404", code)
	}
	if code, _, hdr := get(t, h, "/ui"); code != http.StatusMovedPermanently || hdr.Get("Location") != "/ui/" {
		t.Errorf("/ui: %d %s", code, hdr.Get("Location"))
	}
	if _, _, hdr := get(t, h, "/ui/assets/app-abc.js"); !strings.Contains(hdr.Get("Cache-Control"), "immutable") {
		t.Errorf("assets cache header %q", hdr.Get("Cache-Control"))
	}
	if _, _, hdr := get(t, h, "/ui/"); hdr.Get("Cache-Control") != "no-cache" {
		t.Errorf("index cache header %q", hdr.Get("Cache-Control"))
	}
}

func TestWithUINotBuilt(t *testing.T) {
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	for _, dir := range []string{"", t.TempDir(), filepath.Join(t.TempDir(), "missing")} {
		if _, ok := withUI(api, dir); ok {
			t.Errorf("dir %q: ui mounted without index.html", dir)
		}
	}
}
