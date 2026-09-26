package store_test

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/model"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store/pgtest"
)

// Ported from tests/matrix.rs: one pass over every identity repository and
// the release history.

func nowMs() int64 { return time.Now().UnixMilli() }

func TestPostgresRoundtrip(t *testing.T) {
	s := pgtest.Store(t)
	if s.Backend() != "postgres" {
		t.Fatalf("backend: %s", s.Backend())
	}
	o, user := roundtripUsersAndOrgs(t, s)
	roundtripSessions(t, s, user)
	roundtripAudit(t, s, o, user)
	roundtripTokens(t, s, o, user)
	roundtripMembers(t, s, o, user)
	roundtripReleases(t, s, o, user)
	roundtripAuditPages(t, s, o)
	roundtripThrottle(t, s)
	if err := s.Ping(t.Context()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	s.Close()
}

// orgs + users + bindings
func roundtripUsersAndOrgs(t *testing.T, s *store.Store) (model.Organization, model.User) {
	t.Helper()
	ctx := t.Context()
	o := must[model.Organization](t, "create org")(s.CreateOrg(ctx, "acme", "ACME Inc"))
	if found, ok, err := s.FindOrgBySlug(ctx, "acme"); err != nil || !ok || found.ID != o.ID {
		t.Fatalf("find org: %+v, %v, %v", found, ok, err)
	}
	user := must[model.User](t, "user")(s.CreateUser(ctx, "Alice@Example.com", opt.Some("Alice"), opt.Some("$argon2id$fake")))
	if user.Email != "alice@example.com" {
		t.Fatalf("emails are normalized: %s", user.Email)
	}
	if n := must[int64](t, "count")(s.CountUsers(ctx)); n != 1 {
		t.Fatalf("count: %d", n)
	}
	creds, ok, err := s.FindUserByEmail(ctx, "ALICE@example.com")
	if err != nil || !ok {
		t.Fatalf("find by email: %v, %v", ok, err)
	}
	if h, _ := creds.PasswordHash.Get(); h != "$argon2id$fake" {
		t.Fatalf("password hash: %+v", creds.PasswordHash)
	}
	if _, ok, err := s.FindUserByEmail(ctx, "nobody@example.com"); err != nil || ok {
		t.Fatalf("nobody: %v, %v", ok, err)
	}
	if err := s.AddMembership(ctx, o.ID, user.ID); err != nil {
		t.Fatalf("membership: %v", err)
	}
	if err := s.BindOrgRole(ctx, o.ID, user.ID, perm.Developer); err != nil {
		t.Fatalf("bind: %v", err)
	}
	bindings := must[[]model.RoleBinding](t, "bindings")(s.BindingsForUser(ctx, user.ID))
	if len(bindings) != 1 || bindings[0].Role != perm.Developer || bindings[0].OrgID != o.ID {
		t.Fatalf("bindings: %+v", bindings)
	}
	return o, user
}

// sessions
func roundtripSessions(t *testing.T, s *store.Store, user model.User) {
	t.Helper()
	ctx := t.Context()
	idHash := make32(7)
	if err := s.CreateSession(ctx, store.NewSession{
		IDHash:    idHash,
		UserID:    user.ID,
		ExpiresAt: nowMs() + 60_000,
		IP:        opt.Some("127.0.0.1"),
	}); err != nil {
		t.Fatalf("session: %v", err)
	}
	sess, ok, err := s.FindSession(ctx, idHash)
	if err != nil || !ok || sess.UserID != user.ID || !sess.IsValidAt(nowMs()) {
		t.Fatalf("session: %+v, %v, %v", sess, ok, err)
	}
	if err := s.RevokeSession(ctx, idHash); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	sess, ok, err = s.FindSession(ctx, idHash)
	if err != nil || !ok || sess.IsValidAt(nowMs()) {
		t.Fatalf("revoked session: %+v, %v, %v", sess, ok, err)
	}
	if _, ok, err := s.FindSession(ctx, make32(1)); err != nil || ok {
		t.Fatalf("unknown session: %v, %v", ok, err)
	}
}

func make32(b byte) []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = b
	}
	return out
}

// audit
func roundtripAudit(t *testing.T, s *store.Store, o model.Organization, user model.User) {
	t.Helper()
	ctx := t.Context()
	id := must[ids.AuditID](t, "audit")(s.AppendAudit(ctx, store.NewAudit{
		OrgID:     opt.Some(o.ID),
		ActorKind: "user",
		ActorID:   opt.Some(user.ID.String()),
		Action:    "login",
		Outcome:   "success",
		Data:      opt.Some[any](map[string]any{"method": "password"}),
	}))
	recent := must[[]model.AuditEvent](t, "recent")(s.RecentAudit(ctx, 10))
	if len(recent) != 1 || recent[0].ID != id {
		t.Fatalf("recent: %+v", recent)
	}
	data, _ := recent[0].Data.Get()
	if m, ok := data.(map[string]any); !ok || m["method"] != "password" {
		t.Fatalf("data: %#v", data)
	}
}

// api tokens (scenario 3)
func roundtripTokens(t *testing.T, s *store.Store, o model.Organization, user model.User) {
	t.Helper()
	ctx := t.Context()
	token := must[model.APIToken](t, "token")(s.CreateToken(ctx, store.NewToken{
		ID:         ids.New[ids.Token](),
		OrgID:      o.ID,
		Owner:      user.ID,
		Name:       "ci",
		Prefix:     "kbn_pat_test",
		SecretHash: make32(9),
		Scope:      model.TokenScope{Role: perm.Developer},
	}))
	found, ok, err := s.FindToken(ctx, token.ID)
	if err != nil || !ok || found.Scope.Role != perm.Developer {
		t.Fatalf("found: %+v, %v, %v", found, ok, err)
	}
	if diff := cmp.Diff(make32(9), found.SecretHash); diff != "" {
		t.Fatalf("secret hash (-want +got):\n%s", diff)
	}
	if list := must[[]model.APIToken](t, "list")(s.ListTokens(ctx, user.ID)); len(list) != 1 {
		t.Fatalf("list: %+v", list)
	}
	if err := s.TouchToken(ctx, token.ID); err != nil {
		t.Fatalf("touch: %v", err)
	}
	if revoked := must[bool](t, "revoke")(s.RevokeToken(ctx, token.ID, user.ID)); !revoked {
		t.Fatal("revoke")
	}
	if revoked := must[bool](t, "revoke twice")(s.RevokeToken(ctx, token.ID, user.ID)); revoked {
		t.Fatal("revoke twice")
	}
	revoked, ok, err := s.FindToken(ctx, token.ID)
	if err != nil || !ok || revoked.IsUsableAt(nowMs()) {
		t.Fatalf("revoked token: %+v, %v, %v", revoked, ok, err)
	}
}

// members (scenario 4)
func roundtripMembers(t *testing.T, s *store.Store, o model.Organization, user model.User) {
	t.Helper()
	ctx := t.Context()
	members := must[[]model.Member](t, "members")(s.ListMembers(ctx, o.ID))
	if len(members) != 1 || members[0].Role != perm.Developer {
		t.Fatalf("members: %+v", members)
	}
	if err := s.SetOrgRole(ctx, o.ID, user.ID, perm.Owner); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if n := must[int64](t, "owners")(s.CountOwners(ctx, o.ID)); n != 1 {
		t.Fatalf("owners: %d", n)
	}
	invited := must[model.User](t, "invite")(s.CreateInvitedUser(ctx, "Bob@Example.com", opt.None[string](), opt.Some("$argon2id$tmp")))
	if !invited.MustChangePassword {
		t.Fatal("an invited user must change the password")
	}
	if err := s.BindOrgRole(ctx, o.ID, invited.ID, perm.Viewer); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if members := must[[]model.Member](t, "members")(s.ListMembers(ctx, o.ID)); len(members) != 2 {
		t.Fatalf("members: %+v", members)
	}
	if err := s.SetPasswordHash(ctx, invited.ID, "$argon2id$new"); err != nil {
		t.Fatalf("password: %v", err)
	}
	bob, ok, err := s.FindUserByID(ctx, invited.ID)
	if err != nil || !ok || bob.MustChangePassword {
		t.Fatalf("a new password clears the flag: %+v, %v, %v", bob, ok, err)
	}
	if err := s.RemoveMember(ctx, o.ID, invited.ID); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if members := must[[]model.Member](t, "members")(s.ListMembers(ctx, o.ID)); len(members) != 1 {
		t.Fatalf("members: %+v", members)
	}
}

// releases (scenario 5)
func roundtripReleases(t *testing.T, s *store.Store, o model.Organization, user model.User) {
	t.Helper()
	ctx := t.Context()
	for i, image := range []string{"nginx:1.27", "nginx:1.28"} {
		r := must[model.AppRelease](t, "release")(s.RecordRelease(ctx, store.NewRelease{
			OrgID:     opt.Some(o.ID),
			Namespace: "kb-shop-prod",
			App:       "api",
			Image:     opt.Some(image),
			Spec:      map[string]any{"source": map[string]any{"image": image}},
			Reason:    "deploy",
			ActorID:   opt.Some(user.ID.String()),
		}))
		if r.Revision != int64(i)+1 {
			t.Fatalf("revision: %d", r.Revision)
		}
	}
	releases := must[[]model.AppRelease](t, "releases")(s.ListReleases(ctx, "kb-shop-prod", "api", 10))
	var revisions []int64
	for _, r := range releases {
		revisions = append(revisions, r.Revision)
	}
	if diff := cmp.Diff([]int64{2, 1}, revisions); diff != "" {
		t.Fatalf("revisions (-want +got):\n%s", diff)
	}
	first, ok, err := s.FindRelease(ctx, "kb-shop-prod", "api", 1)
	if err != nil || !ok {
		t.Fatalf("revision 1: %v, %v", ok, err)
	}
	if image, _ := first.Image.Get(); image != "nginx:1.27" {
		t.Fatalf("image: %+v", first.Image)
	}
	spec, ok := first.Spec.(map[string]any)
	if source, _ := spec["source"].(map[string]any); !ok || source["image"] != "nginx:1.27" {
		t.Fatalf("spec: %#v", first.Spec)
	}
}

// audit pagination (scenario 2)
func roundtripAuditPages(t *testing.T, s *store.Store, o model.Organization) {
	t.Helper()
	ctx := t.Context()
	for _, action := range []string{"createApp", "updateApp"} {
		must[ids.AuditID](t, "audit")(s.AppendAudit(ctx, store.NewAudit{
			OrgID: opt.Some(o.ID), ActorKind: "user", Action: action, Outcome: "success",
		}))
	}
	page := must[[]model.AuditEvent](t, "page")(s.ListAudit(ctx, o.ID, opt.None[int64](), 1))
	if len(page) != 1 || page[0].Action != "updateApp" {
		t.Fatalf("page: %+v", page)
	}
	older := must[[]model.AuditEvent](t, "older")(s.ListAudit(ctx, o.ID, opt.Some(page[0].Seq), 10))
	sawCreate := false
	for _, e := range older {
		if org, _ := e.OrgID.Get(); e.Seq >= page[0].Seq || org != o.ID {
			t.Fatalf("older page: %+v", e)
		}
		sawCreate = sawCreate || e.Action == "createApp"
	}
	if !sawCreate {
		t.Fatalf("older page misses createApp: %+v", older)
	}
}

// login throttle windows (scenario 1)
func roundtripThrottle(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := t.Context()
	now := nowMs()
	const window = 60_000
	for i := range int64(2) {
		if err := s.ThrottleRecordFailure(ctx, "bucket-a", now+i, now+i-window); err != nil {
			t.Fatalf("failure: %v", err)
		}
	}
	w, ok, err := s.ThrottleWindow(ctx, "bucket-a")
	if err != nil || !ok || w != (store.ThrottleWindow{Failures: 2, StartedAt: now}) {
		t.Fatalf("window: %+v, %v, %v", w, ok, err)
	}
	later := now + 2*window
	if err := s.ThrottleRecordFailure(ctx, "bucket-a", later, later-window); err != nil {
		t.Fatalf("failure: %v", err)
	}
	w, ok, err = s.ThrottleWindow(ctx, "bucket-a")
	if err != nil || !ok || w != (store.ThrottleWindow{Failures: 1, StartedAt: later}) {
		t.Fatalf("an expired window restarts: %+v, %v, %v", w, ok, err)
	}
	if err := s.ThrottleRecordFailure(ctx, "bucket-b", now, now-window); err != nil {
		t.Fatalf("failure: %v", err)
	}
	if n := must[uint64](t, "purge")(s.ThrottlePurge(ctx, now)); n != 1 {
		t.Fatalf("only the window of bucket-b has expired: %d", n)
	}
	if err := s.ThrottleClear(ctx, "bucket-a"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, ok, err := s.ThrottleWindow(ctx, "bucket-a"); err != nil || ok {
		t.Fatalf("cleared window: %v, %v", ok, err)
	}
}
