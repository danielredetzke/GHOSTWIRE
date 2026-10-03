package main

import (
	"embed"
	"net/http"
)

// The web UI and its icons are built into the binary. The UI talks only to
// /api/v1, the same API the iOS app uses.
//
//go:embed index.html app.js app.css favicon.svg apple-touch-icon.png
var webFiles embed.FS

func webHandler() http.Handler {
	files := http.FileServerFS(webFiles)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/", "/app.js", "/app.css", "/favicon.svg", "/apple-touch-icon.png":
			w.Header().Set("Cache-Control", "no-cache")
			files.ServeHTTP(w, r)
		case "/favicon.ico":
			// Browsers ask for this by default; point them to the SVG.
			http.Redirect(w, r, "/favicon.svg", http.StatusMovedPermanently)
		default:
			http.NotFound(w, r)
		}
	})
}
