// Package web serves the embedded single-page app.
package web

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

//go:embed all:dist
var dist embed.FS

// Handler serves static assets and falls back to index.html for client-side
// routes. basePath is injected as <base href> so relative asset URLs resolve
// from any deep link.
func Handler(basePath string) http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	index, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		index = []byte("<!doctype html><title>Rowsmith</title><p>The web UI was not built into this binary.</p>")
	}
	index = bytes.Replace(index, []byte(`<base href="/">`), []byte(`<base href="`+basePath+`">`), 1)
	files := http.FileServer(http.FS(sub))
	started := time.Now()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if p != "" && p != "index.html" {
			if f, err := sub.Open(p); err == nil {
				st, _ := f.Stat()
				f.Close()
				if st != nil && !st.IsDir() {
					if strings.HasPrefix(p, "assets/") {
						// Vite fingerprints asset names; cache them forever.
						w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
					} else {
						w.Header().Set("Cache-Control", "no-cache")
					}
					files.ServeHTTP(w, r)
					return
				}
			}
			if path.Ext(p) != "" && !strings.HasSuffix(p, ".html") {
				http.NotFound(w, r)
				return
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeContent(w, r, "index.html", started, bytes.NewReader(index))
	})
}
