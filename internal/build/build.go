// Package build is Git → isolated build (M3, ADR-028). It replaces the Rust
// module crates/kuben-platform/src/build: mod.rs (this file), job.rs
// (job.go, scripts.go), steps.rs, observe.rs, evidence.rs, rescan.rs and
// worker.rs (worker.go, attempt.go, cluster.go); scenarios.rs is
// scenarios_test.go.
//
// The worker claims `source.sync` and `build` operations. A sync reads the
// branch head through a [SourceProvider]; a build runs as one rootless
// BuildKit Job with a repository-scoped, short-lived fetch token and no
// service-account token, and succeeds only when an [OutputVerifier] finds
// the reported digest in the registry. The interfaces are the failure
// boundary to the outside world; their HTTP implementations live with the
// API's transport (internal/integrations/github, internal/integrations/oci). After the build,
// the pod writes the image's SBOM and scans it (evidence.go, M4.6); the
// scan is recorded before the attempt completes, so the scan gate judges
// it.
package build

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/Teamtem-dev/kuben/internal/core/artifact"
	"github.com/Teamtem-dev/kuben/internal/core/source"
)

// Head is a branch (or pull request) head read from the provider.
type Head struct {
	// Commit is the commit the ref points to.
	Commit source.CommitSha
	// RepositoryID is the provider's immutable repository id.
	RepositoryID uint64
}

// FetchToken is a read-only credential for exactly one repository. Printing
// or serializing it never shows the token.
type FetchToken struct {
	// Token is the credential itself; never printed.
	Token string
	// ExpiresAt is when the provider stops accepting it, unix milliseconds.
	ExpiresAt int64
}

// String leaves the token out, as the Rust Debug implementation did.
func (t FetchToken) String() string {
	return fmt.Sprintf("FetchToken{Token: <redacted>, ExpiresAt: %d}", t.ExpiresAt)
}

// GoString is String: %#v must not print the token either.
func (t FetchToken) GoString() string { return t.String() }

// Format prints String for every verb: without it, %d and %x would print
// the fields, the token included.
func (t FetchToken) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, t.String()) //nolint:errcheck // fmt.Formatter cannot report a write error
}

// MarshalJSON writes the expiry only: a token is never serialized, and if
// it were, the credential must not go with it.
func (t FetchToken) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]int64{"expires_at": t.ExpiresAt}) //nolint:wrapcheck // integers always encode
}

// ProviderError is why the provider did not answer. Every error a
// [SourceProvider] returns is one of its variants (as a value, so
// errors.As with a variant or with ProviderError finds it).
//
//sumtype:decl
type ProviderError interface {
	error
	providerError()
}

// NotFound says that the installation, repository or branch does not exist
// (any more).
type NotFound struct {
	// What names the missing thing, e.g. `acme/shop@main`.
	What string
}

// Refused says that the provider refused the App's credentials or that the
// installation is suspended.
type Refused struct {
	// Reason is what the provider answered.
	Reason string
}

// Unavailable is a transient failure; try again later.
type Unavailable struct {
	// Reason is what went wrong (an HTTP status, a network error, …).
	Reason string
}

func (e NotFound) Error() string    { return "not found: " + e.What }
func (e Refused) Error() string     { return "refused: " + e.Reason }
func (e Unavailable) Error() string { return "unavailable: " + e.Reason }

func (NotFound) providerError()    {}
func (Refused) providerError()     {}
func (Unavailable) providerError() {}

// SourceProvider is a Git hosting provider, as far as builds need it. Every
// error it returns is a [ProviderError].
type SourceProvider interface {
	// Head is the current head of branch.
	Head(ctx context.Context, installation uint64, repository source.RepoName, branch source.BranchName) (Head, error)
	// PullHead is the current head of pull request number
	// (`refs/pull/<n>/head`), which the base repository serves even for a
	// fork (M5.1).
	PullHead(ctx context.Context, installation uint64, repository source.RepoName, number uint64) (Head, error)
	// FetchToken is a token that can only read repository's contents.
	FetchToken(ctx context.Context, installation uint64, repository source.RepoName) (FetchToken, error)
	// Revoke revokes a token handed out by FetchToken.
	Revoke(ctx context.Context, token FetchToken) error
	// CloneURL is the HTTPS URL build pods clone repository from.
	CloneURL(repository source.RepoName) string
}

// VerifyError is why an output was not accepted. Every error an
// [OutputVerifier] returns is one of its variants.
//
//sumtype:decl
type VerifyError interface {
	error
	verifyError()
}

// ManifestMissing says that the registry answered and does not hold that
// manifest.
type ManifestMissing struct {
	// What is the missing manifest (`repository@digest`), or why the
	// output names none.
	What string
}

// RegistryUnavailable says that the registry could not be asked; try again
// later.
type RegistryUnavailable struct {
	// Reason is what went wrong.
	Reason string
}

func (e ManifestMissing) Error() string { return "the registry has no manifest " + e.What }

func (e RegistryUnavailable) Error() string { return "the registry is unavailable: " + e.Reason }

func (ManifestMissing) verifyError()     {}
func (RegistryUnavailable) verifyError() {}

// OutputVerifier checks a build's reported output against the registry
// (ADR-028).
type OutputVerifier interface {
	// Verify is nil when repository@digest exists, else a [VerifyError].
	Verify(ctx context.Context, repository string, digest artifact.Digest) error
}
