package main

import (
	"embed"
	"net/http"
)

// The web UI is three static files built into the binary. It talks only to
// /api/v1, the same API the iOS app uses.
//
//go:embed index.html app.js app.css
var webFiles embed.FS

func webHandler() http.Handler {
	files := http.FileServerFS(webFiles)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/", "/app.js", "/app.css":
			w.Header().Set("Cache-Control", "no-cache")
			files.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}
