package dnsname_test

import (
	"errors"
	"strings"
	"testing"
	"testing/quick"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/dnsname"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/kerrors"
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
		got, err := dnsname.Canonical(c.in)
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
		want dnsname.Error
	}{
		{"localhost", dnsname.Error{Kind: dnsname.TooShort, Name: "localhost"}},
		{"LocalHost.", dnsname.Error{Kind: dnsname.TooShort, Name: "localhost"}},
		{"*.example.com", dnsname.Error{Kind: dnsname.Wildcard, Name: "example.com"}},
		{" *.Example.COM. ", dnsname.Error{Kind: dnsname.Wildcard, Name: "example.com"}},
		{"", dnsname.Error{Kind: dnsname.Invalid, Name: ""}},
		{"a..b", dnsname.Error{Kind: dnsname.Invalid, Name: "a..b"}},
		{"-a.com", dnsname.Error{Kind: dnsname.Invalid, Name: "-a.com"}},
		{"a-.com", dnsname.Error{Kind: dnsname.Invalid, Name: "a-.com"}},
		{"a_b.com", dnsname.Error{Kind: dnsname.Invalid, Name: "a_b.com"}},
		{"a b.com", dnsname.Error{Kind: dnsname.Invalid, Name: "a b.com"}},
		{long, dnsname.Error{Kind: dnsname.Invalid, Name: long}},
		{tooLong, dnsname.Error{Kind: dnsname.Invalid, Name: tooLong}},
		{"a.*.com", dnsname.Error{Kind: dnsname.Invalid, Name: "a.*.com"}},
		{"xn--zz.com", dnsname.Error{Kind: dnsname.Invalid, Name: "xn--zz.com"}},
		{"a\u200db.com", dnsname.Error{Kind: dnsname.Invalid, Name: "a\u200db.com"}},
		{"example.com。", dnsname.Error{Kind: dnsname.Invalid, Name: "example.com。"}},
		{" a_b.com ", dnsname.Error{Kind: dnsname.Invalid, Name: " a_b.com "}},
	}
	for _, c := range cases {
		got, err := dnsname.Canonical(c.in)
		var de *dnsname.Error
		if !errors.As(err, &de) {
			t.Errorf("Canonical(%q) = %q, %v; want a *dnsname.Error", c.in, got, err)
			continue
		}
		if diff := cmp.Diff(c.want, *de); diff != "" || got != "" {
			t.Errorf("Canonical(%q) = %q (-want +got):\n%s", c.in, got, diff)
		}
		if !errors.Is(err, kerrors.ErrValidation) {
			t.Errorf("Canonical(%q): %v is not a validation error", c.in, err)
		}
	}
}

func TestErrorMessagesArePinned(t *testing.T) {
	cases := []struct {
		err  dnsname.Error
		want string
	}{
		{dnsname.Error{Kind: dnsname.Invalid, Name: "a b"}, "`a b` is not a valid domain name"},
		{dnsname.Error{Kind: dnsname.TooShort, Name: "localhost"}, "`localhost` needs at least two labels"},
		{
			dnsname.Error{Kind: dnsname.Wildcard, Name: "example.com"},
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
		if got := dnsname.Covers(c.claim, c.host); got != c.want {
			t.Errorf("Covers(%q, %q) = %v", c.claim, c.host, got)
		}
	}
	if !dnsname.Overlaps("a.example.com", "example.com") || !dnsname.Overlaps("example.com", "a.example.com") {
		t.Error("a claim overlaps the claim above it, in either order")
	}
	if dnsname.Overlaps("a.example.com", "b.example.com") {
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
		if got := dnsname.LockKey(c.in); got != c.want {
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
		if diff := cmp.Diff(c.want, dnsname.Ancestors(c.in)); diff != "" {
			t.Errorf("Ancestors(%q) (-want +got):\n%s", c.in, diff)
		}
	}
	if got := dnsname.ChallengeName("example.com"); got != "_kuben-challenge.example.com" {
		t.Errorf("got %q", got)
	}
	if dnsname.ChallengeLabel != "_kuben-challenge" {
		t.Errorf("got %q", dnsname.ChallengeLabel)
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
		ancestors := dnsname.Ancestors(host)
		for _, a := range ancestors {
			if !dnsname.Covers(a, host) || dnsname.LockKey(a) != dnsname.LockKey(host) {
				return false
			}
		}
		return ancestors[len(ancestors)-1] == dnsname.LockKey(host)
	}
	if err := quick.Check(property, nil); err != nil {
		t.Error(err)
	}
}
