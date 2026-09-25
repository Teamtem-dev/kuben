package store_test

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/internal/core/artifact"
	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/policy"
	"github.com/Teamtem-dev/kuben/internal/core/scan"
	"github.com/Teamtem-dev/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/internal/store/pgtest"
)

// Ported from repo/scans.rs.

const (
	scanDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	scanNow    = int64(1_800_000_000_000)
)

type scanFixture struct {
	org     ids.OrgID
	project ids.ProjectID
	target  ids.TargetID
	release ids.ReleaseID
}

func newScanFixture(t *testing.T, s *store.Store, gate scan.Gate) scanFixture {
	t.Helper()
	ctx := t.Context()
	o := org(t, s, "a", "A")
	tn := tenant(t, s, o)
	project := must[ids.ProjectID](t, "project")(tn.CreateProject(ctx, "shop", "Shop"))
	env := must[ids.EnvironmentID](t, "environment")(tn.CreateEnvironment(ctx, project, "prod", "Prod", true))
	p := policy.Open()
	p.Scan = gate
	if _, ok, err := tn.SetEnvironmentPolicy(ctx, project, env, p, "user:a"); err != nil || !ok {
		t.Fatalf("policy: %v, %v", ok, err)
	}
	cluster := must[ids.ClusterID](t, "cluster")(tn.CreateCluster(ctx, "eu-1"))
	placement := must[ids.PlacementID](t, "placement")(tn.CreatePlacement(ctx, project, env, cluster, "a-shop"))
	application := must[ids.ApplicationID](t, "app")(tn.CreateApplication(ctx, project, "web", "Web"))
	tgt := must[ids.TargetID](t, "target")(tn.CreateTarget(ctx, project, application, placement))
	release, _ := mustCreate(t)(tn.CreateRelease(ctx, project, store.PortableRelease{
		Application:     application,
		Artifacts:       map[string]artifact.Digest{"web": parseDigest(t, scanDigest)},
		ProcessContract: map[string]any{},
		PortableConfig:  map[string]any{},
		RendererSchema:  1,
		Source:          opt.Some[any](map[string]any{"image_repository": "registry.local/acme/web"}),
		CreatedBy:       "user:a",
	}))
	commit(t, tn)
	return scanFixture{org: o, project: project, target: tgt, release: release}
}

func newScan(critical uint32, findings []string, at int64) store.NewScan {
	listed := make([]scan.Finding, 0, len(findings))
	for _, id := range findings {
		listed = append(listed, scan.Finding{Severity: scan.SeverityCritical, ID: id})
	}
	return store.NewScan{
		Repository: "registry.local/acme/web",
		Digest:     scanDigest,
		Summary: scan.Summary{
			Status:      scan.StatusOK,
			Scanner:     "trivy 0.74.0",
			DBUpdatedAt: opt.Some(at - 3_600_000),
			Counts:      scan.Counts{Critical: critical},
			Findings:    listed,
			ScannedAt:   at,
		},
	}
}

func scanDeploy(t *testing.T, s *store.Store, f scanFixture, reason store.RunReason) store.Started {
	t.Helper()
	ctx := t.Context()
	tn := tenant(t, s, f.org)
	revision := configRevision(t, tn, f.project, f.target, map[string]any{})
	state, ok, err := tn.TargetState(ctx, f.target)
	if err != nil || !ok {
		t.Fatalf("target: %v, %v", ok, err)
	}
	started := must[store.Started](t, "start")(tn.StartDeployment(ctx, store.StartDeployment{
		Project:            f.project,
		Target:             f.target,
		Release:            f.release,
		ConfigRevision:     revision.ID,
		ExpectedGeneration: state.DesiredGeneration,
		LifecycleUID:       state.LifecycleUID,
		Reason:             reason,
		RequestedBy:        "user:a",
		InputHash:          fmt.Append(nil, revision.ID),
	}, store.NewAudit{}, opt.None[store.IdempotencyKey]()))
	commit(t, tn)
	return started
}

func recordScan(t *testing.T, tn *store.Tenant, n store.NewScan) {
	t.Helper()
	if _, err := tn.RecordScan(t.Context(), n); err != nil {
		t.Fatalf("scan: %v", err)
	}
}

func verdict(t *testing.T, tn *store.Tenant, f scanFixture, release ids.ReleaseID) (scan.Verdict, bool) {
	t.Helper()
	v, ok, err := tn.ScanVerdict(t.Context(), f.target, release, nowMs())
	if err != nil {
		t.Fatalf("verdict: %v", err)
	}
	return v, ok
}

func TestScansSbomsAndTheNewestScanPerDigest(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	f := newScanFixture(t, s, scan.Production())
	tn := tenant(t, s, f.org)
	recordScan(t, tn, newScan(2, []string{"CVE-1", "CVE-2"}, scanNow-1000))
	recordScan(t, tn, newScan(0, nil, scanNow))
	latest := must[map[string]scan.Summary](t, "read")(tn.LatestScans(ctx, []string{scanDigest, "sha256:22"}))
	if len(latest) != 1 || latest[scanDigest].Counts.Critical != 0 || latest[scanDigest].ScannedAt != scanNow {
		t.Fatalf("latest: %#v", latest)
	}
	history := must[[]scan.Summary](t, "read")(tn.ScanHistory(ctx, scanDigest, 10))
	if len(history) != 2 {
		t.Fatalf("history: %d", len(history))
	}
	if got := history[1].Findings[0]; got != (scan.Finding{Severity: scan.SeverityCritical, ID: "CVE-1"}) {
		t.Fatalf("finding: %#v", got)
	}
	gz := []byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 3, 1, 2, 3, 4, 5, 6, 7, 8}
	if !must[bool](t, "sbom")(tn.PutSbom(ctx, scanDigest, gz)) {
		t.Fatal("kept")
	}
	if must[bool](t, "sbom")(tn.PutSbom(ctx, scanDigest, gz)) {
		t.Fatal("kept once")
	}
	if kept, ok, err := tn.Sbom(ctx, scanDigest); err != nil || !ok || !cmp.Equal(kept, gz) {
		t.Fatalf("sbom: %v, %v, %v", kept, ok, err)
	}
	kept := must[map[string]struct{}](t, "read")(tn.SbomsKept(ctx, []string{scanDigest, "sha256:33"}))
	if diff := cmp.Diff(map[string]struct{}{scanDigest: {}}, kept); diff != "" {
		t.Fatalf("kept (-want +got):\n%s", diff)
	}
	if digests, ok, err := tn.ReleaseDigests(ctx, f.release); err != nil || !ok || !cmp.Equal(digests, []string{scanDigest}) {
		t.Fatalf("digests: %v, %v, %v", digests, ok, err)
	}
	commit(t, tn)
}

func TestTheGateRefusesOpenFindingsUntilAnExceptionCoversThem(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	f := newScanFixture(t, s, scan.Production())
	if started := scanDeploy(t, s, f, store.ReasonDeploy); !isAccepted(started) {
		t.Fatalf("no scan yet: %#v", started)
	}
	tn := tenant(t, s, f.org)
	recordScan(t, tn, newScan(1, []string{"CVE-2026-1"}, nowMs()))
	commit(t, tn)
	if started := scanDeploy(t, s, f, store.ReasonDeploy); started != (store.StartedVulnerabilityBlocked{}) {
		t.Fatalf("blocked: %#v", started)
	}
	if started := scanDeploy(t, s, f, store.ReasonRestart); !isAccepted(started) {
		t.Fatalf("a restart keeps the running release: %#v", started)
	}

	tn = tenant(t, s, f.org)
	id := must[uuid.UUID](t, "exception")(tn.CreateException(ctx, store.NewException{
		Project:       opt.Some(f.project),
		Vulnerability: "CVE-2026-1",
		Reason:        "not reachable",
		Owner:         "platform",
		CreatedBy:     "user:a",
		ExpiresAt:     nowMs() + 3_600_000,
	}))
	if v, ok := verdict(t, tn, f, f.release); !ok || v != (scan.Pass{}) {
		t.Fatalf("excepted: %#v, %v", v, ok)
	}
	if n := len(must[[]store.VulnException](t, "read")(tn.Exceptions(ctx, false))); n != 1 {
		t.Fatalf("exceptions in force: %d", n)
	}
	if !must[bool](t, "revoke")(tn.RevokeException(ctx, id, "user:a")) {
		t.Fatal("revoked")
	}
	if must[bool](t, "revoke")(tn.RevokeException(ctx, id, "user:a")) {
		t.Fatal("once")
	}
	if n := len(must[[]store.VulnException](t, "read")(tn.Exceptions(ctx, false))); n != 0 {
		t.Fatalf("exceptions in force: %d", n)
	}
	if all := must[[]store.VulnException](t, "read")(tn.Exceptions(ctx, true)); all[0].RevokedBy != opt.Some("user:a") {
		t.Fatalf("revoked by %v", all[0].RevokedBy)
	}
	if v, ok := verdict(t, tn, f, f.release); !ok || !isBlock(v) {
		t.Fatalf("blocked again: %#v, %v", v, ok)
	}
	if v, ok := verdict(t, tn, f, ids.New[ids.Release]()); ok {
		t.Fatalf("another release: %#v", v)
	}
	commit(t, tn)
	tn = tenant(t, s, f.org)
	state, ok, err := tn.TargetState(ctx, f.target)
	if err != nil || !ok {
		t.Fatalf("target: %v, %v", ok, err)
	}
	if state.DesiredGeneration != 2 {
		t.Fatalf("refused runs wrote nothing: %d", state.DesiredGeneration)
	}
}

func isAccepted(started store.Started) bool {
	_, ok := started.(store.StartedAccepted)
	return ok
}

func isBlock(v scan.Verdict) bool {
	_, ok := v.(scan.Block)
	return ok
}

func TestRunningImagesAreDueForARescan(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	f := newScanFixture(t, s, scan.Off())
	accepted, ok := scanDeploy(t, s, f, store.ReasonDeploy).(store.StartedAccepted)
	if !ok {
		t.Fatal("not accepted")
	}
	tn := tenant(t, s, f.org)
	if due := must[[]store.ImageRef](t, "read")(tn.RescanDue(ctx, scanNow, 10)); len(due) != 0 {
		t.Fatalf("nothing succeeded yet: %v", due)
	}
	if _, err := tn.TestExec(ctx, "UPDATE deployment_runs SET phase = 'succeeded' WHERE operation_id = $1", accepted.Operation); err != nil {
		t.Fatalf("succeed: %v", err)
	}
	due := must[[]store.ImageRef](t, "read")(tn.RescanDue(ctx, nowMs(), 10))
	if diff := cmp.Diff([]store.ImageRef{{Repository: "registry.local/acme/web", Digest: scanDigest}}, due); diff != "" {
		t.Fatalf("due (-want +got):\n%s", diff)
	}
	recordScan(t, tn, newScan(0, nil, nowMs()))
	if due := must[[]store.ImageRef](t, "read")(tn.RescanDue(ctx, nowMs()-60_000, 10)); len(due) != 0 {
		t.Fatalf("scanned recently: %v", due)
	}
	commit(t, tn)
}
