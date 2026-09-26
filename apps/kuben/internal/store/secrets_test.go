package store_test

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/artifact"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ops/target"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store/pgtest"
)

// Ported from repo/secrets.rs.

const secretsBy = "user:alice"

type secretFixture struct {
	org         ids.OrgID
	project     ids.ProjectID
	environment ids.EnvironmentID
	target      ids.TargetID
	release     ids.ReleaseID
}

func newSecretFixture(t *testing.T, s *store.Store, slug string) secretFixture {
	t.Helper()
	ctx := t.Context()
	o := org(t, s, slug, slug)
	tn := tenant(t, s, o)
	project := must[ids.ProjectID](t, "project")(tn.CreateProject(ctx, "shop", "Shop"))
	env := must[ids.EnvironmentID](t, "environment")(tn.CreateEnvironment(ctx, project, "prod", "Prod", false))
	cluster := must[ids.ClusterID](t, "cluster")(tn.CreateCluster(ctx, "eu-1"))
	placement := must[ids.PlacementID](t, "placement")(tn.CreatePlacement(ctx, project, env, cluster, slug+"-shop"))
	application := must[ids.ApplicationID](t, "app")(tn.CreateApplication(ctx, project, "web", "Web"))
	tgt := must[ids.TargetID](t, "target")(tn.CreateTarget(ctx, project, application, placement))
	release, _ := mustCreate(t)(tn.CreateRelease(ctx, project, store.PortableRelease{
		Application:     application,
		Artifacts:       map[string]artifact.Digest{"web": parseDigest(t, "sha256:"+strings.Repeat("0123456789abcdef", 4))},
		ProcessContract: map[string]any{},
		PortableConfig:  map[string]any{},
		RendererSchema:  1,
		CreatedBy:       secretsBy,
	}))
	commit(t, tn)
	return secretFixture{org: o, project: project, environment: env, target: tgt, release: release}
}

func sealedTag(tag byte) store.SealedBytes {
	return store.SealedBytes{
		Ciphertext: bytes.Repeat([]byte{tag}, 40),
		WrappedKey: bytes.Repeat([]byte{tag}, 60),
		KeyVersion: 1,
	}
}

// putSecret sets a new revision of name with key `url`.
func putSecret(t *testing.T, s *store.Store, f secretFixture, name string, kind store.SecretKind, tag byte) store.Reserved {
	t.Helper()
	tn := tenant(t, s, f.org)
	reservation := must[store.Reservation](t, "reserve")(
		tn.ReserveSecretRevision(t.Context(), f.project, f.environment, name, kind, secretsBy))
	reserved, ok := reservation.(store.ReservationReserved)
	if !ok {
		t.Fatalf("%s not reserved: %#v", name, reservation)
	}
	if err := tn.InsertSecretRevision(t.Context(), reserved.Reserved, []string{"url"}, sealedTag(tag), secretsBy, store.NewAudit{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	commit(t, tn)
	return reserved.Reserved
}

func referencing(names []string) map[string]any {
	env := make([]any, 0, len(names)+1)
	for _, n := range names {
		env = append(env, map[string]any{"name": strings.ToUpper(n), "fromSecret": map[string]any{"name": n, "key": "url"}})
	}
	env = append(env, map[string]any{"name": "PLAIN", "value": "x"})
	return map[string]any{"env": env}
}

// deploySecrets starts a deploy of a new configuration referencing names.
func deploySecrets(t *testing.T, s *store.Store, f secretFixture, names ...string) store.Started {
	t.Helper()
	ctx := t.Context()
	tn := tenant(t, s, f.org)
	revision := configRevision(t, tn, f.project, f.target, referencing(names))
	state, ok, err := tn.TargetState(ctx, f.target)
	if err != nil || !ok {
		t.Fatalf("target: %v %v", ok, err)
	}
	started := must[store.Started](t, "start")(tn.StartDeployment(ctx, store.StartDeployment{
		Project: f.project, Target: f.target, Release: f.release, ConfigRevision: revision.ID,
		ExpectedGeneration: state.DesiredGeneration, LifecycleUID: state.LifecycleUID,
		Reason: store.ReasonDeploy, RequestedBy: secretsBy, InputHash: []byte(revision.ID.String()),
	}, store.NewAudit{}, opt.None[store.IdempotencyKey]()))
	commit(t, tn)
	return started
}

func runOf(t *testing.T, started store.Started) ids.DeploymentRunID {
	t.Helper()
	return acceptedRun(t, started).Run
}

func TestSecretsAreRevisionsSeenOnlyByTheirOrganization(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	f := newSecretFixture(t, s, "a")
	other := newSecretFixture(t, s, "b")
	first := putSecret(t, s, f, "db", store.SecretOpaque{}, 1)
	second := putSecret(t, s, f, "db", store.SecretOpaque{}, 2)
	if first.Secret != second.Secret || first.Revision != 1 || second.Revision != 2 {
		t.Fatalf("%+v %+v", first, second)
	}

	tn := tenant(t, s, f.org)
	listed := must[[]store.SecretSummary](t, "list")(tn.Secrets(ctx, f.environment))
	if len(listed) != 1 || listed[0].Name != "db" || listed[0].CurrentRevision != 2 {
		t.Fatalf("%+v", listed)
	}
	if diff := cmp.Diff([]string{"url"}, listed[0].Keys); diff != "" {
		t.Fatal(diff)
	}
	revisions, ok, err := tn.SecretRevisions(ctx, f.environment, "db")
	if err != nil || !ok {
		t.Fatalf("revisions: %v %v", ok, err)
	}
	if len(revisions) != 2 || revisions[0].Revision != 2 || revisions[1].Revision != 1 {
		t.Fatalf("%+v", revisions)
	}
	if revisions[0].CreatedBy != secretsBy {
		t.Fatal(revisions[0].CreatedBy)
	}
	if _, ok, err := tn.SecretRevisions(ctx, f.environment, "nope"); ok || err != nil {
		t.Fatalf("nope: %v %v", ok, err)
	}
	for _, tc := range []struct {
		revision uint64
		want     store.Revoked
	}{{2, store.RevokedDone}, {2, store.RevokedAlready}, {9, store.RevokedNotFound}} {
		got := must[store.Revoked](t, "revoke")(tn.RevokeSecretRevision(ctx, f.environment, "db", tc.revision, secretsBy, store.NewAudit{}))
		if got != tc.want {
			t.Fatalf("revision %d: %v, want %v", tc.revision, got, tc.want)
		}
	}
	current, ok, err := tn.Secret(ctx, f.environment, "db")
	if err != nil || !ok || !current.Revoked {
		t.Fatalf("current: %+v %v %v", current, ok, err)
	}
	commit(t, tn)

	tn = tenant(t, s, other.org)
	if got := must[[]store.SecretSummary](t, "list")(tn.Secrets(ctx, f.environment)); len(got) != 0 {
		t.Fatalf("another organization sees %+v", got)
	}
	if _, ok, err := tn.Secret(ctx, f.environment, "db"); ok || err != nil {
		t.Fatalf("secret: %v %v", ok, err)
	}
	if got := must[store.Revoked](t, "revoke")(tn.RevokeSecretRevision(ctx, f.environment, "db", 1, secretsBy, store.NewAudit{})); got != store.RevokedNotFound {
		t.Fatal(got)
	}
	foreign := must[store.Reservation](t, "reserve")(
		tn.ReserveSecretRevision(ctx, other.project, f.environment, "db", store.SecretOpaque{}, secretsBy))
	if _, ok := foreign.(store.ReservationNoEnvironment); !ok {
		t.Fatalf("another organization's environment: %#v", foreign)
	}
}

func TestRunsBindTheCurrentRevisionAndRefuseARevokedOne(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	f := newSecretFixture(t, s, "a")
	db := putSecret(t, s, f, "db", store.SecretOpaque{}, 1)
	first := runOf(t, deploySecrets(t, s, f, "db", "legacy"))
	putSecret(t, s, f, "db", store.SecretOpaque{}, 2)
	second := runOf(t, deploySecrets(t, s, f, "db"))

	tn := tenant(t, s, f.org)
	binding := func(revision uint64) []store.SecretBinding {
		return []store.SecretBinding{{Name: "db", Secret: db.Secret, Revision: revision}}
	}
	if diff := cmp.Diff(binding(1), must[[]store.SecretBinding](t, "read")(tn.RunSecretBindings(ctx, first)), cmp.AllowUnexported(opt.Val[string]{}, ids.TargetID{})); diff != "" {
		t.Fatal(diff)
	}
	if diff := cmp.Diff(binding(2), must[[]store.SecretBinding](t, "read")(tn.RunSecretBindings(ctx, second)), cmp.AllowUnexported(opt.Val[string]{}, ids.TargetID{})); diff != "" {
		t.Fatal(diff)
	}
	bound := must[[]store.BoundSecret](t, "read")(tn.RunSecrets(ctx, second))
	if diff := cmp.Diff(sealedTag(2), bound[0].Sealed); diff != "" || bound[0].Revoked {
		t.Fatalf("%s revoked=%t", diff, bound[0].Revoked)
	}
	inUse := must[map[store.SecretRevisionKey]struct{}](t, "read")(tn.SecretRevisionsInUse(ctx, f.environment))
	if diff := cmp.Diff(map[store.SecretRevisionKey]struct{}{{Secret: db.Secret, Revision: 2}: {}}, inUse); diff != "" {
		t.Fatalf("the superseded run needs nothing: %s", diff)
	}
	users := must[[]store.SecretUser](t, "read")(tn.SecretUsers(ctx, f.environment, "db"))
	if diff := cmp.Diff([]store.SecretUser{{Target: f.target, Slug: "web"}}, users, cmp.AllowUnexported(ids.TargetID{})); diff != "" {
		t.Fatal(diff)
	}
	must[store.Revoked](t, "revoke")(tn.RevokeSecretRevision(ctx, f.environment, "db", 2, secretsBy, store.NewAudit{}))
	commit(t, tn)

	if started := deploySecrets(t, s, f, "db"); started != (store.StartedSecretRevoked{}) {
		t.Fatalf("%#v", started)
	}
	tn = tenant(t, s, f.org)
	state, _, err := tn.TargetState(ctx, f.target)
	if err != nil || state.DesiredGeneration != target.Generation(2) {
		t.Fatalf("nothing was accepted: %v %v", state.DesiredGeneration, err)
	}
	bound = must[[]store.BoundSecret](t, "read")(tn.RunSecrets(ctx, second))
	if !bound[0].Revoked {
		t.Fatal("the materializer sees the revocation")
	}
	commit(t, tn)
	runOf(t, deploySecrets(t, s, f, "legacy"))
}

func TestReferencedSecretsAreNotDeleted(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	f := newSecretFixture(t, s, "a")
	old := putSecret(t, s, f, "db", store.SecretOpaque{}, 1)
	runOf(t, deploySecrets(t, s, f, "db"))
	tn := tenant(t, s, f.org)
	deleted := must[store.SecretDeleted](t, "delete")(tn.DeleteSecret(ctx, f.environment, "db", store.NewAudit{}))
	if diff := cmp.Diff(store.SecretDeleted(store.SecretDeletedInUse{Apps: []string{"web"}}), deleted); diff != "" {
		t.Fatal(diff)
	}
	commit(t, tn)
	runOf(t, deploySecrets(t, s, f))
	tn = tenant(t, s, f.org)
	for _, want := range []store.SecretDeleted{store.SecretDeletedDone{}, store.SecretDeletedNotFound{}} {
		got := must[store.SecretDeleted](t, "delete")(tn.DeleteSecret(ctx, f.environment, "db", store.NewAudit{}))
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatal(diff)
		}
	}
	if got := must[[]store.SecretSummary](t, "list")(tn.Secrets(ctx, f.environment)); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
	commit(t, tn)
	fresh := putSecret(t, s, f, "db", store.SecretOpaque{}, 3)
	if fresh.Secret == old.Secret || fresh.Revision != 1 {
		t.Fatalf("a new secret of the same name: %+v", fresh)
	}
}

func TestKeyringsAreCheckedAndStaleSealsResealed(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	ours := store.KeyFingerprint{Version: 1, SHA256: [32]byte(bytes.Repeat([]byte{1}, 32))}
	theirs := store.KeyFingerprint{Version: 1, SHA256: [32]byte(bytes.Repeat([]byte{2}, 32))}
	check := func(fps ...store.KeyFingerprint) store.KeyringCheck {
		return must[store.KeyringCheck](t, "check")(s.CheckKeyring(ctx, fps))
	}
	if diff := cmp.Diff(store.KeyringCheck{}, check(ours)); diff != "" {
		t.Fatal(diff)
	}
	if diff := cmp.Diff([]uint32{1}, check(theirs).Mismatched); diff != "" {
		t.Fatal(diff)
	}
	rotated := check(store.KeyFingerprint{Version: 2, SHA256: theirs.SHA256})
	if len(rotated.Mismatched) != 0 || fmt.Sprint(rotated.Missing) != "[1]" {
		t.Fatalf("%+v", rotated)
	}

	f := newSecretFixture(t, s, "a")
	reserved := putSecret(t, s, f, "db", store.SecretOpaque{}, 1)
	tn := tenant(t, s, f.org)
	stale := must[[]store.SealedRevision](t, "read")(tn.StaleSeals(ctx, 2, 10))
	if len(stale) != 1 || stale[0].Secret != reserved.Secret || stale[0].Revision != reserved.Revision {
		t.Fatalf("%+v", stale)
	}
	resealed := stale[0].Sealed
	resealed.WrappedKey = bytes.Repeat([]byte{9}, 60)
	resealed.KeyVersion = 2
	if !must[bool](t, "reseal")(tn.Reseal(ctx, stale[0], resealed)) {
		t.Fatal("not resealed")
	}
	if must[bool](t, "reseal")(tn.Reseal(ctx, stale[0], resealed)) {
		t.Fatal("changed meanwhile")
	}
	tampered := resealed
	tampered.Ciphertext = make([]byte, 40)
	if _, err := tn.Reseal(ctx, stale[0], tampered); err == nil {
		t.Fatal("a reseal must keep the ciphertext")
	}
	if got := must[[]store.SealedRevision](t, "read")(tn.StaleSeals(ctx, 2, 10)); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
	commit(t, tn)
}

func TestRunsBindTheLoginOfTheirRegistry(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	f := newSecretFixture(t, s, "a")
	ghcr := store.SecretRegistry{Host: "ghcr.io"}
	login := putSecret(t, s, f, "ghcr", ghcr, 1)
	// The fixture's release has no source; give the target one from ghcr.io.
	tn := tenant(t, s, f.org)
	application, ok, err := tn.Application(ctx, f.project, "web")
	if err != nil || !ok {
		t.Fatalf("app: %v %v", ok, err)
	}
	release, _ := mustCreate(t)(tn.CreateRelease(ctx, f.project, store.PortableRelease{
		Application: application,
		Artifacts: map[string]artifact.Digest{
			"web": parseDigest(t, "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"),
		},
		ProcessContract: map[string]any{},
		PortableConfig:  map[string]any{},
		RendererSchema:  1,
		Source:          opt.Some[any](map[string]any{"image_repository": "ghcr.io/acme/web"}),
		CreatedBy:       secretsBy,
	}))
	for _, tc := range []struct {
		name string
		kind store.SecretKind
		want string
	}{{"ghcr", store.SecretOpaque{}, "another kind"}, {"other", ghcr, "already"}} {
		taken := must[store.Reservation](t, "reserve")(tn.ReserveSecretRevision(ctx, f.project, f.environment, tc.name, tc.kind, secretsBy))
		why, ok := taken.(store.ReservationTaken)
		if !ok || !strings.Contains(why.Why, tc.want) {
			t.Fatalf("%s: %#v", tc.name, taken)
		}
	}
	sealedLogin, ok, err := tn.RegistryLogin(ctx, f.environment, "ghcr.io")
	if err != nil || !ok || sealedLogin.Secret != login.Secret || sealedLogin.Revision != 1 {
		t.Fatalf("login: %+v %v %v", sealedLogin, ok, err)
	}
	if _, ok, err := tn.RegistryLogin(ctx, f.environment, "docker.io"); ok || err != nil {
		t.Fatalf("docker.io: %v %v", ok, err)
	}
	listed := must[[]store.SecretSummary](t, "list")(tn.Secrets(ctx, f.environment))
	if diff := cmp.Diff(store.SecretKind(ghcr), listed[0].Kind); diff != "" {
		t.Fatal(diff)
	}
	commit(t, tn)

	fromGhcr := f
	fromGhcr.release = release
	run := runOf(t, deploySecrets(t, s, fromGhcr))
	tn = tenant(t, s, f.org)
	bound := must[[]store.SecretBinding](t, "read")(tn.RunSecretBindings(ctx, run))
	want := []store.SecretBinding{{Name: "ghcr", Secret: login.Secret, Revision: 1, Registry: opt.Some("ghcr.io")}}
	if diff := cmp.Diff(want, bound, cmp.AllowUnexported(opt.Val[string]{})); diff != "" {
		t.Fatal(diff)
	}
	must[store.Revoked](t, "revoke")(tn.RevokeSecretRevision(ctx, f.environment, "ghcr", 1, secretsBy, store.NewAudit{}))
	if _, ok, err := tn.RegistryLogin(ctx, f.environment, "ghcr.io"); ok || err != nil {
		t.Fatalf("revoked login: %v %v", ok, err)
	}
	commit(t, tn)
	if started := deploySecrets(t, s, fromGhcr); started != (store.StartedSecretRevoked{}) {
		t.Fatalf("%#v", started)
	}
}
