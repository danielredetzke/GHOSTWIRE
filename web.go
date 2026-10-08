package main

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"log/slog"
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

// The pages load app.js, setup.js and app.css with ?v=<hash of the file>, so
// a new binary makes browsers fetch the new files, and a fingerprinted file
// can be cached for good.
var (
	assetHash = map[string]string{}
	indexPage []byte
)

func init() {
	for _, name := range []string{"app.js", "setup.js", "app.css"} {
		b, err := webFiles.ReadFile(name)
		if err != nil {
			panic(err)
		}
		sum := sha256.Sum256(b)
		assetHash[name] = hex.EncodeToString(sum[:5])
	}
	b, err := webFiles.ReadFile("index.html")
	if err != nil {
		panic(err)
	}
	indexPage = fingerprint(b, "/")
}

// fingerprint adds ?v=<hash> to the page's references to base + file.
func fingerprint(page []byte, base string) []byte {
	s := string(page)
	for name, h := range assetHash {
		s = strings.ReplaceAll(s, `"`+base+name+`"`, `"`+base+name+"?v="+h+`"`)
	}
	return []byte(s)
}

func (a *App) webHandler() http.Handler {
	files := http.FileServerFS(webFiles)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if d := a.store.Get().Decoy; d.Enabled {
			serveDecoy(w, r, d.Page)
			return
		}
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			_, _ = w.Write(indexPage)
		case "/app.js", "/setup.js", "/app.css":
			if v := r.URL.Query().Get("v"); v != "" && v == assetHash[r.URL.Path[1:]] {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			files.ServeHTTP(w, r)
		case "/favicon.svg", "/apple-touch-icon.png":
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
		slog.Error("setup page", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	base := "/setup/" + url.PathEscape(token) + "/"
	page := strings.NewReplacer(`href="/`, `href="`+base, `src="/`, `src="`+base).Replace(string(b))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(fingerprint([]byte(page), base))
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
