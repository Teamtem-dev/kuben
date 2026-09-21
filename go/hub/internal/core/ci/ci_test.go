package ci_test

import (
	"encoding/json"
	"errors"
	"maps"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ci"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/perm"
)

const (
	now      int64 = 1_800_000_100
	audience       = "https://kuben.example.com"
)

// claims are the claims of a push to main, with the members of extra on top.
func claims(t *testing.T, extra map[string]any) ci.GithubClaims {
	t.Helper()
	base := map[string]any{
		"iss": ci.GithubActionsIssuer, "aud": audience, "sub": "repo:acme/shop:ref:refs/heads/main",
		"jti": "j-1", "iat": 1_800_000_000, "nbf": 1_800_000_000, "exp": 1_800_000_300,
		"repository": "acme/shop", "repository_id": "123456", "repository_owner": "acme",
		"repository_owner_id": "42", "ref": "refs/heads/main", "event_name": "push",
		"environment": "production",
	}
	maps.Copy(base, extra)
	return decode(t, base)
}

func decode(t *testing.T, members map[string]any) ci.GithubClaims {
	t.Helper()
	raw, err := json.Marshal(members)
	if err != nil {
		t.Fatal(err)
	}
	var c ci.GithubClaims
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func policy() ci.TrustPolicy {
	return ci.TrustPolicy{
		RepositoryID:      123_456,
		RepositoryOwnerID: 42,
		Refs:              []string{"refs/heads/main", "refs/tags/v*"},
		Events:            ci.DefaultEvents(),
		Role:              perm.Developer,
		TokenTTLSecs:      ci.DefaultCITokenTTLSecs,
	}
}

func TestClaimsParseGithubShapes(t *testing.T) {
	c := claims(t, nil)
	if c.RepositoryID != 123_456 || c.RepositoryOwnerID != 42 {
		t.Errorf("got %d, %d", c.RepositoryID, c.RepositoryOwnerID)
	}
	if diff := cmp.Diff([]string{audience}, c.Aud); diff != "" {
		t.Error(diff)
	}
	if c.GitRef != "refs/heads/main" || c.Nbf != opt.Some[int64](1_800_000_000) || c.Environment != opt.Some("production") {
		t.Errorf("got %+v", c)
	}
	many := decode(t, map[string]any{
		"iss": "i", "aud": []string{"a", "b"}, "sub": "s", "jti": "j", "iat": 1, "exp": 2,
		"repository": "r", "repository_id": 7, "repository_owner": "o",
		"repository_owner_id": "8", "ref": "refs/heads/x", "event_name": "push",
	})
	if len(many.Aud) != 2 || many.RepositoryID != 7 || many.RepositoryOwnerID != 8 || many.Environment.IsSome() || many.Nbf.IsSome() {
		t.Errorf("got %+v", many)
	}
	refused := []string{
		`{"iss": "i"}`,
		`[]`,
		`{"iss":"i","aud":"a","sub":"s","jti":"j","iat":1,"exp":2,"repository":"r","repository_id":"x","repository_owner":"o","repository_owner_id":"8","ref":"r","event_name":"push"}`,
		`{"iss":"i","aud":"a","sub":"s","jti":"j","iat":1,"exp":2,"repository":"r","repository_id":-7,"repository_owner":"o","repository_owner_id":"8","ref":"r","event_name":"push"}`,
		`{"iss":"i","aud":"a","sub":"s","jti":"j","iat":1,"exp":2,"repository":"r","repository_id":7.5,"repository_owner":"o","repository_owner_id":"8","ref":"r","event_name":"push"}`,
		`{"iss":null,"aud":"a","sub":"s","jti":"j","iat":1,"exp":2,"repository":"r","repository_id":7,"repository_owner":"o","repository_owner_id":"8","ref":"r","event_name":"push"}`,
		`{"iss":"i","aud":7,"sub":"s","jti":"j","iat":1,"exp":2,"repository":"r","repository_id":7,"repository_owner":"o","repository_owner_id":"8","ref":"r","event_name":"push"}`,
	}
	for _, raw := range refused {
		var c ci.GithubClaims
		if err := json.Unmarshal([]byte(raw), &c); err == nil {
			t.Errorf("%s must be refused", raw)
		}
	}
}

func TestTokensAreCheckedForIssuerAudienceAndTime(t *testing.T) {
	c := claims(t, nil)
	long, anonymous := c, c
	long.Exp = c.Iat + ci.MaxOIDCLifetimeSecs + 1
	anonymous.Jti = ""
	late := c
	late.Nbf = opt.Some(c.Iat + 1000)
	cases := []struct {
		name     string
		claims   ci.GithubClaims
		issuer   string
		audience string
		now      int64
		want     error
	}{
		{"valid", c, ci.GithubActionsIssuer, audience, now, nil},
		{"issuer", c, "https://evil", audience, now, ci.TokenIssuer},
		{"audience", c, ci.GithubActionsIssuer, "other", now, ci.TokenAudience},
		{"expired", c, ci.GithubActionsIssuer, audience, c.Exp + ci.ClockLeewaySecs, ci.TokenExpired},
		{"within the leeway", c, ci.GithubActionsIssuer, audience, c.Exp + 10, nil},
		{"early", c, ci.GithubActionsIssuer, audience, c.Iat - ci.ClockLeewaySecs - 1, ci.TokenNotYetValid},
		{"nbf after iat", late, ci.GithubActionsIssuer, audience, now, ci.TokenNotYetValid},
		{"too long", long, ci.GithubActionsIssuer, audience, now, ci.TokenTooLong},
		{"no id", anonymous, ci.GithubActionsIssuer, audience, now, ci.TokenNoID},
	}
	for _, tc := range cases {
		if got := tc.claims.Check(tc.issuer, tc.audience, tc.now); !errors.Is(got, tc.want) || (tc.want == nil) != (got == nil) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
	extreme := c
	extreme.Exp, extreme.Iat = 1<<63-1, -1<<63
	if got := extreme.Check(ci.GithubActionsIssuer, audience, now); got != ci.TokenTooLong {
		t.Errorf("overflow must saturate, got %v", got)
	}
}

func TestRefPatternsAreExactOrTrailingPrefixes(t *testing.T) {
	cases := []struct {
		pattern, ref string
		want         bool
	}{
		{"refs/heads/main", "refs/heads/main", true},
		{"refs/heads/main", "refs/heads/main2", false},
		{"refs/tags/v*", "refs/tags/v1.2.3", true},
		{"refs/tags/v*", "refs/heads/v1", false},
	}
	for _, c := range cases {
		if got := ci.RefMatches(c.pattern, c.ref); got != c.want {
			t.Errorf("RefMatches(%q, %q) = %v", c.pattern, c.ref, got)
		}
	}
}

func TestAMatchingWorkflowIsTrusted(t *testing.T) {
	if err := policy().Evaluate(claims(t, nil)); err != nil {
		t.Error(err)
	}
	tag := claims(t, map[string]any{"ref": "refs/tags/v2.0.0", "event_name": "release"})
	if err := policy().Evaluate(tag); err != nil {
		t.Error(err)
	}
}

func TestEverythingElseIsDenied(t *testing.T) {
	everything, permissive := policy(), policy()
	everything.Refs, everything.Events = []string{"refs/heads/*"}, []string{"push"}
	permissive.Events = []string{"pull_request_target"}
	cases := []struct {
		name   string
		policy ci.TrustPolicy
		extra  map[string]any
		want   ci.Denied
	}{
		{"repository", policy(), map[string]any{"repository_id": 1}, ci.DeniedRepository},
		{"owner", policy(), map[string]any{"repository_owner_id": 1}, ci.DeniedOwner},
		{"ref", policy(), map[string]any{"ref": "refs/heads/feature"}, ci.DeniedRef},
		{"unsafe event", policy(), map[string]any{"event_name": "pull_request"}, ci.DeniedEvent},
		{"event not named", policy(), map[string]any{"event_name": "schedule"}, ci.DeniedEvent},
		{"pull-request refs never match", everything, map[string]any{"ref": "refs/pull/7/merge"}, ci.DeniedRef},
		{"unsafe even when named", permissive, map[string]any{"event_name": "pull_request_target"}, ci.DeniedEvent},
	}
	for _, tc := range cases {
		if got := tc.policy.Evaluate(claims(t, tc.extra)); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestEnvironmentsNarrowAPolicy(t *testing.T) {
	p := policy()
	p.Environments = []string{"production"}
	if err := p.Evaluate(claims(t, nil)); err != nil {
		t.Error(err)
	}
	if got := p.Evaluate(claims(t, map[string]any{"environment": "staging"})); got != ci.DeniedEnvironment {
		t.Errorf("got %v", got)
	}
	if got := p.Evaluate(claims(t, map[string]any{"environment": nil})); got != ci.DeniedEnvironment {
		t.Errorf("got %v", got)
	}
}

func TestPoliciesAreValidated(t *testing.T) {
	if err := policy().Validate(); err != nil {
		t.Fatal(err)
	}
	with := func(change func(*ci.TrustPolicy)) ci.TrustPolicy {
		p := policy()
		change(&p)
		return p
	}
	cases := []struct {
		policy  ci.TrustPolicy
		want    ci.InvalidPolicy
		message string
	}{
		{
			with(func(p *ci.TrustPolicy) { p.Refs = nil }),
			ci.InvalidPolicy{Reason: ci.InvalidRefCount},
			"a policy names 1 to 20 refs",
		},
		{
			with(func(p *ci.TrustPolicy) { p.Refs = make([]string, 21) }),
			ci.InvalidPolicy{Reason: ci.InvalidRefCount},
			"a policy names 1 to 20 refs",
		},
		{
			with(func(p *ci.TrustPolicy) { p.Refs = []string{"main"} }),
			ci.InvalidPolicy{Reason: ci.InvalidRef, Value: "main"},
			"ref pattern \"main\" must start with refs/heads/ or refs/tags/, with `*` only at the end",
		},
		{
			with(func(p *ci.TrustPolicy) { p.Refs = []string{"refs/pull/*"} }),
			ci.InvalidPolicy{Reason: ci.InvalidRef, Value: "refs/pull/*"},
			"ref pattern \"refs/pull/*\" must start with refs/heads/ or refs/tags/, with `*` only at the end",
		},
		{
			with(func(p *ci.TrustPolicy) { p.Refs = []string{"refs/heads/*/x"} }),
			ci.InvalidPolicy{Reason: ci.InvalidRef, Value: "refs/heads/*/x"},
			"ref pattern \"refs/heads/*/x\" must start with refs/heads/ or refs/tags/, with `*` only at the end",
		},
		{
			with(func(p *ci.TrustPolicy) { p.Refs = []string{"refs/heads/a b"} }),
			ci.InvalidPolicy{Reason: ci.InvalidRef, Value: "refs/heads/a b"},
			"ref pattern \"refs/heads/a b\" must start with refs/heads/ or refs/tags/, with `*` only at the end",
		},
		{
			with(func(p *ci.TrustPolicy) { p.Events = nil }),
			ci.InvalidPolicy{Reason: ci.InvalidListLength},
			"a policy names at most 20 environments and events",
		},
		{
			with(func(p *ci.TrustPolicy) { p.Events = []string{"pull_request"} }),
			ci.InvalidPolicy{Reason: ci.InvalidEvent, Value: "pull_request"},
			"unknown or unsafe event \"pull_request\"",
		},
		{
			with(func(p *ci.TrustPolicy) { p.Role = perm.Owner }),
			ci.InvalidPolicy{Reason: ci.InvalidRole, Value: "owner"},
			"CI tokens are capped at developer or admin, not owner",
		},
		{
			with(func(p *ci.TrustPolicy) { p.Role = perm.Viewer }),
			ci.InvalidPolicy{Reason: ci.InvalidRole, Value: "viewer"},
			"CI tokens are capped at developer or admin, not viewer",
		},
		{
			with(func(p *ci.TrustPolicy) { p.TokenTTLSecs = 7200 }),
			ci.InvalidPolicy{Reason: ci.InvalidTTL},
			"CI tokens live between 60 and 3600 seconds",
		},
		{
			with(func(p *ci.TrustPolicy) { p.RepositoryID = 0 }),
			ci.InvalidPolicy{Reason: ci.InvalidIDs},
			"repository and owner ids must be set",
		},
	}
	for _, tc := range cases {
		var got ci.InvalidPolicy
		if err := tc.policy.Validate(); !errors.As(err, &got) || got != tc.want || got.Error() != tc.message {
			t.Errorf("%+v: got %v, want %v", tc.policy, err, tc.message)
		}
	}
}

func TestPolicyWireForm(t *testing.T) {
	raw, err := json.Marshal(policy())
	if err != nil {
		t.Fatal(err)
	}
	want := `{"repositoryId":123456,"repositoryOwnerId":42,"refs":["refs/heads/main","refs/tags/v*"],` +
		`"environments":[],"events":["push","workflow_dispatch","release"],"role":"developer","tokenTtlSecs":900}`
	if string(raw) != want {
		t.Errorf("got %s", raw)
	}
	var back ci.TrustPolicy
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(policy().Refs, back.Refs); diff != "" || back.Role != perm.Developer || back.TokenTTLSecs != 900 {
		t.Errorf("got %+v", back)
	}
	without := `{"repositoryId":1,"repositoryOwnerId":2,"refs":["refs/heads/main"],"events":["push"],"role":"admin","tokenTtlSecs":60}`
	if err := json.Unmarshal([]byte(without), &back); err != nil || back.Role != perm.Admin || len(back.Environments) != 0 {
		t.Errorf("environments may be left out: %v, %+v", err, back)
	}
	refused := []string{
		`{"repositoryOwnerId":2,"refs":["refs/heads/main"],"events":["push"],"role":"admin","tokenTtlSecs":60}`,
		`{"repositoryId":1,"repositoryOwnerId":2,"refs":null,"events":["push"],"role":"admin","tokenTtlSecs":60}`,
		`{"repositoryId":1,"repositoryOwnerId":2,"refs":["refs/heads/main"],"events":["push"],"role":"root","tokenTtlSecs":60}`,
		`{"repositoryId":1,"repositoryOwnerId":2,"refs":["refs/heads/main"],"events":["push"],"role":"admin","tokenTtlSecs":-1}`,
		`{"repositoryId":1,"repositoryOwnerId":2,"refs":["refs/heads/main"],"environments":null,"events":["push"],"role":"admin","tokenTtlSecs":60}`,
	}
	for _, r := range refused {
		if err := json.Unmarshal([]byte(r), &back); err == nil {
			t.Errorf("%s must be refused", r)
		}
	}
}
