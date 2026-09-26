// Package sso is single sign-on with an OpenID Connect identity provider
// (M4.3; plan §13, S04): which people may sign in and with which
// organization role. It replaces the Rust module kuben-core/src/sso.rs.
//
// The role comes from the provider's groups through an explicit mapping and
// is set again at every sign-in, so a person moved out of a group loses the
// role the next time they sign in, and a person in no mapped group (and no
// default role) cannot sign in at all. Email addresses must be verified by
// the provider and may be limited to domains.
package sso

import (
	"slices"
	"strings"
	"unicode"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ascii"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/perm"
)

const (
	// ClockLeewaySecs is the clock skew tolerated on `exp` and `iat`.
	ClockLeewaySecs int64 = 60
	// MaxIDTokenLifetimeSecs is the longest an ID token may live; a longer
	// lived one is refused.
	MaxIDTokenLifetimeSecs int64 = 24 * 3600
	// LoginWindowSecs is how long a started sign-in may take.
	LoginWindowSecs int64 = 10 * 60
)

// IDClaims are the claims of an ID token Kuben reads. Groups come from a
// configurable claim, so the rest of the payload is kept in Other. They are
// decoded from JSON only; `aud` may be one string or a list.
type IDClaims struct {
	Iss           string
	Aud           []string
	Sub           string
	Exp           int64
	Iat           int64
	Nonce         opt.Val[string]
	Email         opt.Val[string]
	EmailVerified opt.Val[bool]
	Name          opt.Val[string]
	// Other has every claim not named above, as encoding/json decodes it.
	Other map[string]any
}

// Denied is why an ID token or the person it names was refused.
type Denied string

// The reasons a sign-in is refused.
const (
	DeniedIssuer      Denied = "the token was issued by another issuer"
	DeniedAudience    Denied = "the token is for another client"
	DeniedExpired     Denied = "the token has expired or lives too long"
	DeniedNotYetValid Denied = "the token is not valid yet"
	DeniedNonce       Denied = "the token does not belong to this sign-in"
	DeniedIdentity    Denied = "the provider sent no subject or email"
	DeniedUnverified  Denied = "the provider has not verified the email address"
	DeniedDomain      Denied = "the email domain is not allowed"
	DeniedNoRole      Denied = "no group of this person is mapped to a role"
)

func (e Denied) Error() string { return string(e) }

// Check checks issuer, audience, nonce and times at now (Unix seconds). The
// error is a [Denied].
func (c IDClaims) Check(issuer, clientID, nonce string, now int64) error {
	if c.Iss != issuer {
		return DeniedIssuer
	}
	if !slices.Contains(c.Aud, clientID) {
		return DeniedAudience
	}
	if got, ok := c.Nonce.Get(); !ok || got != nonce {
		return DeniedNonce
	}
	if now >= clock.SaturatingAdd(c.Exp, ClockLeewaySecs) || clock.SaturatingSub(c.Exp, c.Iat) > MaxIDTokenLifetimeSecs {
		return DeniedExpired
	}
	if clock.SaturatingAdd(now, ClockLeewaySecs) < c.Iat {
		return DeniedNotYetValid
	}
	return nil
}

// Groups are the groups in claim: a list of strings (anything else in it is
// skipped), or one string.
func (c IDClaims) Groups(claim string) []string {
	switch v := c.Other[claim].(type) {
	case []any:
		groups := make([]string, 0, len(v))
		for _, item := range v {
			if g, ok := item.(string); ok {
				groups = append(groups, g)
			}
		}
		return groups
	case string:
		return []string{v}
	}
	return nil
}

// Policy is who may sign in, and as what.
type Policy struct {
	// Groups maps a provider group to an organization role.
	Groups map[string]perm.Role
	// DefaultRole is the role of people in no mapped group; absent refuses
	// them.
	DefaultRole opt.Val[perm.Role]
	// AllowedDomains are the email domains allowed (lowercase); empty for
	// any.
	AllowedDomains []string
	// RequireVerifiedEmail refuses emails the provider has not verified (or
	// says nothing about).
	RequireVerifiedEmail bool
}

// Person is a person the provider vouched for.
type Person struct {
	Subject string
	// Email is trimmed and lowercase.
	Email string
	Name  opt.Val[string]
	Role  perm.Role
}

// Admit is the person claims names, with the role their groups earn: the
// strongest mapped one, else the default. The error is a [Denied].
func (p Policy) Admit(claims IDClaims, groups []string) (Person, error) {
	email := ascii.Lower(strings.TrimSpace(claims.Email.Or("")))
	if !strings.Contains(email, "@") {
		return Person{}, DeniedIdentity
	}
	if claims.Sub == "" || len(claims.Sub) > 255 {
		return Person{}, DeniedIdentity
	}
	if p.RequireVerifiedEmail && !claims.EmailVerified.Or(false) {
		return Person{}, DeniedUnverified
	}
	domain := email[strings.LastIndex(email, "@")+1:]
	if len(p.AllowedDomains) > 0 && !slices.Contains(p.AllowedDomains, domain) {
		return Person{}, DeniedDomain
	}
	granted, ok := p.roleOf(groups).Get()
	if !ok {
		return Person{}, DeniedNoRole
	}
	name := claims.Name
	if n, named := name.Get(); named && strings.TrimSpace(n) == "" {
		name = opt.None[string]()
	}
	return Person{Subject: claims.Sub, Email: email, Name: name, Role: granted}, nil
}

// roleOf is the strongest role groups are mapped to, else the default.
func (p Policy) roleOf(groups []string) opt.Val[perm.Role] {
	strongest := opt.None[perm.Role]()
	for _, g := range groups {
		mapped, ok := p.Groups[g]
		if !ok {
			continue
		}
		if best, found := strongest.Get(); !found || mapped.Rank() >= best.Rank() {
			strongest = opt.Some(mapped)
		}
	}
	if strongest.IsSome() {
		return strongest
	}
	return p.DefaultRole
}

// SafeReturnTo is where to go after signing in: path when it is a path on
// this site, and `/` for anything else (nothing, another site, a
// scheme-relative URL, backslashes, control characters, more than 512
// bytes).
func SafeReturnTo(path string) string {
	if strings.HasPrefix(path, "/") &&
		!strings.HasPrefix(path, "//") &&
		!strings.Contains(path, `\`) &&
		len(path) <= 512 &&
		!strings.ContainsFunc(path, unicode.IsControl) {
		return path
	}
	return "/"
}
