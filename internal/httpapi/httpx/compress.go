package httpx

import (
	"compress/gzip"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// minCompressSize is tower-http's SizeAbove default: smaller bodies are
// sent as they are.
const minCompressSize = 32

// Compressor gzips responses as tower-http's CompressionLayer with its
// DefaultPredicate did in Rust (crates/kuben-api/src/lib.rs): unless the
// client refuses gzip, the response already has a Content-Encoding or a
// Content-Range, it is an image (SVG aside), an event stream or gRPC, or
// its body is under 32 bytes. Brotli and zstd, which Rust also offered,
// are not in Go's standard library; every browser takes gzip instead.
type Compressor struct {
	writers sync.Pool // *gzip.Writer, reset for each response
}

// NewCompressor is a compressor with its own pool of gzip writers.
func NewCompressor() *Compressor {
	return &Compressor{writers: sync.Pool{New: func() any { return gzip.NewWriter(nil) }}}
}

// Wrap compresses the responses of next.
func (c *Compressor) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !AcceptsGzip(r.Header.Values("Accept-Encoding")) {
			next.ServeHTTP(w, r)
			return
		}
		cw := &compressWriter{ResponseWriter: w, pool: &c.writers}
		defer cw.finish()
		next.ServeHTTP(cw, r)
	})
}

// AcceptsGzip reports whether Accept-Encoding lets gzip be used: its
// q-value (listed, or through `*`) is above zero and not below an
// explicitly listed identity's.
func AcceptsGzip(values []string) bool {
	gzipQ, identityQ, wildcardQ := -1, -1, -1
	for _, value := range values {
		for part := range strings.SplitSeq(value, ",") {
			coding, param, _ := strings.Cut(part, ";")
			q, ok := qValue(param)
			if !ok {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(coding)) {
			case "gzip":
				gzipQ = q
			case "identity":
				identityQ = q
			case "*":
				wildcardQ = q
			}
		}
	}
	if gzipQ < 0 {
		gzipQ = wildcardQ
	}
	return gzipQ > 0 && gzipQ >= identityQ
}

// qValue reads `q=0.5` (thousandths); no parameter is 1000.
func qValue(param string) (int, bool) {
	param = strings.TrimSpace(param)
	if param == "" {
		return 1000, true
	}
	name, value, ok := strings.Cut(param, "=")
	if !ok || !strings.EqualFold(strings.TrimSpace(name), "q") {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || f < 0 || f > 1 {
		return 0, false
	}
	return int(f * 1000), true
}

// compressible is DefaultPredicate's content-type half.
func compressible(h http.Header) bool {
	if h.Get("Content-Encoding") != "" || h.Get("Content-Range") != "" {
		return false
	}
	ct := h.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "image/") && !strings.HasPrefix(ct, "image/svg+xml"),
		strings.HasPrefix(ct, "text/event-stream"),
		strings.HasPrefix(ct, "application/grpc") && !strings.HasPrefix(ct, "application/grpc-web"):
		return false
	}
	if n, err := strconv.ParseInt(h.Get("Content-Length"), 10, 64); err == nil && n < minCompressSize {
		return false
	}
	return true
}

// compressWriter holds the header back until it knows whether the body is
// compressed: at 32 bytes of body, a flush, or the end of the response.
type compressWriter struct {
	http.ResponseWriter
	pool    *sync.Pool
	status  int    // the status the handler wrote; 0 before WriteHeader
	pending []byte // body held back while under minCompressSize
	decided bool   // the header went out
	gz      *gzip.Writer
}

// WriteHeader records the status; it is sent once the body is known.
func (w *compressWriter) WriteHeader(status int) {
	if w.status != 0 || w.decided {
		return
	}
	if status < http.StatusOK {
		w.ResponseWriter.WriteHeader(status) // informational: the real status follows
		return
	}
	w.status = status
	if !compressible(w.Header()) {
		w.start(false)
	}
}

// Write compresses or passes the body on, holding back its first bytes.
func (w *compressWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if !w.decided {
		w.pending = append(w.pending, b...)
		if len(w.pending) < minCompressSize {
			return len(b), nil
		}
		w.start(true)
		held := w.pending
		w.pending = nil
		if _, err := w.body().Write(held); err != nil {
			return 0, err //nolint:wrapcheck // a pass-through
		}
		return len(b), nil
	}
	return w.body().Write(b) //nolint:wrapcheck // a pass-through
}

// Flush sends what is held back (compressed when it is long enough to be)
// and flushes the connection.
func (w *compressWriter) Flush() {
	if !w.decided {
		if w.status == 0 {
			w.status = http.StatusOK
		}
		w.start(len(w.pending) >= minCompressSize)
		w.writePending()
	}
	if w.gz != nil {
		_ = w.gz.Flush() //nolint:errcheck // the connection's error shows on the next write
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *compressWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// start sends the header, compressed or not.
func (w *compressWriter) start(compress bool) {
	w.decided = true
	h := w.Header()
	if compress {
		if h.Get("Content-Type") == "" {
			// net/http sniffs only bodies without a Content-Encoding.
			h.Set("Content-Type", http.DetectContentType(w.pending))
		}
		h.Add("Vary", "Accept-Encoding")
		h.Set("Content-Encoding", "gzip")
		h.Del("Content-Length")
		h.Del("Accept-Ranges")
		gz, ok := w.pool.Get().(*gzip.Writer)
		if !ok {
			gz = gzip.NewWriter(nil)
		}
		gz.Reset(w.ResponseWriter)
		w.gz = gz
	}
	if w.status != 0 {
		w.ResponseWriter.WriteHeader(w.status)
	}
}

func (w *compressWriter) body() interface{ Write([]byte) (int, error) } {
	if w.gz != nil {
		return w.gz
	}
	return w.ResponseWriter
}

func (w *compressWriter) writePending() {
	if len(w.pending) > 0 {
		_, _ = w.body().Write(w.pending) //nolint:errcheck // the client is gone
		w.pending = nil
	}
}

// finish ends the response: a short body goes out as it is, a compressed
// one gets its gzip trailer.
func (w *compressWriter) finish() {
	if !w.decided {
		if w.status == 0 && len(w.pending) == 0 {
			return // nothing was written; net/http sends its own 200
		}
		if w.status == 0 {
			w.status = http.StatusOK
		}
		w.start(false)
		w.writePending()
	}
	if w.gz != nil {
		_ = w.gz.Close() //nolint:errcheck // the client is gone
		w.gz.Reset(nil)
		w.pool.Put(w.gz)
		w.gz = nil
	}
}
