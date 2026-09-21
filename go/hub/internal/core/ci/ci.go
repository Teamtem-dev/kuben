// Package ci is trust for external CI (M4.2; plan §13, S04): which GitHub
// Actions workflows may exchange their OIDC token for a short-lived Kuben
// token. It replaces the Rust module kuben-core/src/ci.rs.
//
// A trust policy names the repository and its owner by their immutable
// numeric ids (a renamed or re-created repository is someone else), the
// refs, environments and events it accepts, the role the token is capped at
// and how long it lives. Everything is denied unless a policy allows it;
// pull-request refs never carry deploy authority, so a fork cannot use a
// policy by opening a pull request.
package ci

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/perm"
)

const (
	// GithubActionsIssuer is the issuer of GitHub Actions OIDC tokens.
	GithubActionsIssuer = "https://token.actions.githubusercontent.com"
	// DefaultCITokenTTLSecs is the default life of an exchanged token.
	DefaultCITokenTTLSecs uint32 = 15 * 60
	// MinCITokenTTLSecs is the shortest life a policy may give a token.
	MinCITokenTTLSecs uint32 = 60
	// MaxCITokenTTLSecs is the longest life a policy may give a token.
	MaxCITokenTTLSecs uint32 = 60 * 60
	// ClockLeewaySecs is the clock skew tolerated on `exp`, `nbf` and `iat`.
	ClockLeewaySecs int64 = 60
	// MaxOIDCLifetimeSecs: a provider token living longer than this is refused.
	MaxOIDCLifetimeSecs int64 = 60 * 60

	maxPatterns = 20
)

// DefaultEvents are the events accepted when a policy names none.
func DefaultEvents() []string { return []string{"push", "workflow_dispatch", "release"} }

// unsafeEvents are events whose workflows may run code a pull request
// controls.
func unsafeEvents() []string {
	return []string{"pull_request", "pull_request_target", "workflow_run"}
}

func knownEvents() []string {
	return []string{"push", "workflow_dispatch", "release", "schedule", "deployment", "merge_group"}
}

// GithubClaims are the claims of a GitHub Actions OIDC token Kuben reads.
// They are decoded from JSON only: `aud` may be one string or a list, and
// the numeric ids may be strings (GitHub sends them so).
type GithubClaims struct {
	Iss               string
	Aud               []string
	Sub               string
	Jti               string
	Iat               int64
	Nbf               opt.Val[int64]
	Exp               int64
	Repository        string
	RepositoryID      uint64
	RepositoryOwner   string
	RepositoryOwnerID uint64
	// GitRef is the claim `ref`.
	GitRef      string
	EventName   string
	Environment opt.Val[string]
	WorkflowRef opt.Val[string]
	RunID       opt.Val[string]
	Actor       opt.Val[string]
}

// TokenError is why a provider token is not acceptable, whatever the policy.
type TokenError string

// The reasons a token is refused.
const (
	TokenIssuer      TokenError = "the token was issued by another issuer"
	TokenAudience    TokenError = "the token is for another audience"
	TokenExpired     TokenError = "the token has expired"
	TokenNotYetValid TokenError = "the token is not valid yet"
	TokenTooLong     TokenError = "the token lives too long"
	TokenNoID        TokenError = "the token has no id"
)

func (e TokenError) Error() string { return string(e) }

// Check checks issuer, audience and times at now (Unix seconds). The error
// is a [TokenError].
func (c GithubClaims) Check(issuer, audience string, now int64) error {
	if c.Iss != issuer {
		return TokenIssuer
	}
	if !slices.Contains(c.Aud, audience) {
		return TokenAudience
	}
	if c.Jti == "" || len(c.Jti) > 256 {
		return TokenNoID
	}
	if now >= clock.SaturatingAdd(c.Exp, ClockLeewaySecs) {
		return TokenExpired
	}
	notBefore := max(c.Nbf.Or(c.Iat), c.Iat)
	if clock.SaturatingAdd(now, ClockLeewaySecs) < notBefore {
		return TokenNotYetValid
	}
	if saturatingSub(c.Exp, c.Iat) > MaxOIDCLifetimeSecs {
		return TokenTooLong
	}
	return nil
}

// InvalidReason is the kind of fault an [InvalidPolicy] names.
type InvalidReason string

// The faults of a policy. The values are for logs; the message people see
// is [InvalidPolicy.Error].
const (
	InvalidRefCount   InvalidReason = "ref_count"
	InvalidRef        InvalidReason = "ref"
	InvalidListLength InvalidReason = "list_length"
	InvalidEvent      InvalidReason = "event"
	InvalidRole       InvalidReason = "role"
	InvalidTTL        InvalidReason = "ttl"
	InvalidIDs        InvalidReason = "ids"
)

// InvalidPolicy is why a policy was refused. Value is the offending ref
// pattern, event or role for the reasons that have one.
type InvalidPolicy struct {
	Reason InvalidReason
	Value  string
}

func (e InvalidPolicy) Error() string {
	switch e.Reason {
	case InvalidRefCount:
		return fmt.Sprintf("a policy names 1 to %d refs", maxPatterns)
	case InvalidRef:
		return fmt.Sprintf("ref pattern %q must start with refs/heads/ or refs/tags/, with `*` only at the end", e.Value)
	case InvalidListLength:
		return fmt.Sprintf("a policy names at most %d environments and events", maxPatterns)
	case InvalidEvent:
		return fmt.Sprintf("unknown or unsafe event %q", e.Value)
	case InvalidRole:
		return fmt.Sprintf("CI tokens are capped at developer or admin, not %s", e.Value)
	case InvalidTTL:
		return fmt.Sprintf("CI tokens live between %d and %d seconds", MinCITokenTTLSecs, MaxCITokenTTLSecs)
	case InvalidIDs:
		return "repository and owner ids must be set"
	}
	return "invalid policy: " + string(e.Reason)
}

// Denied is why a token did not match a policy.
type Denied string

// The reasons a token does not match.
const (
	DeniedRepository  Denied = "another repository"
	DeniedOwner       Denied = "another repository owner"
	DeniedRef         Denied = "a ref the policy does not allow"
	DeniedEnvironment Denied = "an environment the policy does not allow"
	DeniedEvent       Denied = "an event the policy does not allow"
)

func (e Denied) Error() string { return string(e) }

// TrustPolicy is a trust policy for one GitHub repository.
type TrustPolicy struct {
	RepositoryID      uint64 `json:"repositoryId"`
	RepositoryOwnerID uint64 `json:"repositoryOwnerId"`
	// Refs are patterns such as `refs/heads/main` and `refs/tags/v*`.
	Refs []string `json:"refs"`
	// Environments are the GitHub environments allowed; empty for any (or
	// none).
	Environments []string `json:"environments"`
	// Events are the workflow events allowed.
	Events []string `json:"events"`
	// Role is the role exchanged tokens are capped at.
	Role         perm.Role `json:"role"`
	TokenTTLSecs uint32    `json:"tokenTtlSecs"`
}

func patternOK(pattern string) bool {
	body := strings.TrimSuffix(pattern, "*")
	return (strings.HasPrefix(pattern, "refs/heads/") || strings.HasPrefix(pattern, "refs/tags/")) &&
		!strings.Contains(body, "*") &&
		len(pattern) <= 255 &&
		!strings.ContainsFunc(pattern, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}

// RefMatches reports whether gitRef matches pattern: exactly, or as a prefix
// before a final `*`.
func RefMatches(pattern, gitRef string) bool {
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(gitRef, prefix)
	}
	return pattern == gitRef
}

// Validate refuses a policy that is not safe to store. The error is an
// [InvalidPolicy].
func (p TrustPolicy) Validate() error {
	if p.RepositoryID == 0 || p.RepositoryOwnerID == 0 {
		return InvalidPolicy{Reason: InvalidIDs}
	}
	if len(p.Refs) == 0 || len(p.Refs) > maxPatterns {
		return InvalidPolicy{Reason: InvalidRefCount}
	}
	if i := slices.IndexFunc(p.Refs, func(r string) bool { return !patternOK(r) }); i >= 0 {
		return InvalidPolicy{Reason: InvalidRef, Value: p.Refs[i]}
	}
	if len(p.Environments) > maxPatterns || len(p.Events) == 0 || len(p.Events) > maxPatterns {
		return InvalidPolicy{Reason: InvalidListLength}
	}
	known := knownEvents()
	if i := slices.IndexFunc(p.Events, func(e string) bool { return !slices.Contains(known, e) }); i >= 0 {
		return InvalidPolicy{Reason: InvalidEvent, Value: p.Events[i]}
	}
	if p.Role != perm.Developer && p.Role != perm.Admin {
		return InvalidPolicy{Reason: InvalidRole, Value: string(p.Role)}
	}
	if p.TokenTTLSecs < MinCITokenTTLSecs || p.TokenTTLSecs > MaxCITokenTTLSecs {
		return InvalidPolicy{Reason: InvalidTTL}
	}
	return nil
}

// Evaluate reports whether claims satisfy this policy. Deny by default; the
// error is a [Denied].
func (p TrustPolicy) Evaluate(claims GithubClaims) error {
	if claims.RepositoryID != p.RepositoryID {
		return DeniedRepository
	}
	if claims.RepositoryOwnerID != p.RepositoryOwnerID {
		return DeniedOwner
	}
	if slices.Contains(unsafeEvents(), claims.EventName) || !slices.Contains(p.Events, claims.EventName) {
		return DeniedEvent
	}
	if strings.HasPrefix(claims.GitRef, "refs/pull/") ||
		!slices.ContainsFunc(p.Refs, func(pattern string) bool { return RefMatches(pattern, claims.GitRef) }) {
		return DeniedRef
	}
	if len(p.Environments) > 0 {
		env, named := claims.Environment.Get()
		if !named || !slices.Contains(p.Environments, env) {
			return DeniedEnvironment
		}
	}
	return nil
}

func saturatingSub(a, b int64) int64 {
	diff := a - b
	// Overflow exactly when the operands' signs differ and the result's sign
	// is not a's.
	if (a < 0) != (b < 0) && (diff < 0) != (a < 0) {
		if a < 0 {
			return -1 << 63
		}
		return 1<<63 - 1
	}
	return diff
}
