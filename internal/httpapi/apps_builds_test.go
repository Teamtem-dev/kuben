package api_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/internal/core/artifact"
	"github.com/Teamtem-dev/kuben/internal/core/ids"
	kerr "github.com/Teamtem-dev/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/internal/core/ops/build"
	"github.com/Teamtem-dev/kuben/internal/core/ops/target"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/source"
	api "github.com/Teamtem-dev/kuben/internal/httpapi"
	"github.com/Teamtem-dev/kuben/internal/store"
)

const buildDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// checked is a value that must have been read without error.
func checked[T any](t *testing.T) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

func uuidOf(n byte) uuid.UUID {
	var u uuid.UUID
	u[15] = n
	return u
}

// routes/apps/builds.rs builds_show_their_image_and_outcome.
func TestBuildsShowTheirImageAndOutcome(t *testing.T) {
	digest := checked[artifact.Digest](t)(artifact.ParseDigest(buildDigest))
	attempt := store.BuildAttempt{
		ID:                  ids.From[ids.BuildAttempt](uuidOf(1)),
		Org:                 ids.From[ids.Org](uuidOf(2)),
		Project:             ids.From[ids.Project](uuidOf(3)),
		Application:         ids.From[ids.Application](uuidOf(4)),
		Target:              ids.From[ids.Target](uuidOf(5)),
		Binding:             ids.From[ids.SourceBinding](uuidOf(6)),
		AttemptNo:           1,
		Commit:              checked[source.CommitSha](t)(source.ParseCommitSha("0123456789abcdef0123456789abcdef01234567")),
		SourceEpoch:         target.SourceEpoch(1),
		LifecycleUID:        uuidOf(7),
		Repository:          checked[source.RepoName](t)(source.ParseRepoName("acme/shop")),
		ImageRepository:     "ghcr.io/acme/shop",
		InstallationID:      9,
		Branch:              checked[source.BranchName](t)(source.ParseBranchName("main")),
		Phase:               build.Succeeded,
		ReportedDigest:      opt.Some(digest),
		Digest:              opt.Some(digest),
		Release:             opt.Some(ids.From[ids.Release](uuidOf(8))),
		DeployDecision:      opt.Some("StaleSource"),
		Operation:           ids.From[ids.Operation](uuidOf(10)),
		CreatedAt:           1,
		StartedAt:           opt.Some[int64](2),
		FinishedAt:          opt.Some[int64](3),
		BuildConfigRevision: 0,
	}
	dto := api.BuildDtoOf(attempt)
	if image, ok := dto.Image.Get(); !ok || image != "ghcr.io/acme/shop@"+buildDigest {
		t.Errorf("image = %v", dto.Image)
	}
	if dto.Phase != "succeeded" || dto.Strategy != "auto" {
		t.Errorf("phase %s, strategy %s", dto.Phase, dto.Strategy)
	}
	if !dto.Deployment.IsNull() {
		t.Errorf("deployment = %v", dto.Deployment)
	}
	if d, ok := dto.DeployDecision.Get(); !ok || d != "StaleSource" {
		t.Errorf("deploy decision = %v", dto.DeployDecision)
	}
	raw := checked[[]byte](t)(json.Marshal(&dto))
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["deployDecision"]; !ok {
		t.Errorf("camelCase: %s", raw)
	}
	if v, ok := fields["deployment"]; !ok || v != nil {
		t.Errorf("an absent deployment is null, as serde wrote it: %s", raw)
	}
}

// routes/apps/builds.rs build_ids_are_uuids.
func TestBuildIdsAreUuids(t *testing.T) {
	if _, err := api.BuildID("0192f3a1-0000-7000-8000-000000000001"); err != nil {
		t.Errorf("a UUID: %v", err)
	}
	_, err := api.BuildID("nope")
	var e *kerr.Error
	if !errors.As(err, &e) || e.Code != kerr.NotFound {
		t.Errorf("not a UUID: %v", err)
	}
}
