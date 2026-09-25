package apidocs_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/apidocs"
)

func TestTheEmbeddedContractIsTheFrozenOne(t *testing.T) {
	frozen, err := os.ReadFile("../../../../../packages/api-client/openapi.json")
	if err != nil {
		t.Skipf("the contract is not in this checkout: %v", err)
	}
	embedded, err := os.ReadFile("openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(frozen, embedded) {
		t.Fatal("apidocs/openapi.json differs from packages/api-client/openapi.json: copy it again")
	}
}

func TestThePageCarriesTheSpecOfThisBuild(t *testing.T) {
	for _, c := range []struct{ version, want string }{
		{"dev", `"version":"1.2.0"`},
		{"2.0.0-alpha.1", `"version":"2.0.0-alpha.1"`},
	} {
		html, err := apidocs.HTML(c.version)
		if err != nil {
			t.Fatalf("%s: %v", c.version, err)
		}
		page := string(html)
		if !strings.Contains(page, `"title":"Kuben API"`) || !strings.Contains(page, c.want) ||
			strings.Contains(page, "$spec") || !strings.Contains(page, "@scalar/api-reference") {
			t.Fatalf("%s: %.300s", c.version, page)
		}
	}
}

// tests/http.rs openapi_docs_are_served.
func TestOpenAPIDocsAreServed(t *testing.T) {
	html, err := apidocs.HTML("dev")
	if err != nil {
		t.Fatal(err)
	}
	h := apidocs.Handler(html)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, apidocs.Path, nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("GET: %d %v", rec.Code, rec.Header())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, apidocs.Path, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", rec.Code)
	}
}
