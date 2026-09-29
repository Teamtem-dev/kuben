package store_test

import (
	"testing"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/artifact"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store/pgtest"
)

func TestImageRegistriesAreTheFirstSegment(t *testing.T) {
	for given, want := range map[string]string{
		"ghcr.io/acme/web":                   "ghcr.io",
		"docker.io/library/nginx":            "docker.io",
		"registry.example.com:5000/acme/web": "registry.example.com:5000",
		"":                                   "",
	} {
		if got := store.ImageRegistry(given); got != want {
			t.Errorf("%q: %q", given, got)
		}
	}
}

// Runs pull with the organization's login for their registry, unless they
// are bound to their environment's own.
func TestRunsWithoutTheirOwnLoginPullWithTheOrganizations(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	f := newSecretFixture(t, s, "a")
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
	registry := ids.New[ids.OrgRegistry]()
	saved := must[store.OrgRegistrySaved](t, "add")(tn.CreateOrgRegistry(ctx, store.NewOrgRegistry{
		ID: registry, Name: "github", Preset: store.PresetGHCR, Server: "ghcr.io", Username: "bot",
		Password: sealedTag(5), CreatedBy: secretsBy,
	}))
	if _, ok := saved.(store.OrgRegistryStored); !ok {
		t.Fatalf("%#v", saved)
	}
	commit(t, tn)

	fromGhcr := f
	fromGhcr.release = release
	m := materializationOf(t, s, f.org, deploySecrets(t, s, fromGhcr))
	if r, ok := m.OrgRegistry.Get(); !ok || r.ID != registry || r.Username != "bot" {
		t.Fatalf("the organization's login: %+v", m.OrgRegistry)
	}
	if m := materializationOf(t, s, f.org, deploySecrets(t, s, f)); m.OrgRegistry.IsSome() {
		t.Fatal("a release without a registry pulls with none")
	}

	putSecret(t, s, f, "ghcr", store.SecretRegistry{Host: "ghcr.io"}, 1)
	if m := materializationOf(t, s, f.org, deploySecrets(t, s, fromGhcr)); m.OrgRegistry.IsSome() || len(m.Secrets) != 1 {
		t.Fatalf("the environment's own login wins: %+v %+v", m.OrgRegistry, m.Secrets)
	}
}

// materializationOf is the materialization of the run started.
func materializationOf(t *testing.T, s *store.Store, org ids.OrgID, started store.Started) store.Materialization {
	t.Helper()
	ctx := t.Context()
	tn := tenant(t, s, org)
	defer tn.Rollback(ctx) //nolint:errcheck // read only
	m, found, err := tn.Materialization(ctx, acceptedRun(t, started).Operation)
	if err != nil || !found {
		t.Fatalf("materialization: %v %v", found, err)
	}
	return m
}
