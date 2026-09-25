package domain_test

import (
	"errors"
	"strings"
	"testing"
	"testing/quick"

	"github.com/google/go-cmp/cmp"

	domain "github.com/Teamtem-dev/kuben/internal/core/dnsname"
	kerr "github.com/Teamtem-dev/kuben/internal/core/kerrors"
)

func TestNamesAreMadeCanonical(t *testing.T) {
	good := []struct{ in, want string }{
		{"App.Example.COM.", "app.example.com"},
		{"  App.Example.COM  ", "app.example.com"},
		{"bücher.example", "xn--bcher-kva.example"},
		{"BÜCHER.example", "xn--bcher-kva.example"},
		{"xn--bcher-kva.example", "xn--bcher-kva.example"},
		{"XN--BCHER-KVA.Example", "xn--bcher-kva.example"},
		// Non-transitional processing keeps the sharp s.
		{"faß.de", "xn--fa-hia.de"},
		// Hyphens in the third and fourth place are allowed, as in Rust.
		{"ab--cd.com", "ab--cd.com"},
		// No STD3 rules while mapping: the result only has to be LDH.
		{"a≠b.com", "xn--ab-miv.com"},
		{"1.example.com", "1.example.com"},
		{strings.Repeat("a", 63) + ".com", strings.Repeat("a", 63) + ".com"},
	}
	for _, c := range good {
		got, err := domain.Canonical(c.in)
		if err != nil || got != c.want {
			t.Errorf("Canonical(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

func TestRefusedNames(t *testing.T) {
	long := strings.Repeat("a", 64) + ".com"
	tooLong := strings.Repeat(strings.Repeat("a", 60)+".", 4) + "example.com"
	cases := []struct {
		in   string
		want domain.Error
	}{
		{"localhost", domain.Error{Kind: domain.TooShort, Name: "localhost"}},
		{"LocalHost.", domain.Error{Kind: domain.TooShort, Name: "localhost"}},
		{"*.example.com", domain.Error{Kind: domain.Wildcard, Name: "example.com"}},
		{" *.Example.COM. ", domain.Error{Kind: domain.Wildcard, Name: "example.com"}},
		{"", domain.Error{Kind: domain.Invalid, Name: ""}},
		{"a..b", domain.Error{Kind: domain.Invalid, Name: "a..b"}},
		{"-a.com", domain.Error{Kind: domain.Invalid, Name: "-a.com"}},
		{"a-.com", domain.Error{Kind: domain.Invalid, Name: "a-.com"}},
		{"a_b.com", domain.Error{Kind: domain.Invalid, Name: "a_b.com"}},
		{"a b.com", domain.Error{Kind: domain.Invalid, Name: "a b.com"}},
		{long, domain.Error{Kind: domain.Invalid, Name: long}},
		{tooLong, domain.Error{Kind: domain.Invalid, Name: tooLong}},
		{"a.*.com", domain.Error{Kind: domain.Invalid, Name: "a.*.com"}},
		{"xn--zz.com", domain.Error{Kind: domain.Invalid, Name: "xn--zz.com"}},
		{"a\u200db.com", domain.Error{Kind: domain.Invalid, Name: "a\u200db.com"}},
		{"example.com。", domain.Error{Kind: domain.Invalid, Name: "example.com。"}},
		{" a_b.com ", domain.Error{Kind: domain.Invalid, Name: " a_b.com "}},
	}
	for _, c := range cases {
		got, err := domain.Canonical(c.in)
		var de *domain.Error
		if !errors.As(err, &de) {
			t.Errorf("Canonical(%q) = %q, %v; want a *domain.Error", c.in, got, err)
			continue
		}
		if diff := cmp.Diff(c.want, *de); diff != "" || got != "" {
			t.Errorf("Canonical(%q) = %q (-want +got):\n%s", c.in, got, diff)
		}
		if !errors.Is(err, kerr.ErrValidation) {
			t.Errorf("Canonical(%q): %v is not a validation error", c.in, err)
		}
	}
}

func TestErrorMessagesArePinned(t *testing.T) {
	cases := []struct {
		err  domain.Error
		want string
	}{
		{domain.Error{Kind: domain.Invalid, Name: "a b"}, "`a b` is not a valid domain name"},
		{domain.Error{Kind: domain.TooShort, Name: "localhost"}, "`localhost` needs at least two labels"},
		{
			domain.Error{Kind: domain.Wildcard, Name: "example.com"},
			"wildcards are not claimed: claim `example.com`, which covers every name below it",
		},
	}
	for _, c := range cases {
		if got := c.err.Error(); got != c.want {
			t.Errorf("got %q, want %q", got, c.want)
		}
	}
}

func TestClaimsCoverTheirSubdomainsOnly(t *testing.T) {
	covers := []struct {
		claim, host string
		want        bool
	}{
		{"example.com", "example.com", true},
		{"example.com", "a.b.example.com", true},
		{"example.com", "badexample.com", false},
		{"a.example.com", "example.com", false},
	}
	for _, c := range covers {
		if got := domain.Covers(c.claim, c.host); got != c.want {
			t.Errorf("Covers(%q, %q) = %v", c.claim, c.host, got)
		}
	}
	if !domain.Overlaps("a.example.com", "example.com") || !domain.Overlaps("example.com", "a.example.com") {
		t.Error("a claim overlaps the claim above it, in either order")
	}
	if domain.Overlaps("a.example.com", "b.example.com") {
		t.Error("siblings do not overlap")
	}
}

func TestLocksAndAncestorsFollowTheLastTwoLabels(t *testing.T) {
	keys := []struct{ in, want string }{
		{"a.b.example.com", "example.com"},
		{"example.com", "example.com"},
		{"xn--bcher-kva.example", "xn--bcher-kva.example"},
		{"localhost", "localhost"},
		{"", ""},
	}
	for _, c := range keys {
		if got := domain.LockKey(c.in); got != c.want {
			t.Errorf("LockKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	ancestors := []struct {
		in   string
		want []string
	}{
		{"a.b.example.com", []string{"a.b.example.com", "b.example.com", "example.com"}},
		{"example.com", []string{"example.com"}},
		{"localhost", []string{"localhost"}},
	}
	for _, c := range ancestors {
		if diff := cmp.Diff(c.want, domain.Ancestors(c.in)); diff != "" {
			t.Errorf("Ancestors(%q) (-want +got):\n%s", c.in, diff)
		}
	}
	if got := domain.ChallengeName("example.com"); got != "_kuben-challenge.example.com" {
		t.Errorf("got %q", got)
	}
	if domain.ChallengeLabel != "_kuben-challenge" {
		t.Errorf("got %q", domain.ChallengeLabel)
	}
}

// Every ancestor covers the host, shares its lock, and the last one is the
// lock key itself: what serializes overlapping claims.
func TestAncestorsShareTheLockOfTheirHost(t *testing.T) {
	property := func(labels []uint8) bool {
		parts := []string{"example", "com"}
		for _, l := range labels {
			parts = append([]string{string(rune('a' + l%26))}, parts...)
		}
		host := strings.Join(parts, ".")
		ancestors := domain.Ancestors(host)
		for _, a := range ancestors {
			if !domain.Covers(a, host) || domain.LockKey(a) != domain.LockKey(host) {
				return false
			}
		}
		return ancestors[len(ancestors)-1] == domain.LockKey(host)
	}
	if err := quick.Check(property, nil); err != nil {
		t.Error(err)
	}
}
