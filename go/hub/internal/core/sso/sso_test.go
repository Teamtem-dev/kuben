package sso_test

import (
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/perm"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/sso"
)

const now int64 = 1_800_000_100

// claims are Carol's claims, with the members of extra on top.
func claims(t *testing.T, extra map[string]any) sso.IDClaims {
	t.Helper()
	base := map[string]any{
		"iss": "https://idp.example.com", "aud": "kuben-console", "sub": "u-1",
		"iat": 1_800_000_000, "exp": 1_800_000_300, "nonce": "n-1",
		"email": "Carol@Example.com", "email_verified": true, "name": "Carol",
		"groups": []string{"platform-admins", "devs"},
	}
	maps.Copy(base, extra)
	raw, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	var c sso.IDClaims
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func policy() sso.Policy {
	return sso.Policy{
		Groups:               map[string]perm.Role{"devs": perm.Developer, "platform-admins": perm.Admin},
		AllowedDomains:       []string{"example.com"},
		RequireVerifiedEmail: true,
	}
}

func TestIDTokensAreBoundToIssuerClientNonceAndTime(t *testing.T) {
	c := claims(t, nil)
	cases := []struct {
		name  string
		extra map[string]any
		nonce string
		now   int64
		want  error
	}{
		{"valid", nil, "n-1", now, nil},
		{"another nonce", nil, "n-2", now, sso.DeniedNonce},
		{"no nonce", map[string]any{"nonce": nil}, "n-1", now, sso.DeniedNonce},
		{"issuer", map[string]any{"iss": "https://evil"}, "n-1", now, sso.DeniedIssuer},
		{"audience", map[string]any{"aud": []string{"x", "y"}}, "n-1", now, sso.DeniedAudience},
		{"one of many audiences", map[string]any{"aud": []string{"x", "kuben-console"}}, "n-1", now, nil},
		{"expired", nil, "n-1", c.Exp + sso.ClockLeewaySecs, sso.DeniedExpired},
		{"early", nil, "n-1", c.Iat - sso.ClockLeewaySecs - 1, sso.DeniedNotYetValid},
		{"too long", map[string]any{"exp": 1_800_000_000 + sso.MaxIDTokenLifetimeSecs + 1}, "n-1", now, sso.DeniedExpired},
	}
	for _, tc := range cases {
		got := claims(t, tc.extra).Check("https://idp.example.com", "kuben-console", tc.nonce, tc.now)
		if !errors.Is(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestClaimsKeepTheRestOfThePayload(t *testing.T) {
	c := claims(t, map[string]any{"roles": "ops", "nonce": nil})
	want := map[string]any{"groups": []any{"platform-admins", "devs"}, "roles": "ops"}
	if diff := cmp.Diff(want, c.Other); diff != "" {
		t.Error(diff)
	}
	for _, raw := range []string{`{"iss":"i"}`, `{"iss":"i","aud":1,"sub":"s","exp":2,"iat":1}`, `{"iss":"i","aud":"a","sub":null,"exp":2,"iat":1}`, `[]`} {
		var c sso.IDClaims
		if err := json.Unmarshal([]byte(raw), &c); err == nil {
			t.Errorf("%s must be refused", raw)
		}
	}
}

func TestGroupsComeFromTheConfiguredClaim(t *testing.T) {
	c := claims(t, map[string]any{"roles": "ops", "mixed": []any{"a", 1, "b"}, "count": 3})
	cases := []struct {
		claim string
		want  []string
	}{
		{"groups", []string{"platform-admins", "devs"}},
		{"roles", []string{"ops"}},
		{"mixed", []string{"a", "b"}},
		{"count", nil},
		{"missing", nil},
	}
	for _, tc := range cases {
		if diff := cmp.Diff(tc.want, c.Groups(tc.claim)); diff != "" {
			t.Errorf("%s: %s", tc.claim, diff)
		}
	}
}

func TestTheStrongestMappedGroupDecidesTheRole(t *testing.T) {
	c := claims(t, nil)
	person, err := policy().Admit(c, c.Groups("groups"))
	if err != nil {
		t.Fatal(err)
	}
	want := sso.Person{Subject: "u-1", Email: "carol@example.com", Name: opt.Some("Carol"), Role: perm.Admin}
	if person != want {
		t.Errorf("got %+v", person)
	}
	dev, err := policy().Admit(c, []string{"devs"})
	if err != nil || dev.Role != perm.Developer {
		t.Errorf("got %+v, %v", dev, err)
	}
	blank, err := policy().Admit(claims(t, map[string]any{"name": "  "}), []string{"devs", "platform-admins", "devs"})
	if err != nil || blank.Name.IsSome() || blank.Role != perm.Admin {
		t.Errorf("got %+v, %v", blank, err)
	}
}

func TestUnknownUnverifiedAndForeignPeopleAreRefused(t *testing.T) {
	defaulted, trusting := policy(), policy()
	defaulted.DefaultRole = opt.Some(perm.Viewer)
	trusting.RequireVerifiedEmail = false
	devs := []string{"devs"}
	cases := []struct {
		name   string
		policy sso.Policy
		extra  map[string]any
		groups []string
		want   error
	}{
		{"no mapped group", policy(), nil, []string{"marketing"}, sso.DeniedNoRole},
		{"default role", defaulted, nil, nil, nil},
		{"unverified", policy(), map[string]any{"email_verified": false}, devs, sso.DeniedUnverified},
		{"verification unknown", policy(), map[string]any{"email_verified": nil}, devs, sso.DeniedUnverified},
		{"verification not required", trusting, map[string]any{"email_verified": nil}, devs, nil},
		{"foreign domain", policy(), map[string]any{"email": "eve@evil.example"}, devs, sso.DeniedDomain},
		{"domains match exactly", policy(), map[string]any{"email": "eve@sub.example.com"}, devs, sso.DeniedDomain},
		{"no email", policy(), map[string]any{"email": nil}, devs, sso.DeniedIdentity},
		{"not an address", policy(), map[string]any{"email": "carol"}, devs, sso.DeniedIdentity},
		{"no subject", policy(), map[string]any{"sub": ""}, devs, sso.DeniedIdentity},
		{"long subject", policy(), map[string]any{"sub": strings.Repeat("s", 256)}, devs, sso.DeniedIdentity},
	}
	for _, tc := range cases {
		person, err := tc.policy.Admit(claims(t, tc.extra), tc.groups)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, err, tc.want)
		}
		if tc.name == "default role" && person.Role != perm.Viewer {
			t.Errorf("got %+v", person)
		}
	}
}

func TestSignInsOnlyReturnToThisSite(t *testing.T) {
	if got := sso.SafeReturnTo("/projects/shop"); got != "/projects/shop" {
		t.Errorf("got %q", got)
	}
	bad := []string{"", "//evil.example", "https://evil.example", `/\evil`, "projects", "/a\nb", "/" + strings.Repeat("a", 512)}
	for _, path := range bad {
		if got := sso.SafeReturnTo(path); got != "/" {
			t.Errorf("SafeReturnTo(%q) = %q", path, got)
		}
	}
}
