package problem_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Teamtem-dev/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/internal/httpapi/problem"
)

func TestProblemsMatchTheRustBodies(t *testing.T) {
	cases := []struct {
		err    error
		status int
		body   string
	}{
		{
			kerrors.New(kerrors.NotFound, "no route for /api/v1/x"), 404,
			`{"code":"not_found","title":"Not Found","status":404,"detail":"not found: no route for /api/v1/x"}`,
		},
		{kerrors.ErrForbidden, 403, `{"code":"forbidden","title":"Forbidden","status":403,"detail":"forbidden"}`},
		{
			kerrors.New(kerrors.Validation, "name <x> & y"), 422,
			`{"code":"validation_failed","title":"Unprocessable Entity","status":422,"detail":"validation failed: name <x> & y"}`,
		},
		{
			fmt.Errorf("load: %w", errors.New("secret db text")), 500,
			`{"code":"internal","title":"Internal Server Error","status":500}`,
		},
		{
			kerrors.New(kerrors.InsecureTransport, "use https"), 403,
			`{"code":"insecure_transport","title":"Forbidden","status":403,"detail":"use https"}`,
		},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		problem.Write(rec, nil, c.err)
		if rec.Code != c.status || rec.Body.String() != c.body || rec.Header().Get("Content-Type") != "application/problem+json" {
			t.Errorf("%v: got %d %s", c.err, rec.Code, rec.Body)
		}
	}
	rec := httptest.NewRecorder()
	problem.Write(rec, nil, kerrors.TooMany(30))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "30" {
		t.Fatalf("got %d %q", rec.Code, rec.Header().Get("Retry-After"))
	}
}
