// Package problem writes RFC 9457 application/problem+json responses
// (crates/kuben-api/src/error.rs). The body, the codes and the status of
// each code are part of the API contract.
package problem

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/Teamtem-dev/kuben/internal/core/kerrors"
)

// Problem is the Problem Details body. Field order and names match the Rust
// type; Detail is left out when absent.
type Problem struct {
	// Code is the stable, machine-readable code, e.g. `forbidden`.
	Code string `json:"code"`
	// Title is the HTTP reason phrase.
	Title string `json:"title"`
	// Status is the HTTP status.
	Status int `json:"status"`
	// Detail explains the error when that is safe to expose.
	Detail string `json:"detail,omitempty"`
}

// Status is the HTTP status of a code.
func Status(code kerrors.Code) int {
	switch code {
	case kerrors.NotFound:
		return http.StatusNotFound
	case kerrors.Conflict:
		return http.StatusConflict
	case kerrors.Unauthorized:
		return http.StatusUnauthorized
	case kerrors.Forbidden, kerrors.InsecureTransport:
		return http.StatusForbidden
	case kerrors.Validation:
		return http.StatusUnprocessableEntity
	case kerrors.Unavailable:
		return http.StatusServiceUnavailable
	case kerrors.RateLimited:
		return http.StatusTooManyRequests
	case kerrors.Internal:
		return http.StatusInternalServerError
	}
	return http.StatusInternalServerError
}

// From is the problem an error answers with. Internal error text never
// reaches the client; it is logged instead.
func From(err error) (Problem, uint64) {
	var e *kerrors.Error
	if !errors.As(err, &e) || e == nil {
		e = kerrors.Wrap(err, "request failed")
	}
	status := Status(e.Code)
	p := Problem{Code: string(e.Code), Title: http.StatusText(status), Status: status}
	if e.Code != kerrors.Internal {
		p.Detail = e.Error()
	}
	return p, e.RetryAfterSecs
}

// Write sends err as a problem. Server errors are logged with their cause.
func Write(w http.ResponseWriter, logger *slog.Logger, err error) {
	p, retryAfter := From(err)
	if p.Status >= 500 && logger != nil {
		logger.Error("request failed", "error", err)
	}
	if p.Code == string(kerrors.RateLimited) {
		w.Header().Set("Retry-After", strconv.FormatUint(retryAfter, 10))
	}
	WriteProblem(w, p)
}

// WriteProblem sends p as it is.
func WriteProblem(w http.ResponseWriter, p Problem) {
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(p) //nolint:errcheck // a Problem always encodes
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	_, _ = w.Write(bytes.TrimSuffix(body.Bytes(), []byte("\n"))) //nolint:errcheck // the client is gone
}
