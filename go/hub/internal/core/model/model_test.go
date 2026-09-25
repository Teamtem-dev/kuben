package model_test

import (
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/model"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/perm"
)

const (
	uuidA = "0192f3a1-0000-7000-8000-000000000001"
	uuidB = "0192f3a1-0000-7000-8000-000000000002"
)

func TestTokensExpireAndRevoke(t *testing.T) {
	token := model.APIToken{
		ID:         ids.New[ids.Token](),
		OrgID:      ids.New[ids.Org](),
		Name:       "ci",
		Prefix:     "kbn_pat_x",
		SecretHash: make([]byte, 32),
		Scope:      model.TokenScope{Role: perm.Developer},
		ExpiresAt:  opt.Some[int64](1_000),
	}
	never := token
	never.ExpiresAt = opt.None[int64]()
	revoked := never
	revoked.RevokedAt = opt.Some[int64](5)
	for _, tc := range []struct {
		name  string
		token model.APIToken
		now   int64
		want  bool
	}{
		{"before expiry", token, 999, true},
		{"at expiry", token, 1_000, false},
		{"no expiry", never, math.MaxInt64, true},
		{"revoked", revoked, 0, false},
	} {
		if got := tc.token.IsUsableAt(tc.now); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSessionsExpireAndRevoke(t *testing.T) {
	s := model.Session{UserID: ids.New[ids.User](), ExpiresAt: 1_000}
	revoked := s
	revoked.RevokedAt = opt.Some[int64](5)
	for _, tc := range []struct {
		name    string
		session model.Session
		now     int64
		want    bool
	}{
		{"before expiry", s, 999, true},
		{"at expiry", s, 1_000, false},
		{"revoked", revoked, 0, false},
	} {
		if got := tc.session.IsValidAt(tc.now); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestTokenScopeJSONIsCompact(t *testing.T) {
	got := marshal(t, model.TokenScope{Role: perm.Viewer})
	if got != `{"role":"viewer"}` {
		t.Fatalf("got %s", got)
	}
	full := model.TokenScope{
		Role:        perm.Admin,
		Project:     opt.Some(uuid.MustParse(uuidA)),
		Environment: opt.Some(uuid.MustParse(uuidB)),
	}
	want := `{"role":"admin","project":"` + uuidA + `","environment":"` + uuidB + `"}`
	if got = marshal(t, full); got != want {
		t.Fatalf("got %s", got)
	}
	var back model.TokenScope
	if err := json.Unmarshal([]byte(want), &back); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(full, back, cmp.AllowUnexported(opt.Val[uuid.UUID]{})); diff != "" {
		t.Fatal(diff)
	}
}

func TestKindsParseAndPinTheirStrings(t *testing.T) {
	subjects := map[model.SubjectKind]string{
		model.SubjectUser: "user", model.SubjectTeam: "team", model.SubjectToken: "token",
	}
	for kind, s := range subjects {
		if got, err := model.ParseSubjectKind(s); err != nil || got != kind || kind.String() != s {
			t.Errorf("%s: got %s, %v", s, got, err)
		}
		if got := marshal(t, kind); got != `"`+s+`"` {
			t.Errorf("json of %s: %s", s, got)
		}
	}
	scopes := map[model.ScopeKind]string{
		model.ScopeOrg: "org", model.ScopeProject: "project",
		model.ScopeEnvironment: "environment", model.ScopeApp: "app",
	}
	for kind, s := range scopes {
		if got, err := model.ParseScopeKind(s); err != nil || got != kind || kind.String() != s {
			t.Errorf("%s: got %s, %v", s, got, err)
		}
		if got := marshal(t, kind); got != `"`+s+`"` {
			t.Errorf("json of %s: %s", s, got)
		}
	}
	if _, err := model.ParseSubjectKind("group"); !errors.Is(err, kerr.ErrValidation) {
		t.Errorf("group: %v", err)
	}
	if _, err := model.ParseScopeKind("cluster"); !errors.Is(err, kerr.ErrValidation) {
		t.Errorf("cluster: %v", err)
	}
	var sk model.SubjectKind
	if err := json.Unmarshal([]byte(`"User"`), &sk); err == nil {
		t.Error("kinds are lowercase on the wire")
	}
	var sc model.ScopeKind
	if err := json.Unmarshal([]byte(`"namespace"`), &sc); err == nil {
		t.Error("namespace is not a scope kind")
	}
}

func TestSerializedModelsKeepTheirJSON(t *testing.T) {
	org := ids.From[ids.Org](uuid.MustParse(uuidA))
	for _, tc := range []struct {
		name string
		v    any
		want string
	}{
		{
			"organization",
			model.Organization{ID: org, Slug: "acme", Name: "Acme", CreatedAt: 7},
			`{"id":"` + uuidA + `","slug":"acme","name":"Acme","created_at":7}`,
		},
		{
			"user",
			model.User{ID: ids.From[ids.User](uuid.MustParse(uuidB)), Email: "a@b.c", IsActive: true, CreatedAt: 1},
			`{"id":"` + uuidB + `","email":"a@b.c","display_name":null,"is_active":true,"must_change_password":false,"created_at":1}`,
		},
		{
			"audit event, empty",
			model.AuditEvent{Seq: 3, ID: ids.From[ids.Audit](uuid.MustParse(uuidB)), ActorKind: "system", Action: "app.deploy", Outcome: "ok", CreatedAt: 9},
			`{"seq":3,"id":"` + uuidB + `","org_id":null,"actor_kind":"system","actor_id":null,"action":"app.deploy",` +
				`"target_kind":null,"target_ref":null,"outcome":"ok","ip":null,"request_id":null,"data":null,"created_at":9}`,
		},
		{
			"audit event, full",
			model.AuditEvent{
				Seq: 4, ID: ids.From[ids.Audit](uuid.MustParse(uuidB)), OrgID: opt.Some(org), ActorKind: "user",
				ActorID: opt.Some("u1"), Action: "app.deploy", TargetKind: opt.Some("app"), TargetRef: opt.Some("web"),
				Outcome: "denied", IP: opt.Some("10.0.0.1"), RequestID: opt.Some("r1"),
				Data: opt.Some[any](map[string]any{"n": 1.0}), CreatedAt: 9,
			},
			`{"seq":4,"id":"` + uuidB + `","org_id":"` + uuidA + `","actor_kind":"user","actor_id":"u1","action":"app.deploy",` +
				`"target_kind":"app","target_ref":"web","outcome":"denied","ip":"10.0.0.1","request_id":"r1","data":{"n":1},"created_at":9}`,
		},
		{
			"app release",
			model.AppRelease{
				ID: "r1", Revision: 2, Namespace: "ns", App: "web", Image: opt.Some("nginx@sha256:x"),
				Spec: map[string]any{"replicas": 2.0}, Reason: "deploy", CreatedAt: 5,
			},
			`{"id":"r1","revision":2,"namespace":"ns","app":"web","image":"nginx@sha256:x","spec":{"replicas":2},` +
				`"reason":"deploy","actor_id":null,"note":null,"created_at":5}`,
		},
	} {
		if got := marshal(t, tc.v); got != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.name, got, tc.want)
		}
	}
}

func TestAuditEventsRoundTrip(t *testing.T) {
	in := model.AuditEvent{
		Seq: 4, ID: ids.From[ids.Audit](uuid.MustParse(uuidB)), ActorKind: "user", ActorID: opt.Some("u1"),
		Action: "x", Outcome: "ok", Data: opt.Some[any](map[string]any{"k": "v"}), CreatedAt: 9,
	}
	var out model.AuditEvent
	if err := json.Unmarshal([]byte(marshal(t, in)), &out); err != nil {
		t.Fatal(err)
	}
	if marshal(t, out) != marshal(t, in) || !out.ActorID.IsSome() || out.OrgID.IsSome() {
		t.Fatalf("got %+v", out)
	}
}

func TestSecretsStayOutOfJSON(t *testing.T) {
	creds := model.UserCredentials{PasswordHash: opt.Some("$argon2id$secret")}
	if got := marshal(t, creds); got != `{}` {
		t.Errorf("credentials: %s", got)
	}
}

func marshal(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
