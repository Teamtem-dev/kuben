// Package oci resolves image references to digests (plan §8.1, I03) and
// verifies build outputs in their registry ([Verifier]). It replaces the
// Rust module crates/kuben-api/src/oci.rs.
//
// A release pins every image by digest. When the API is given a tag, it asks
// the image's registry once which digest the tag names now, and records both:
// the digest is what gets deployed, the tag is provenance (the maintainer's
// decision on 2026-09-15, option A). A reference that is already
// `repository@sha256:…` needs no registry at all.
//
// Registries are reached over HTTPS only, through the proxy the environment
// names (HTTPS_PROXY, NO_PROXY).
package oci

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/Teamtem-dev/kuben/internal/core/artifact"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
)

// DockerHub is the registry name of images given without one.
const DockerHub = "docker.io"

// MaxTags is the most tags read from a repository.
const MaxTags = 10_000

// Reference is what an image reference names: a [Tag] or a [Digest].
//
//sumtype:decl
type Reference interface {
	reference()
}

// Tag is a reference by tag, e.g. `1.27`.
type Tag struct {
	// Name is the tag, at most 128 bytes.
	Name string
}

// Digest is a reference pinned by digest.
type Digest struct {
	// Digest is the manifest digest.
	Digest artifact.Digest
}

func (Tag) reference()    {}
func (Digest) reference() {}

// ImageRef is a parsed image reference, normalized like Docker does it.
type ImageRef struct {
	// Registry is `docker.io`, `ghcr.io`, `registry.example.com:5000`, …
	Registry string
	// Path is the path in the registry: `library/nginx`, `acme/web`, …
	Path string
	// Reference is the tag or digest; never nil for a parsed reference.
	Reference Reference
}

// Repository is `registry/path`, the repository a digest is deployed from.
func (r ImageRef) Repository() string { return r.Registry + "/" + r.Path }

// Resolved is an image pinned by digest, and the reference it was given as.
type Resolved struct {
	// Repository is `registry/path`.
	Repository string
	// Digest is the digest the reference named when it was resolved.
	Digest artifact.Digest
	// Given is the reference as the caller gave it, e.g. `nginx:1.27`.
	Given string
}

// Pinned is `repository@digest`.
func (r Resolved) Pinned() string { return r.Repository + "@" + r.Digest.String() }

// Login is a set of credentials for pulling from a private registry: the
// values of a `registry` secret (Rust RegistryLogin). Printing it never shows
// the password.
type Login struct {
	// Username is the registry user.
	Username string
	// Password is the password or access token; never printed.
	Password string
}

// Basic is the `Authorization: Basic …` value of the login.
func (l Login) Basic() string { return "Basic " + l.encoded() }

// encoded is base64(`username:password`), the credentials of Basic.
func (l Login) encoded() string {
	return base64.StdEncoding.EncodeToString([]byte(l.Username + ":" + l.Password))
}

// String names the user and leaves the password out.
func (l Login) String() string { return fmt.Sprintf("Login{Username: %q, ..}", l.Username) }

// GoString is String: %#v must not print the password either.
func (l Login) GoString() string { return l.String() }

// Format prints String for every verb: without it, %d and %x would print
// the fields, the password included.
func (l Login) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, l.String()) //nolint:errcheck // fmt.Formatter cannot report a write error
}

// MarshalJSON writes the username only: a login is never serialized, and
// if it were, the password must not go with it.
func (l Login) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{"username": l.Username}) //nolint:wrapcheck // strings always encode
}

// Resolver resolves image references to digests.
type Resolver interface {
	// ResolveAs resolves image, pulling as login when given (a private registry).
	ResolveAs(ctx context.Context, image string, login opt.Val[Login]) (Resolved, error)
	// ListTags is the tags of repository (M5.4), at most MaxTags.
	ListTags(ctx context.Context, repository string, login opt.Val[Login]) ([]string, error)
}

// ResolveError is why a reference was not resolved. Every error the
// resolvers of this package return is one of its variants (as a value, so
// errors.As with a variant or with ResolveError finds it).
//
//sumtype:decl
type ResolveError interface {
	error
	resolveError()
}

// Invalid says that the text is not an image reference.
type Invalid struct {
	// Image is the text as given.
	Image string
}

// NotFound says that the registry has no such image or repository.
type NotFound struct {
	// Image is the reference as given.
	Image string
}

// Unauthorized says that the registry refuses the pull.
type Unauthorized struct {
	// Image is the reference as given.
	Image string
}

// Unreachable says that the registry could not be asked; the API answers
// 503 for it (every other ResolveError is the caller's to fix: 422).
type Unreachable struct {
	// Image is the reference as given.
	Image string
	// Reason is what went wrong (an HTTP status, a network error, …).
	Reason string
}

// RateLimited says that the registry asked to slow down.
type RateLimited struct {
	// Image is the reference as given.
	Image string
	// RetryAfter is how many seconds to wait before trying again (at most 3600;
	// 60 when the registry did not say).
	RetryAfter uint64
}

func (e Invalid) Error() string { return fmt.Sprintf("`%s` is not an image reference", e.Image) }

func (e NotFound) Error() string { return fmt.Sprintf("the registry has no image `%s`", e.Image) }

func (e Unauthorized) Error() string {
	return fmt.Sprintf("the registry of `%s` refuses the pull: add a login for it to the environment's registries, or give repository@sha256:… instead", e.Image)
}

func (e Unreachable) Error() string {
	return fmt.Sprintf("cannot reach the registry of `%s`: %s", e.Image, e.Reason)
}

func (e RateLimited) Error() string {
	return fmt.Sprintf("the registry of `%s` limits requests; retry in %ds", e.Image, e.RetryAfter)
}

func (Invalid) resolveError()      {}
func (NotFound) resolveError()     {}
func (Unauthorized) resolveError() {}
func (Unreachable) resolveError()  {}
func (RateLimited) resolveError()  {}

// IsUnreachable reports whether err is (or wraps) an [Unreachable]: the
// registry could not be asked, which the API answers with 503; every other
// failure of a resolution is the caller's to fix (422).
func IsUnreachable(err error) bool {
	var u Unreachable
	return errors.As(err, &u)
}

// IsRateLimited reports whether err is a registry asking to slow down.
func IsRateLimited(err error) bool {
	var r RateLimited
	return errors.As(err, &r)
}
