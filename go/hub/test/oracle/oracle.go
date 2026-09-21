// Package oracle compares the Go server with the Rust one (plan §3,
// principle 4: the Rust binary is the referee until cutover). The same
// scenario runs against both, each on an empty database; statuses, the
// headers that are contract and JSON bodies must be equal once the values
// that differ by nature (ids, times, secrets) are replaced by placeholders.
package oracle

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Step is one request of a scenario.
type Step struct {
	Name   string
	Method string
	Path   string
	Body   any
	// Headers are sent as given; the console's CSRF header is always sent.
	Headers map[string]string
}

// Result is what one server answered to one step, normalized.
type Result struct {
	Status  int
	Headers map[string]string
	Body    any
}

// ContractHeaders are compared; every other header may differ.
var ContractHeaders = []string{"Content-Type", "Cache-Control", "Content-Security-Policy", "Retry-After"}

var (
	uuidRe  = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	timeRe  = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})`)
	tokenRe = regexp.MustCompile(`kbn_pat_[0-9a-f]{32}_[A-Za-z0-9_-]+`)
)

// Normalize replaces ids, times and tokens in s by placeholders.
func Normalize(s string) string {
	s = tokenRe.ReplaceAllString(s, "<token>")
	s = uuidRe.ReplaceAllString(s, "<uuid>")
	return timeRe.ReplaceAllString(s, "<time>")
}

func normalizeJSON(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if strings.HasSuffix(k, "_at") || strings.HasSuffix(k, "At") || k == "seq" {
				if val != nil {
					val = "<time-or-seq>"
				}
			}
			out[k] = normalizeJSON(val)
		}
		return out
	case []any:
		for i := range x {
			x[i] = normalizeJSON(x[i])
		}
		return x
	case string:
		return Normalize(x)
	default:
		return v
	}
}

// Client runs a scenario against one server, keeping its cookies.
type Client struct {
	base string
	http *http.Client
}

// NewClient talks to the server at base (e.g. http://127.0.0.1:3000).
func NewClient(base string) (*Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("cookie jar: %w", err)
	}
	return &Client{base: strings.TrimSuffix(base, "/"), http: &http.Client{Jar: jar, Timeout: 30 * time.Second}}, nil
}

// Do runs one step.
func (c *Client) Do(s Step) (Result, error) {
	var body io.Reader
	if s.Body != nil {
		data, err := json.Marshal(s.Body)
		if err != nil {
			return Result{}, fmt.Errorf("%s: %w", s.Name, err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequest(s.Method, c.base+s.Path, body)
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", s.Name, err)
	}
	req.Header.Set("X-Kuben-Client", "oracle")
	if s.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range s.Headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", s.Name, err)
	}
	if resp == nil {
		return Result{}, fmt.Errorf("%s: no response", s.Name)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", s.Name, err)
	}
	r := Result{Status: resp.StatusCode, Headers: map[string]string{}}
	for _, h := range ContractHeaders {
		if v := resp.Header.Get(h); v != "" {
			r.Headers[h] = v
		}
	}
	var decoded any
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		// The console's page names hashed assets of its own build: only
		// the headers are the contract.
		r.Body = "<html>"
	} else if len(raw) > 0 && json.Unmarshal(raw, &decoded) == nil {
		r.Body = normalizeJSON(decoded)
	} else if len(raw) > 0 {
		r.Body = Normalize(string(raw))
	}
	return r, nil
}

// Diff describes how two results differ ("" when they do not).
func Diff(want, got Result) string {
	var out []string
	if want.Status != got.Status {
		out = append(out, fmt.Sprintf("status %d ≠ %d", want.Status, got.Status))
	}
	keys := map[string]bool{}
	for k := range want.Headers {
		keys[k] = true
	}
	for k := range got.Headers {
		keys[k] = true
	}
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if want.Headers[k] != got.Headers[k] {
			out = append(out, fmt.Sprintf("header %s %q ≠ %q", k, want.Headers[k], got.Headers[k]))
		}
	}
	w, err1 := json.Marshal(want.Body)
	g, err2 := json.Marshal(got.Body)
	if err1 != nil || err2 != nil || !bytes.Equal(w, g) {
		out = append(out, fmt.Sprintf("body\n  rust: %s\n  go:   %s", w, g))
	}
	return strings.Join(out, "; ")
}
