package api_test

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/api/v1alpha1"
	"github.com/Teamtem-dev/kuben/internal/core/ids"
	kerr "github.com/Teamtem-dev/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/source"
	api "github.com/Teamtem-dev/kuben/internal/httpapi"
	"github.com/Teamtem-dev/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/internal/store"
)

// sourceBody is routes/apps/source.rs tests body().
func sourceBody(image string) gen.PutSource {
	return gen.PutSource{
		InstallationId:  7,
		Repository:      "Acme/Shop",
		Branch:          "main",
		Strategy:        gen.NewOptStrategyDto(gen.StrategyDtoDockerfile),
		Context:         gen.NewOptString("./apps/web/"),
		Dockerfile:      gen.NewOptNilString(""),
		ImageRepository: image,
	}
}

// routes/apps/source.rs bindings_are_validated_and_normalized.
func TestBindingsAreValidatedAndNormalized(t *testing.T) {
	body := sourceBody("registry.example.com:5000/acme/shop")
	b, err := api.NewBinding(&body)
	if err != nil {
		t.Fatal(err)
	}
	if b.Repository.String() != "acme/shop" {
		t.Errorf("repository %s", b.Repository)
	}
	if b.Recipe.Context.String() != "apps/web" {
		t.Errorf("context %s", b.Recipe.Context)
	}
	if b.Recipe.Dockerfile.IsSome() {
		t.Errorf("an empty Dockerfile path is the default: %v", b.Recipe.Dockerfile)
	}
	if b.Recipe.Strategy != source.Dockerfile {
		t.Errorf("strategy %s", b.Recipe.Strategy)
	}
	if b.ImageRepository != "registry.example.com:5000/acme/shop" {
		t.Errorf("image repository %s", b.ImageRepository)
	}
	if b.InstallationID != 7 || b.PullRequest.IsSome() {
		t.Errorf("installation %d, pull request %v", b.InstallationID, b.PullRequest)
	}
	hub := sourceBody("acme/shop")
	if b, err := api.NewBinding(&hub); err != nil || b.ImageRepository != "docker.io/acme/shop" {
		t.Errorf("Docker Hub: %v %v", b.ImageRepository, err)
	}
	// Nothing given for the strategy, context and Dockerfile: auto, the
	// root and the default.
	bare := gen.PutSource{InstallationId: 1, Repository: "a/b", Branch: "main", ImageRepository: "ghcr.io/a/b"}
	b, err = api.NewBinding(&bare)
	if err != nil {
		t.Fatal(err)
	}
	if b.Recipe.Strategy != source.Auto || !b.Recipe.Context.IsRoot() || b.Recipe.Dockerfile.IsSome() {
		t.Errorf("defaults: %+v", b.Recipe)
	}
}

// routes/apps/source.rs the_spec_mirrors_the_binding.
func TestTheSpecMirrorsTheBinding(t *testing.T) {
	git := api.GitSource(sourceBody("ghcr.io/acme/shop"))
	if git.Repo != "acme/shop" || git.Path != "./apps/web/" {
		t.Errorf("repo %s, path %s", git.Repo, git.Path)
	}
	if git.Build.Strategy != v1alpha1.BuildStrategyDockerfile || git.Build.Dockerfile != nil {
		t.Errorf("build %+v", git.Build)
	}
}

// routes/apps/source.rs tags_digests_and_escapes_are_refused.
func TestTagsDigestsAndEscapesAreRefused(t *testing.T) {
	refused := func(what string, body gen.PutSource) {
		t.Helper()
		_, err := api.NewBinding(&body)
		if kerr.CodeOf(err) != kerr.Validation {
			t.Errorf("%s: %v", what, err)
		}
	}
	for _, image := range []string{"ghcr.io/acme/shop:v1", "ghcr.io/acme/shop@sha256:00", "Upper/Case"} {
		refused(image, sourceBody(image))
	}
	escape := sourceBody("ghcr.io/acme/shop")
	escape.Context = gen.NewOptString("../secrets")
	refused("an escaping context", escape)
	branch := sourceBody("ghcr.io/acme/shop")
	branch.Branch = "a..b"
	refused("a bad branch", branch)
}

// The texts of the refusals are Rust's.
func TestImageRepositoryRefusalsSayWhy(t *testing.T) {
	cases := []struct{ image, want string }{
		{"ghcr.io/acme/shop:v1", "validation failed: `ghcr.io/acme/shop:v1` must name a repository without tag or digest; builds tag and pin it"},
		{"ghcr.io/acme/shop@sha256:00", "validation failed: `ghcr.io/acme/shop@sha256:00` must name a repository without tag or digest; builds tag and pin it"},
		{"Upper/Case", "validation failed: `Upper/Case` is not an image reference"},
	}
	for _, c := range cases {
		if _, err := api.ImageRepository(c.image); err == nil || err.Error() != c.want {
			t.Errorf("%s: %v", c.image, err)
		}
	}
	// A port is not a tag: only the last path part is looked at.
	if got, err := api.ImageRepository("localhost:5000/shop"); err != nil || got != "localhost:5000/shop" {
		t.Errorf("a registry port: %s %v", got, err)
	}
}

// SourceDto::new: absent values are null, the strategy is lowercase.
func TestSourcesShowTheirBinding(t *testing.T) {
	repo, err := source.ParseRepoName("acme/shop")
	if err != nil {
		t.Fatal(err)
	}
	branch, err := source.ParseBranchName("main")
	if err != nil {
		t.Fatal(err)
	}
	b := store.SourceBinding{InstallationID: 9, Repository: repo, Branch: branch, ImageRepository: "ghcr.io/acme/shop"}
	dto := api.SourceDtoOf(b, opt.None[ids.OperationID]())
	raw, err := json.Marshal(&dto)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"installationId": float64(9), "repository": "acme/shop", "branch": "main", "strategy": "auto",
		"context": "", "dockerfile": nil, "imageRepository": "ghcr.io/acme/shop", "head": nil, "syncOperation": nil,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("source (-want +got):\n%s", diff)
	}
	sync := ids.New[ids.Operation]()
	if dto := api.SourceDtoOf(b, opt.Some(sync)); dto.SyncOperation.Or("") != sync.String() {
		t.Errorf("sync operation %v", dto.SyncOperation)
	}
}
