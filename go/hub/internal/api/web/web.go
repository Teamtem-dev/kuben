// Package web serves the embedded console (crates/kuben-api/src/web.rs):
// every path that is not an API route gets the single-page app with a strict
// Content-Security-Policy; hashed assets are cached forever; `/api/` paths
// never fall through to the app.
//
// The console build is copied into dist/ before `go build` (turbo task
// hub#build); without it the page says the UI is not embedded, as a Rust
// build without the embed-ui feature did.
package web

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/problem"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
)

// CSP allows nothing inline and nothing foreign: the console has no inline
// scripts or styles (React sets styles through the CSSOM, which CSP allows).
const CSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"font-src 'self'; connect-src 'self'; frame-ancestors 'none'; object-src 'none'; base-uri 'self'; form-action 'self'"

//go:embed all:dist
var embedded embed.FS

const notEmbedded = "<!doctype html><title>Kuben</title><p>Kuben API is running. The web UI is not embedded in " +
	"this build (build the console first) — during development use the Vite dev server.</p>"

// Handler serves the console from dist.
type Handler struct{ files fs.FS }

// New serves the console embedded in the binary.
func New() *Handler {
	sub, err := fs.Sub(embedded, "dist")
	if err != nil {
		sub = embedded // unreachable: dist is embedded
	}
	return &Handler{files: sub}
}

// NewFS serves the console from files (tests, development).
func NewFS(files fs.FS) *Handler { return &Handler{files: files} }

// SecurityHeaders are the headers every console response carries.
func SecurityHeaders(h http.Header) {
	h.Set("Content-Security-Policy", CSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	h.Set("X-Frame-Options", "DENY")
}

// ServeHTTP answers with a file of the console or its index.html.
func (s *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if strings.HasPrefix(p, "/api/") {
		problem.Write(w, nil, kerr.New(kerr.NotFound, "no route for %s", p))
		return
	}
	rel := strings.TrimPrefix(path.Clean("/"+p), "/")
	asset := false
	if rel == "" || strings.HasSuffix(rel, ".map") || !s.exists(rel) {
		rel = "index.html"
	} else {
		asset = strings.HasPrefix(rel, "assets/")
	}
	body, err := fs.ReadFile(s.files, rel)
	h := w.Header()
	SecurityHeaders(h)
	if err != nil {
		h.Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(notEmbedded)) //nolint:errcheck // the client is gone
		return
	}
	contentType := mime.TypeByExtension(path.Ext(rel))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	h.Set("Content-Type", contentType)
	if asset {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	_, _ = w.Write(body) //nolint:errcheck,gosec // the client is gone; body is an embedded console file
}

func (s *Handler) exists(rel string) bool {
	info, err := fs.Stat(s.files, rel)
	if err != nil {
		return false
	}
	return !info.IsDir()
}
