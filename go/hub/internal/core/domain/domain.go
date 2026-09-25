// Package domain has domain names and claims (M5.2; plan §14.3). It replaces
// the Rust module kuben-core/src/domain.rs.
//
// A host is compared only in canonical form: lower case, IDNA (punycode),
// no trailing dot. A verified claim on `example.com` covers the name itself
// and every name below it, so two organizations' claims must never
// overlap.
package domain

import (
	"strings"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ascii"

	"golang.org/x/net/idna"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
)

// ChallengeLabel is the label of the TXT record that proves a claim on a
// domain.
const ChallengeLabel = "_kuben-challenge"

// ErrorKind says why a name is not a claimable domain.
type ErrorKind string

// The kinds.
const (
	// Invalid: the name is not a domain name at all.
	Invalid ErrorKind = "invalid"
	// TooShort: a single label, such as `localhost`.
	TooShort ErrorKind = "tooShort"
	// Wildcard: `*.example.com`; the claim on `example.com` covers it.
	Wildcard ErrorKind = "wildcard"
)

// Error is a refused domain name. Name is the text the message shows: the
// name as given for Invalid, the canonical name for TooShort and the name
// to claim instead for Wildcard.
type Error struct {
	Kind ErrorKind
	Name string
}

func (e *Error) Error() string {
	switch e.Kind {
	case Invalid:
		return "`" + e.Name + "` is not a valid domain name"
	case TooShort:
		return "`" + e.Name + "` needs at least two labels"
	case Wildcard:
		return "wildcards are not claimed: claim `" + e.Name + "`, which covers every name below it"
	}
	return "`" + e.Name + "` is not a claimable domain"
}

// Unwrap makes the error a validation failure for errors.Is and kerr.CodeOf.
func (e *Error) Unwrap() error {
	return kerr.New(kerr.Validation, "%s", e.Error())
}

// uts46 is UTS-46 non-transitional processing without STD3 rules, hyphen
// checks or DNS length checks, as Rust's idna::domain_to_ascii does it;
// Canonical applies the stricter LDH and length rules to the result itself.
func uts46() *idna.Profile {
	return idna.New(
		idna.MapForLookup(),
		idna.BidiRule(),
		idna.CheckJoiners(true),
		idna.CheckHyphens(false),
		idna.StrictDomainName(false),
		idna.VerifyDNSLength(false),
	)
}

// Canonical is name in canonical form: IDNA (punycode), lower case, no
// trailing dot.
func Canonical(name string) (string, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(name), ".")
	if rest, ok := strings.CutPrefix(trimmed, "*."); ok {
		return "", &Error{Kind: Wildcard, Name: ascii.Lower(rest)}
	}
	ascii, err := uts46().ToASCII(trimmed)
	if err != nil || !validASCII(ascii) {
		return "", &Error{Kind: Invalid, Name: name}
	}
	if !strings.Contains(ascii, ".") {
		return "", &Error{Kind: TooShort, Name: ascii}
	}
	return ascii, nil
}

func validASCII(ascii string) bool {
	if ascii == "" || len(ascii) > 253 {
		return false
	}
	for label := range strings.SplitSeq(ascii, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := range len(label) {
			if !isAlnum(label[i]) && label[i] != '-' {
				return false
			}
		}
	}
	return true
}

func isAlnum(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// Covers reports whether a claim on claim covers host (both canonical).
func Covers(claim, host string) bool {
	if host == claim {
		return true
	}
	prefix, ok := strings.CutSuffix(host, claim)
	return ok && strings.HasSuffix(prefix, ".")
}

// Overlaps reports whether claims on a and b (both canonical) overlap.
func Overlaps(a, b string) bool {
	return Covers(a, b) || Covers(b, a)
}

// LockKey is the name whose lock serializes claims that could overlap
// domain: its last two labels. The string is the input of a PostgreSQL
// advisory lock shared with existing installations.
func LockKey(domain string) string {
	last := strings.LastIndexByte(domain, '.')
	if last < 0 {
		return domain
	}
	at := strings.LastIndexByte(domain[:last], '.')
	if at < 0 {
		return domain
	}
	return domain[at+1:]
}

// ChallengeName is the TXT record name for domain.
func ChallengeName(domain string) string {
	return ChallengeLabel + "." + domain
}

// Ancestors is every name that could hold a claim covering host, from the
// host itself up to its last two labels (the candidates to look up).
func Ancestors(host string) []string {
	out := []string{host}
	rest := host
	for {
		_, parent, ok := strings.Cut(rest, ".")
		if !ok || !strings.Contains(parent, ".") {
			return out
		}
		out = append(out, parent)
		rest = parent
	}
}
