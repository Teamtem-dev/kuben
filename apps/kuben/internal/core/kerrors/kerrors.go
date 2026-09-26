// Package kerrors is the domain error every layer shares. The API maps it to
// RFC 9457 application/problem+json; the codes are part of the API contract.
package kerrors

import (
	"errors"
	"fmt"
)

// Code is the stable, machine-readable name of a kind of failure, used in
// API responses and audit logs.
type Code string

// The codes.
const (
	NotFound          Code = "not_found"
	Conflict          Code = "conflict"
	Unauthorized      Code = "unauthorized"
	Forbidden         Code = "forbidden"
	InsecureTransport Code = "insecure_transport" // Refused over this connection, not for this caller (ADR-031).
	Validation        Code = "validation_failed"
	Unavailable       Code = "unavailable"
	RateLimited       Code = "rate_limited"
	Internal          Code = "internal"
)

// Error is a domain failure.
type Error struct {
	Code   Code
	Detail string
	// RetryAfterSecs is set on RateLimited: the caller may retry after it.
	RetryAfterSecs uint64
	cause          error
}

func (e *Error) Error() string {
	switch e.Code {
	case Unauthorized, Forbidden:
		return string(e.Code)
	case RateLimited:
		return fmt.Sprintf("too many attempts; retry in %ds", e.RetryAfterSecs)
	case InsecureTransport:
		return e.Detail
	case NotFound:
		return "not found: " + e.Detail
	case Conflict:
		return "conflict: " + e.Detail
	case Validation:
		return "validation failed: " + e.Detail
	case Unavailable:
		return "unavailable: " + e.Detail
	case Internal:
		return "internal error: " + e.Detail
	}
	return string(e.Code) + ": " + e.Detail
}

// Unwrap is the error this one was made from, if any.
func (e *Error) Unwrap() error { return e.cause }

// Is matches errors of the same code, so errors.Is(err, kerrors.ErrForbidden)
// works for every forbidden error.
func (e *Error) Is(target error) bool {
	var t *Error
	return errors.As(target, &t) && t.Code == e.Code && t.Detail == "" && t.cause == nil
}

// Sentinels for errors.Is.
var (
	ErrNotFound     = &Error{Code: NotFound}
	ErrConflict     = &Error{Code: Conflict}
	ErrUnauthorized = &Error{Code: Unauthorized}
	ErrForbidden    = &Error{Code: Forbidden}
	ErrValidation   = &Error{Code: Validation}
	ErrUnavailable  = &Error{Code: Unavailable}
	ErrRateLimited  = &Error{Code: RateLimited}
	ErrInternal     = &Error{Code: Internal}
)

// New is an error of code with a formatted detail.
func New(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// TooMany is a RateLimited error.
func TooMany(retryAfterSecs uint64) *Error {
	return &Error{Code: RateLimited, RetryAfterSecs: retryAfterSecs}
}

// Wrap keeps err as the cause of an Internal error. The detail is what the
// log gets; the API never shows it.
func Wrap(err error, format string, args ...any) *Error {
	detail := fmt.Sprintf(format, args...)
	if err == nil {
		return &Error{Code: Internal, Detail: detail}
	}
	return &Error{Code: Internal, Detail: detail + ": " + err.Error(), cause: err}
}

// CodeOf is the code of the first *Error in err's chain, or Internal.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return Internal
}
