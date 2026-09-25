// Package apidocs serves the API reference at /api/docs: the page
// utoipa-scalar rendered in Rust (crates/kuben-api/src/lib.rs), over the
// frozen contract instead of a spec built at run time.
package apidocs

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// contract is a byte-identical copy of packages/api-client/openapi.json (a
// test keeps them equal while that file exists).
//
//go:embed openapi.json
var contract []byte

// Path is where the reference is served.
const Path = "/api/docs"

// page is utoipa-scalar 0.3's default template; `$spec` is replaced by the
// spec as compact JSON, as Scalar::to_html does.
const page = `<!doctype html>
<html>
<head>
    <title>Scalar</title>
    <meta charset="utf-8"/>
    <meta
            name="viewport"
            content="width=device-width, initial-scale=1"/>
</head>
<body>

<script
        id="api-reference"
        type="application/json">
    $spec
</script>
<script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference"></script>
</body>
</html>
`

// frozenVersion is the version the frozen contract names; the served spec
// names the running build instead, as Rust's spec named its crate version.
const frozenVersion = `"version":"1.2.0"`

// HTML is the reference page for a build of version (the frozen version for
// a development build, "dev").
func HTML(version string) ([]byte, error) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, contract); err != nil {
		return nil, fmt.Errorf("the embedded contract: %w", err)
	}
	spec := compact.String()
	if version != "dev" {
		if strings.Count(spec, frozenVersion) != 1 {
			return nil, fmt.Errorf("the embedded contract does not name version 1.2.0 once")
		}
		name, err := json.Marshal(version)
		if err != nil {
			return nil, fmt.Errorf("the version: %w", err)
		}
		spec = strings.Replace(spec, frozenVersion, `"version":`+string(name), 1)
	}
	return []byte(strings.Replace(page, "$spec", spec, 1)), nil
}

// Handler answers GET and HEAD with the page, anything else with 405 (an
// axum method router's answer).
func Handler(html []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET,HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(html) //nolint:errcheck // the client is gone
	})
}
