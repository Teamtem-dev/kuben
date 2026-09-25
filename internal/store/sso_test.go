package store_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/model"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/internal/core/sso"
	"github.com/Teamtem-dev/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/internal/store/pgtest"
)

const ssoProvider = "https://idp.example.com"

func ssoPerson(subject, email string, role perm.Role) sso.Person {
	return sso.Person{Subject: subject, Email: email, Name: opt.Some("Carol"), Role: role}
}

func pendingSSO() store.PendingSSO {
	return store.PendingSSO{
		Nonce:    string(bytes.Repeat([]byte("n"), 32)),
		Verifier: string(bytes.Repeat([]byte("v"), 43)),
		ReturnTo: "/projects",
	}
}

func signedIn(t *testing.T, s *store.Store, o ids.OrgID, person sso.Person) model.User {
	t.Helper()
	out, err := s.SSOSignIn(t.Context(), o, ssoProvider, person)
	if err != nil {
		t.Fatalf("sign in: %v", err)
	}
	in, ok := out.(store.SSOSignedIn)
	if !ok {
		t.Fatalf("refused: %#v", out)
	}
	return in.User
}

func hasIdentity(t *testing.T, s *store.Store, user ids.UserID) bool {
	t.Helper()
	linked, err := s.HasIdentity(t.Context(), user)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	return linked
}

func TestAStartedSignInIsTakenOnceAndExpires(t *testing.T) {
	base := pgtest.Store(t)
	ctx := t.Context()
	now := time.Now().UnixMilli()
	s := base.WithClock(clock.Fixed(now))
	state := bytes.Repeat([]byte{7}, 32)
	if err := s.BeginSSO(ctx, state, pendingSSO(), now+600_000); err != nil {
		t.Fatalf("begin: %v", err)
	}
	got, found, err := s.TakeSSO(ctx, state)
	if err != nil || !found {
		t.Fatalf("take: %v %v", found, err)
	}
	if diff := cmp.Diff(pendingSSO(), got); diff != "" {
		t.Errorf("taken (-want +got):\n%s", diff)
	}
	if _, found, err := s.TakeSSO(ctx, state); err != nil || found {
		t.Errorf("once: %v %v", found, err)
	}
	expired := bytes.Repeat([]byte{8}, 32)
	if err := s.BeginSSO(ctx, expired, pendingSSO(), now+1); err != nil {
		t.Fatalf("begin: %v", err)
	}
	// Five milliseconds later, as the Rust test slept.
	if _, found, err := base.WithClock(clock.Fixed(now+5)).TakeSSO(ctx, expired); err != nil || found {
		t.Errorf("expired: %v %v", found, err)
	}
	if err := s.BeginSSO(ctx, []byte{1, 1, 1}, pendingSSO(), now+1_000); err == nil {
		t.Error("a short state hash was accepted")
	}
}

func TestSignInsCreateLinkAndResyncTheRole(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	o := org(t, s, "acme", "ACME")
	carol := signedIn(t, s, o, ssoPerson("s-1", "carol@example.com", perm.Admin))
	if !hasIdentity(t, s, carol.ID) {
		t.Error("carol is not linked")
	}
	members := must[[]model.Member](t, "members")(s.ListMembers(ctx, o))
	if len(members) != 1 || members[0].Role != perm.Admin {
		t.Fatalf("members: %+v", members)
	}

	again := signedIn(t, s, o, ssoPerson("s-1", "carol@example.com", perm.Viewer))
	if again.ID != carol.ID {
		t.Error("the same subject is another user")
	}
	members = must[[]model.Member](t, "members")(s.ListMembers(ctx, o))
	if len(members) != 1 || members[0].Role != perm.Viewer {
		t.Fatalf("demoted by the provider: %+v", members)
	}

	local := must[model.User](t, "user")(s.CreateUser(ctx, "dave@example.com", opt.None[string](), opt.Some("phc")))
	if hasIdentity(t, s, local.ID) {
		t.Error("a local account is linked")
	}
	linked := signedIn(t, s, o, ssoPerson("s-2", "dave@example.com", perm.Developer))
	if linked.ID != local.ID {
		t.Error("not linked by the verified email")
	}
	if !hasIdentity(t, s, local.ID) {
		t.Error("dave is not linked")
	}
}

func TestDeactivatedAccountsStayOut(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	o := org(t, s, "acme", "ACME")
	user := must[model.User](t, "user")(s.CreateUser(ctx, "erin@example.com", opt.None[string](), opt.None[string]()))
	if _, err := s.TestExec(ctx, "UPDATE users SET is_active = FALSE WHERE id = $1", user.ID.String()); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	out, err := s.SSOSignIn(ctx, o, ssoProvider, ssoPerson("s-3", "erin@example.com", perm.Admin))
	if err != nil {
		t.Fatalf("sign in: %v", err)
	}
	if _, ok := out.(store.SSOInactive); !ok {
		t.Errorf("outcome: %#v", out)
	}
	if members := must[[]model.Member](t, "members")(s.ListMembers(ctx, o)); len(members) != 0 {
		t.Errorf("nothing changed: %+v", members)
	}
	if hasIdentity(t, s, user.ID) {
		t.Error("rolled back: erin is linked")
	}
}
