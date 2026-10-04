package main

import (
	"embed"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The web UI and its icons are built into the binary. The UI talks only to
// /api/v1, the same API the iOS app uses.
//
//go:embed index.html setup.html app.js setup.js app.css favicon.svg apple-touch-icon.png ShipporiMinchoB1-ExtraBold.woff2
var webFiles embed.FS

func (a *App) webHandler() http.Handler {
	files := http.FileServerFS(webFiles)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if d := a.store.Get().Decoy; d.Enabled {
			serveDecoy(w, r, d.Page)
			return
		}
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

// setupAllowed reports whether the setup page and its files may be served for
// this token. With the decoy on, only a live setup link gets past the decoy.
func (a *App) setupAllowed(token string) bool {
	cfg := a.store.Get()
	if !cfg.Decoy.Enabled {
		return true
	}
	p := cfg.peerByToken(token)
	return p != nil && !p.Setup.expired(time.Now())
}

// setupPage serves the page a setup link opens. The token stays in the URL;
// setup.js reads it from there and talks to /api/v1/setup. The page loads its
// files from under the link, so they work while the decoy hides the root.
func (a *App) setupPage(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if !a.setupAllowed(token) {
		serveDecoy(w, r, a.store.Get().Decoy.Page)
		return
	}
	b, err := webFiles.ReadFile("setup.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	base := "/setup/" + url.PathEscape(token) + "/"
	page := strings.NewReplacer(`href="/`, `href="`+base, `src="/`, `src="`+base).Replace(string(b))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(page))
}

func (a *App) setupAsset(w http.ResponseWriter, r *http.Request) {
	file := r.PathValue("file")
	switch file {
	case "setup.js", "app.css", "favicon.svg", "apple-touch-icon.png", "ShipporiMinchoB1-ExtraBold.woff2":
	default:
		a.webHandler().ServeHTTP(w, r)
		return
	}
	if !a.setupAllowed(r.PathValue("token")) {
		serveDecoy(w, r, a.store.Get().Decoy.Page)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFileFS(w, r, webFiles, file)
}
