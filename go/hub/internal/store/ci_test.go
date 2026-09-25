package store_test

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ci"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/model"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/perm"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store/pgtest"
)

type ciFixture struct {
	org     ids.OrgID
	project ids.ProjectID
	owner   ids.UserID
}

func newCIFixture(t *testing.T, s *store.Store) ciFixture {
	t.Helper()
	ctx := t.Context()
	o := org(t, s, "a", "A")
	owner := must[model.User](t, "user")(s.CreateUser(ctx, "owner@example.com", opt.None[string](), opt.None[string]()))
	if err := s.AddMembership(ctx, o, owner.ID); err != nil {
		t.Fatalf("member: %v", err)
	}
	if err := s.BindOrgRole(ctx, o, owner.ID, perm.Owner); err != nil {
		t.Fatalf("role: %v", err)
	}
	tn := tenant(t, s, o)
	project := must[ids.ProjectID](t, "project")(tn.CreateProject(ctx, "shop", "Shop"))
	commit(t, tn)
	return ciFixture{org: o, project: project, owner: owner.ID}
}

func newCIPolicy(f ciFixture, name string) store.NewCIPolicy {
	return store.NewCIPolicy{
		Org:        f.org,
		Project:    f.project,
		Name:       name,
		Repository: "acme/shop",
		Policy: ci.TrustPolicy{
			RepositoryID:      123_456,
			RepositoryOwnerID: 42,
			Refs:              []string{"refs/heads/main"},
			Environments:      []string{},
			Events:            ci.DefaultEvents(),
			Role:              perm.Developer,
			TokenTTLSecs:      ci.DefaultCITokenTTLSecs,
		},
		CreatedBy: f.owner,
	}
}

func ciExchange(f ciFixture, policy uuid.UUID, jti string) store.CIExchange {
	now := time.Now().UnixMilli()
	return store.CIExchange{
		Policy:            policy,
		Issuer:            ci.GithubActionsIssuer,
		Jti:               jti,
		ProviderExpiresAt: now + 300_000,
		Token: store.NewToken{
			ID:         ids.New[ids.Token](),
			OrgID:      f.org,
			Owner:      f.owner,
			Name:       "ci:" + jti,
			Prefix:     "kbn_pat_ci",
			SecretHash: []byte(jti),
			Scope:      model.TokenScope{Role: perm.Developer, Project: opt.Some(f.project.UUID())},
			ExpiresAt:  opt.Some(now + 900_000),
		},
	}
}

// ciCmp compares ids and optional values by value.
func ciCmp() cmp.Option {
	return cmp.AllowUnexported(ids.OrgID{}, ids.ProjectID{}, ids.UserID{}, ids.EnvironmentID{}, opt.Val[ids.EnvironmentID]{}, opt.Val[int64]{})
}

func issued(t *testing.T, got store.Exchanged) model.APIToken {
	t.Helper()
	token, ok := got.(store.ExchangedIssued)
	if !ok {
		t.Fatalf("not issued: %#v", got)
	}
	return token.Token
}

// Ported from repo/ci.rs: policies_round_trip_and_names_are_unique.
func TestPoliciesRoundTripAndNamesAreUnique(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	f := newCIFixture(t, s)
	made := must[store.CIPolicy](t, "create")(s.CreateCIPolicy(ctx, newCIPolicy(f, "deploy")))
	read, found, err := s.CIPolicy(ctx, made.ID)
	if err != nil || !found {
		t.Fatalf("read: %v, %v", found, err)
	}
	if diff := cmp.Diff(made, read, ciCmp()); diff != "" {
		t.Fatalf("read (-made +read):\n%s", diff)
	}
	listed := must[[]store.CIPolicy](t, "list")(s.CIPolicies(ctx, f.org))
	if diff := cmp.Diff([]store.CIPolicy{made}, listed, ciCmp()); diff != "" {
		t.Fatalf("list (-want +got):\n%s", diff)
	}
	if _, err := s.CreateCIPolicy(ctx, newCIPolicy(f, "deploy")); !store.IsUniqueViolation(err) {
		t.Fatalf("a second `deploy`: %v", err)
	}
}

// Ported from repo/ci.rs: a_provider_token_is_exchanged_once.
func TestAProviderTokenIsExchangedOnce(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	f := newCIFixture(t, s)
	policy := must[store.CIPolicy](t, "create")(s.CreateCIPolicy(ctx, newCIPolicy(f, "deploy")))
	token := issued(t, must[store.Exchanged](t, "exchange")(s.ExchangeCIToken(ctx, ciExchange(f, policy.ID, "j-1"))))
	if got, err := s.ExchangeCIToken(ctx, ciExchange(f, policy.ID, "j-1")); err != nil || got != (store.ExchangedReplayed{}) {
		t.Fatalf("replay: %#v, %v", got, err)
	}
	unknown := uuid.Must(uuid.NewV7())
	if got, err := s.ExchangeCIToken(ctx, ciExchange(f, unknown, "j-2")); err != nil || got != (store.ExchangedPolicyRevoked{}) {
		t.Fatalf("unknown policy: %#v, %v", got, err)
	}
	for _, listed := range must[[]model.APIToken](t, "list")(s.ListTokens(ctx, f.owner)) {
		if listed.ID == token.ID {
			t.Fatal("CI tokens are listed under their policy, not their owner")
		}
	}
	if _, found, err := s.FindToken(ctx, token.ID); err != nil || !found {
		t.Fatalf("find: %v, %v", found, err)
	}
}

// Ported from repo/ci.rs: revoking_a_policy_revokes_its_tokens_and_stops_exchanges.
func TestRevokingAPolicyRevokesItsTokensAndStopsExchanges(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	f := newCIFixture(t, s)
	policy := must[store.CIPolicy](t, "create")(s.CreateCIPolicy(ctx, newCIPolicy(f, "deploy")))
	token := issued(t, must[store.Exchanged](t, "exchange")(s.ExchangeCIToken(ctx, ciExchange(f, policy.ID, "j-1"))))
	other := org(t, s, "b", "B")
	if revoked, err := s.RevokeCIPolicy(ctx, other, policy.ID); err != nil || revoked {
		t.Fatalf("another org's policy: %v, %v", revoked, err)
	}
	if revoked, err := s.RevokeCIPolicy(ctx, f.org, policy.ID); err != nil || !revoked {
		t.Fatalf("revoke: %v, %v", revoked, err)
	}
	if revoked, err := s.RevokeCIPolicy(ctx, f.org, policy.ID); err != nil || revoked {
		t.Fatalf("again: %v, %v", revoked, err)
	}
	read, found, err := s.FindToken(ctx, token.ID)
	if err != nil || !found {
		t.Fatalf("find: %v, %v", found, err)
	}
	if _, revoked := read.RevokedAt.Get(); !revoked {
		t.Fatal("the token is revoked with its policy")
	}
	if got, err := s.ExchangeCIToken(ctx, ciExchange(f, policy.ID, "j-2")); err != nil || got != (store.ExchangedPolicyRevoked{}) {
		t.Fatalf("exchange: %#v, %v", got, err)
	}
	if _, err := s.TestExec(ctx, "UPDATE ci_trust_policies SET role = 'admin' WHERE id = $1", policy.ID); err == nil {
		t.Fatal("a policy is only ever revoked")
	}
	if _, err := s.TestExec(ctx, "DELETE FROM ci_trust_policies WHERE id = $1", policy.ID); err == nil {
		t.Fatal("a policy is never deleted")
	}
}
