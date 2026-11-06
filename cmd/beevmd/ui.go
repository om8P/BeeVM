package main

import (
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// withUI serves the optional web dashboard (M11) at /ui/ next to the API, if dir holds a built
// dashboard (an index.html). The dashboard is plain files that talk to the public API only:
// nothing here knows what is inside them, so deleting web/ never breaks the build. Returns the
// API handler unchanged and false when there is nothing to serve.
func withUI(apiHandler http.Handler, dir string) (http.Handler, bool) {
	if dir == "" {
		return apiHandler, false
	}
	if st, err := os.Stat(filepath.Join(dir, "index.html")); err != nil || st.IsDir() {
		return apiHandler, false
	}
	files := http.FileServer(http.Dir(dir))
	mux := http.NewServeMux()
	mux.Handle("/", apiHandler)
	mux.Handle("GET /ui", http.RedirectHandler("/ui/", http.StatusMovedPermanently))
	mux.Handle("GET /ui/", http.StripPrefix("/ui", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A path that is not a file (a page of the app, typed into the address bar) gets the
		// app itself; the app decides what to show.
		// A missing asset stays a 404 (HTML in place of a script only confuses the browser).
		name := path.Clean("/" + r.URL.Path)
		st, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil && strings.HasPrefix(name, "/assets/") {
			http.NotFound(w, r)
			return
		}
		if name == "/" || err != nil || st.IsDir() {
			w.Header().Set("Cache-Control", "no-cache")
			http.ServeFile(w, r, filepath.Join(dir, "index.html"))
			return
		}
		if strings.HasPrefix(name, "/assets/") {
			// Vite puts a content hash in these names, so they never change.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		files.ServeHTTP(w, r)
	})))
	return mux, true
}
