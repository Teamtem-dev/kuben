package httpx_test

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/httpx"
)

func TestAcceptsGzipFollowsTheQValues(t *testing.T) {
	cases := []struct {
		header string
		want   bool
	}{
		{"", false},
		{"gzip", true},
		{"gzip, deflate, br", true},
		{"br;q=1.0, gzip;q=0.8", true},
		{"GZIP;Q=0.5", true},
		{"gzip;q=0", false},
		{"deflate", false},
		{"*", true},
		{"*;q=0", false},
		{"gzip;q=0.5, identity;q=0.8", false},
		{"gzip;q=0.8, identity;q=0.5", true},
		{"gzip;q=nope", false},
		{"gzip;q=2", false},
	}
	for _, c := range cases {
		if got := httpx.AcceptsGzip([]string{c.header}); got != c.want {
			t.Errorf("%q: %v, want %v", c.header, got, c.want)
		}
	}
}

// answer is what a client got: status, header and decoded body.
type answer struct {
	StatusCode int
	Header     http.Header
}

// serve runs handler behind the compressor for a client that takes gzip.
func serve(t *testing.T, method string, handler http.HandlerFunc) (answer, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, "/x", nil)
	req.Header.Set("Accept-Encoding", "gzip, br")
	httpx.NewCompressor().Wrap(handler).ServeHTTP(rec, req)
	resp := rec.Result()
	defer resp.Body.Close()
	var body io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		body = gz
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	return answer{StatusCode: resp.StatusCode, Header: resp.Header}, string(raw)
}

func TestTheDefaultPredicateDecides(t *testing.T) {
	long := strings.Repeat("{\"a\":1}", 20)
	cases := []struct {
		name, contentType, body string
		status                  int
		gzipped                 bool
	}{
		{"json", "application/json", long, 200, true},
		{"an error", "application/problem+json", long, 404, true},
		{"under 32 bytes", "application/json", `{"ok":true}`, 200, false},
		{"exactly 32 bytes", "text/plain", strings.Repeat("x", 32), 200, true},
		{"an image", "image/png", long, 200, false},
		{"an svg", "image/svg+xml", long, 200, true},
		{"an event stream", "text/event-stream", long, 200, false},
		{"grpc", "application/grpc", long, 200, false},
		{"grpc-web", "application/grpc-web", long, 200, true},
		{"no content", "", "", 204, false},
	}
	for _, c := range cases {
		resp, body := serve(t, http.MethodGet, func(w http.ResponseWriter, _ *http.Request) {
			if c.contentType != "" {
				w.Header().Set("Content-Type", c.contentType)
			}
			w.Header().Set("Accept-Ranges", "bytes")
			w.WriteHeader(c.status)
			for i := 0; i < len(c.body); i += 7 { // in pieces, as handlers write
				_, _ = io.WriteString(w, c.body[i:min(i+7, len(c.body))])
			}
		})
		gzipped := resp.Header.Get("Content-Encoding") == "gzip"
		if resp.StatusCode != c.status || gzipped != c.gzipped || body != c.body {
			t.Errorf("%s: %d gzip=%v %q", c.name, resp.StatusCode, gzipped, body)
		}
		if gzipped && (resp.Header.Get("Vary") != "Accept-Encoding" || resp.Header.Get("Accept-Ranges") != "") {
			t.Errorf("%s: headers %v", c.name, resp.Header)
		}
	}
}

func TestAnEncodedOrPartialBodyIsLeftAlone(t *testing.T) {
	for _, header := range []string{"Content-Encoding", "Content-Range"} {
		resp, _ := serve(t, http.MethodGet, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set(header, "br")
			_, _ = io.WriteString(w, strings.Repeat("x", 100))
		})
		if resp.Header.Get("Content-Encoding") == "gzip" {
			t.Errorf("%s: compressed again", header)
		}
	}
}

func TestAFlushSendsWhatIsHeld(t *testing.T) {
	resp, body := serve(t, http.MethodGet, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "short")
		http.NewResponseController(w).Flush() //nolint:errcheck // a recorder flushes
		_, _ = io.WriteString(w, strings.Repeat(" more", 20))
	})
	if resp.Header.Get("Content-Encoding") == "gzip" || body != "short"+strings.Repeat(" more", 20) {
		t.Fatalf("%v %q", resp.Header, body)
	}
}

func TestAClientWithoutGzipGetsThePlainBody(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	httpx.NewCompressor().Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", 100))
	})).ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "" || rec.Header().Get("Vary") != "" || rec.Body.Len() != 100 {
		t.Fatalf("%v %d", rec.Header(), rec.Body.Len())
	}
}

func TestHeadAndEmptyBodiesKeepTheirStatus(t *testing.T) {
	resp, body := serve(t, http.MethodHead, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
	})
	if resp.StatusCode != http.StatusOK || body != "" || resp.Header.Get("Content-Encoding") != "" {
		t.Fatalf("%d %v %q", resp.StatusCode, resp.Header, body)
	}
}

func TestACompressedBodyKeepsItsSniffedType(t *testing.T) {
	resp, _ := serve(t, http.MethodGet, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "<!doctype html><html>"+strings.Repeat(" ", 40))
	})
	if resp.Header.Get("Content-Encoding") != "gzip" || resp.Header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("%v", resp.Header)
	}
}
