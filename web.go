package main

import (
	"embed"
	"net/http"
)

// The web UI and its icons are built into the binary. The UI talks only to
// /api/v1, the same API the iOS app uses.
//
//go:embed index.html setup.html app.js setup.js app.css favicon.svg apple-touch-icon.png ShipporiMinchoB1-ExtraBold.woff2
var webFiles embed.FS

func webHandler() http.Handler {
	files := http.FileServerFS(webFiles)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/", "/app.js", "/setup.js", "/app.css", "/favicon.svg", "/apple-touch-icon.png":
			w.Header().Set("Cache-Control", "no-cache")
			files.ServeHTTP(w, r)
		case "/ShipporiMinchoB1-ExtraBold.woff2":
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			files.ServeHTTP(w, r)
		case "/favicon.ico":
			// Browsers ask for this by default; point them to the SVG.
			http.Redirect(w, r, "/favicon.svg", http.StatusMovedPermanently)
		default:
			http.NotFound(w, r)
		}
	})
}

// setupPage serves the page a setup link opens. The token stays in the URL;
// setup.js reads it from there and talks to /api/v1/setup.
func setupPage(w http.ResponseWriter, r *http.Request) {
	b, err := webFiles.ReadFile("setup.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}
