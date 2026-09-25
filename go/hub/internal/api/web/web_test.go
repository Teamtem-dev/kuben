package web_test

import (
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/web"
)

func TestThePolicyAllowsNothingInlineOrForeign(t *testing.T) {
	if strings.Contains(web.CSP, "unsafe") {
		t.Fatal(web.CSP)
	}
	for _, d := range []string{"default-src 'self'", "script-src 'self'", "style-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(web.CSP, d) {
			t.Errorf("%s missing", d)
		}
	}
}

func TestSPARouting(t *testing.T) {
	h := web.NewFS(fstest.MapFS{
		"index.html":             {Data: []byte("<!doctype html><div id=root></div>")},
		"assets/app-1a2b.js":     {Data: []byte("console.log(1)")},
		"assets/app-1a2b.js.map": {Data: []byte("{}")},
	})
	cases := []struct {
		path, contentType, cache, body string
		status                         int
	}{
		{"/", "text/html", "no-cache", "<!doctype", 200},
		{"/projects/shop", "text/html", "no-cache", "<!doctype", 200},
		{"/assets/app-1a2b.js", "text/javascript", "public, max-age=31536000, immutable", "console", 200},
		{"/assets/app-1a2b.js.map", "text/html", "no-cache", "<!doctype", 200},
		{"/api/v1/nothing", "application/problem+json", "", `{"code":"not_found"`, 404},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", c.path, nil))
		if rec.Code != c.status || rec.Header().Get("Content-Type") != c.contentType ||
			rec.Header().Get("Cache-Control") != c.cache || !strings.HasPrefix(rec.Body.String(), c.body) {
			t.Errorf("%s: got %d %q %q %.40q", c.path, rec.Code, rec.Header().Get("Content-Type"), rec.Header().Get("Cache-Control"), rec.Body.String())
		}
		if c.status == 200 && rec.Header().Get("Content-Security-Policy") != web.CSP {
			t.Errorf("%s: no CSP", c.path)
		}
	}
}

func TestWithoutABuildTheAppSaysSo(t *testing.T) {
	rec := httptest.NewRecorder()
	web.NewFS(fstest.MapFS{}).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if !strings.Contains(rec.Body.String(), "not embedded") || rec.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("got %s", rec.Body)
	}
}

// The types mime_guess 2.0.5 gave the Rust console handler.
func TestContentTypesAreMimeGuesses(t *testing.T) {
	for file, want := range map[string]string{
		"index.html":           "text/html",
		"assets/index-abc.js":  "text/javascript",
		"assets/index-abc.css": "text/css",
		"favicon.svg":          "image/svg+xml",
		"assets/inter.woff2":   "font/woff2",
		"assets/LOGO.PNG":      "image/png",
		"assets/data.bin":      "application/octet-stream",
		"assets/no-extension":  "application/octet-stream",
		"site.webmanifest":     "application/manifest+json",
	} {
		if got := web.ContentType(file); got != want {
			t.Errorf("%s: %s, want %s", file, got, want)
		}
	}
}
