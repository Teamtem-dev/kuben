package httpapi

// Deploy acceptance on the SQL model (routes/apps/deployments.rs, ADR-032;
// plan §8.2).
//
// `POST …/deployments` accepts a deployment run in one transaction and
// answers 202 with the run to poll; the materializer carries it out. Only
// apps that exist in SQL are found here: an app known only as a resource is
// 404 until it is created through SQL or imported.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/artifact"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ops/target"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/policy"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/access"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/httpx"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/jsonx"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

// receiptTTL is how long an Idempotency-Key receipt is kept; the run
// itself stays.
const receiptTTL = 24 * time.Hour

// runReason is From<DeployReason> for RunReason.
func runReason(r gen.DeployReason) store.RunReason {
	switch r {
	case gen.DeployReasonRollback:
		return store.ReasonRollback
	case gen.DeployReasonPromotion:
		return store.ReasonPromotion
	case gen.DeployReasonDeploy:
	}
	return store.ReasonDeploy
}

// deploymentDto is DeploymentDto::from(RunSummary).
func deploymentDto(s store.RunSummary) gen.DeploymentDto {
	var expires gen.OptNilInt64
	if at, ok := s.ApprovalExpiresAt.Get(); ok {
		expires = gen.NewOptNilInt64(at)
	} else {
		expires.SetToNull()
	}
	var hash gen.OptNilString
	if h, ok := s.PlanHash.Get(); ok {
		hash = gen.NewOptNilString(policy.Hex(h[:]))
	} else {
		hash.SetToNull()
	}
	return gen.DeploymentDto{
		Run: s.Run.UUID(), Operation: s.Operation.UUID(), Generation: int64(uint64(s.Generation)), //nolint:gosec // generations stay far below 2^63
		Phase: string(s.Phase), ApprovalsRequired: gen.NewOptInt32(int32(s.ApprovalsRequired)),
		ApprovalExpiresAt: expires, PlanHash: hash,
	}
}

// idempotencyKey is the caller's Idempotency-Key, if any: 1–200 visible
// ASCII characters.
func idempotencyKey(header gen.OptNilString, actor string) (opt.Val[store.IdempotencyKey], error) {
	value, ok := header.Get()
	if !ok {
		return opt.None[store.IdempotencyKey](), nil
	}
	key := trimSpace(value)
	visible := !strings.ContainsFunc(key, func(r rune) bool { return r <= ' ' || r >= 0x7f })
	if key == "" || len(key) > 200 || !visible {
		return opt.None[store.IdempotencyKey](), kerrors.New(kerrors.Validation,
			"Idempotency-Key must be 1 to 200 visible ASCII characters")
	}
	return opt.Some(store.IdempotencyKey{Actor: actor, Key: key, TTL: receiptTTL}), nil
}

// pinnedImage splits `repository@sha256:…` into the repository and the
// digest.
func pinnedImage(image string) (string, artifact.Digest, error) {
	i := strings.LastIndexByte(image, '@')
	if i <= 0 {
		return "", artifact.Digest{}, kerrors.New(kerrors.Validation,
			"`%s` is not pinned by digest: use repository@sha256:…", image)
	}
	digest, err := artifact.ParseDigest(image[i+1:])
	if err != nil {
		return "", artifact.Digest{}, kerrors.New(kerrors.Validation, "%s", err.Error())
	}
	return image[:i], digest, nil
}

// deploymentRequest is the request as Rust hashed it: its fields as JSON,
// absent ones null, the configuration as given.
type deploymentRequest struct {
	image              opt.Val[string]
	release            opt.Val[uuid.UUID]
	config             opt.Val[any]
	reason             gen.DeployReason
	expectedGeneration uint64
}

func readDeploymentRequest(req *gen.StartDeploymentRequest) (deploymentRequest, error) {
	if req.ExpectedGeneration < 0 {
		return deploymentRequest{}, kerrors.New(kerrors.Validation,
			"expected_generation: invalid value: integer `%d`, expected u64", req.ExpectedGeneration)
	}
	out := deploymentRequest{
		reason: req.Reason.Or(gen.DeployReasonDeploy), expectedGeneration: uint64(req.ExpectedGeneration),
	}
	if image, ok := req.Image.Get(); ok {
		out.image = opt.Some(image)
	}
	if release, ok := req.Release.Get(); ok {
		out.release = opt.Some(release)
	}
	if config, ok := req.Config.Get(); ok {
		data, err := json.Marshal(config)
		if err != nil {
			return deploymentRequest{}, kerrors.Wrap(err, "a deployment's config")
		}
		value, err := jsonx.DecodeAny(data)
		if err != nil {
			return deploymentRequest{}, kerrors.Wrap(err, "a deployment's config")
		}
		out.config = opt.Some(value)
	}
	return out, nil
}

// inputHash is sha-256 of the canonical request with its path, the text
// Rust hashed (serde_json with sorted keys), so a replay across the cutover
// still matches its receipt.
func (r deploymentRequest) inputHash(project, environment, app string) ([]byte, error) {
	request := map[string]any{
		"image": nil, "release": nil, "config": nil, "reason": string(r.reason),
		"expected_generation": json.Number(strconv.FormatUint(r.expectedGeneration, 10)),
	}
	if image, ok := r.image.Get(); ok {
		request["image"] = image
	}
	if release, ok := r.release.Get(); ok {
		request["release"] = release.String()
	}
	if config, ok := r.config.Get(); ok {
		request["config"] = config
	}
	text, err := jsonx.CanonicalValue(map[string]any{
		"project": project, "environment": environment, "app": app, "request": request,
	})
	if err != nil {
		return nil, kerrors.Wrap(err, "a deployment request")
	}
	sum := sha256.Sum256([]byte(text))
	return sum[:], nil
}

// StartDeployment accepts a deployment of this app. The release (from
// `image`, or an existing `release`), the configuration revision and the
// run are written in one transaction with the audit record and the message
// to the executors; then the call answers 202 with the run's Location.
// Replaying the request with the same Idempotency-Key returns the first
// run.
func (s *Server) StartDeployment(
	ctx context.Context, req *gen.StartDeploymentRequest, params gen.StartDeploymentParams,
) (gen.StartDeploymentRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	app, err := s.findApp(ctx, a, params.Project, params.Environment, params.App)
	if err != nil {
		return nil, err
	}
	if _, err := a.Require(perm.AppDeploy, app.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	actorKind, actor := a.Actor()
	key, err := idempotencyKey(params.IdempotencyKey, actor)
	if err != nil {
		return nil, err
	}
	body, err := readDeploymentRequest(req)
	if err != nil {
		return nil, err
	}
	hash, err := body.inputHash(params.Project, params.Environment, params.App)
	if err != nil {
		return nil, err
	}
	t, err := s.deps.Store.Tenant(ctx, app.env.project.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit; drops a replay's writes
	summary, warnings, err := s.startRun(ctx, t, a, app, body, key, hash, actorKind, actor)
	if err != nil {
		return nil, err
	}
	httpx.SetHeader(ctx, "Location", "/api/v1/projects/"+params.Project+"/environments/"+params.Environment+
		"/apps/"+params.App+"/deployments/"+summary.Run.String())
	dto := deploymentDto(summary)
	if len(warnings) > 0 {
		dto.Warnings = warnings
	}
	return &dto, nil
}

// startRun admits, records and starts the run body asks for, in t, and
// commits unless the request was a replay.
func (s *Server) startRun(
	ctx context.Context, t *store.Tenant, a access.Access, app appScope, body deploymentRequest,
	key opt.Val[store.IdempotencyKey], hash []byte, actorKind, actor string,
) (store.RunSummary, []string, error) {
	if err := ensureMayDeploy(ctx, t, a, app.app.Target, app.chain()); err != nil {
		return store.RunSummary{}, nil, err
	}
	var warnings []string
	if config, ok := body.config.Get(); ok {
		// Without a new configuration the resources do not change.
		spec, ok := specOf(opt.Some(config), opt.Some(anyImage))
		if !ok {
			return store.RunSummary{}, nil, kerrors.New(kerrors.Validation, "`config` is not an app configuration")
		}
		admitted, err := s.admit(ctx, t, admissionPlacement{environment: app.env.env.ID, quota: app.env.env.Quota, target: app.app.Target}, &spec)
		if err != nil {
			return store.RunSummary{}, nil, err
		}
		warnings = admitted
	}
	release, err := s.releaseFor(ctx, t, app, body, actor)
	if err != nil {
		return store.RunSummary{}, nil, err
	}
	reason := runReason(body.reason)
	if reason.CarriesNewCode() {
		gate, err := s.scanGate(ctx, t, app.app.Target, release)
		if err != nil {
			return store.RunSummary{}, nil, err
		}
		warnings = append(warnings, gate...)
	}
	revision, err := configRevisionFor(ctx, t, app, body.config, actor)
	if err != nil {
		return store.RunSummary{}, nil, err
	}
	state, found, err := t.TargetState(ctx, app.app.Target)
	if err != nil {
		return store.RunSummary{}, nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found {
		return store.RunSummary{}, nil, kerrors.New(kerrors.NotFound, "app `%s`", app.app.Slug)
	}
	started, err := t.StartDeployment(ctx, store.StartDeployment{
		Project: app.env.project.project.ID, Target: app.app.Target, Release: release, ConfigRevision: revision,
		ExpectedGeneration: target.Generation(body.expectedGeneration), LifecycleUID: state.LifecycleUID,
		Reason: reason, RequestedBy: actor, InputHash: hash,
	}, store.NewAudit{
		ActorKind: actorKind, ActorID: opt.Some(a.Current.User.ID.String()), Action: "startDeployment",
		TargetKind: opt.Some("app"), TargetRef: opt.Some(app.reference()), Outcome: "accepted",
	}, key)
	if err != nil {
		return store.RunSummary{}, nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	summary, err := s.acceptedRun(ctx, t, app, started)
	return summary, warnings, err
}

// acceptedRun is the run a start answered with: the new run (committed), or
// the first run of a replay (nothing of this request is kept).
func (s *Server) acceptedRun(ctx context.Context, t *store.Tenant, app appScope, started store.Started) (store.RunSummary, error) {
	switch st := started.(type) {
	case store.StartedAccepted:
		summary, found, err := t.RunOfTarget(ctx, app.app.Target, st.Run)
		if err != nil || !found {
			return store.RunSummary{}, orConflict(err, kerrors.Wrap(nil, "the accepted run is missing"))
		}
		if err := t.Commit(ctx); err != nil {
			return store.RunSummary{}, err //nolint:wrapcheck // a store error, answered as internal
		}
		return summary, nil
	case store.StartedReplayed:
		summary, found, err := t.RunOfOperation(ctx, st.Operation)
		if err != nil || !found {
			return store.RunSummary{}, orConflict(err, kerrors.Wrap(nil, "the replayed run is missing"))
		}
		return summary, nil
	case store.StartedKeyReused:
		return store.RunSummary{}, kerrors.New(kerrors.Conflict, "this Idempotency-Key was used for a different request")
	case store.StartedNotFound:
		return store.RunSummary{}, kerrors.New(kerrors.NotFound, "that release or configuration of this app")
	case store.StartedRejected, store.StartedSecretRevoked, store.StartedVulnerabilityBlocked,
		store.StartedFrozen, store.StartedUntrusted:
	}
	return store.RunSummary{}, startedErr(started)
}

// releaseFor is the release to deploy: a new one from `image`
// (deduplicated by content), or an existing `release`. Exactly one of the
// two must be given.
func (s *Server) releaseFor(ctx context.Context, t *store.Tenant, app appScope, body deploymentRequest, actor string) (ids.ReleaseID, error) {
	image, hasImage := body.image.Get()
	release, hasRelease := body.release.Get()
	switch {
	case hasImage && !hasRelease:
		repository, digest, err := pinnedImage(image)
		if err != nil {
			return ids.ReleaseID{}, err
		}
		id, _, err := t.CreateRelease(ctx, app.env.project.project.ID, store.PortableRelease{
			Application: app.app.Application, Artifacts: map[string]artifact.Digest{webProcess: digest},
			ProcessContract: map[string]any{}, PortableConfig: map[string]any{}, RendererSchema: 1,
			Source:    opt.Some[any](map[string]any{"image_repository": repository}),
			CreatedBy: actor,
		})
		return id, err //nolint:wrapcheck // a store error, answered as internal
	case !hasImage && hasRelease:
		return ids.From[ids.Release](release), nil
	}
	return ids.ReleaseID{}, kerrors.New(kerrors.Validation, "give exactly one of `image` and `release`")
}

// configRevisionFor is the configuration to deploy: a new revision from
// config, or the target's latest one.
func configRevisionFor(ctx context.Context, t *store.Tenant, app appScope, config opt.Val[any], actor string) (ids.ConfigRevisionID, error) {
	if c, ok := config.Get(); ok {
		revision, found, err := t.CreateConfigRevision(ctx, app.env.project.project.ID, app.app.Target, c, actor)
		if err != nil || !found {
			return ids.ConfigRevisionID{}, orConflict(err, kerrors.New(kerrors.NotFound, "the app"))
		}
		return revision.ID, nil
	}
	revision, found, err := t.LatestConfigRevision(ctx, app.app.Target)
	if err != nil || !found {
		return ids.ConfigRevisionID{}, orConflict(err,
			kerrors.New(kerrors.Validation, "the app has no configuration yet: send `config`"))
	}
	return revision, nil
}

// ListDeployments is the app's newest deployment runs, each with its
// timeline.
func (s *Server) ListDeployments(ctx context.Context, params gen.ListDeploymentsParams) (gen.ListDeploymentsRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	app, err := s.findApp(ctx, a, params.Project, params.Environment, params.App)
	if err != nil {
		return nil, err
	}
	if _, err := a.Require(perm.AppRead, app.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	runs, phases, err := s.runsOf(ctx, app, min(max(params.Limit.Or(10), 1), 50))
	if err != nil {
		return nil, err
	}
	actors, err := s.loadActors(ctx)
	if err != nil {
		return nil, err
	}
	items := gen.ListDeploymentsOKApplicationJSON{}
	for _, r := range runs {
		timeline := []gen.PhaseStep{}
		for _, p := range phases[r.Run] {
			timeline = append(timeline, gen.PhaseStep{Phase: p.Phase, At: p.At})
		}
		items = append(items, gen.DeploymentSummary{
			Run: r.Run.UUID(), Generation: int64(uint64(r.Generation)), Reason: r.Reason, Phase: string(r.Phase), //nolint:gosec // bounded
			Outcome: optNilString(r.Outcome), RequestedBy: actors.name(r.RequestedBy), CreatedAt: r.CreatedAt,
			Image: optNilString(r.Image), Timeline: timeline,
		})
	}
	return &items, nil
}

func (s *Server) runsOf(ctx context.Context, app appScope, limit int64) ([]store.RunRecord, map[ids.DeploymentRunID][]store.PhaseEntry, error) {
	t, err := s.deps.Store.Tenant(ctx, app.env.project.org)
	if err != nil {
		return nil, nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	runs, err := t.Runs(ctx, app.app.Target, limit)
	if err != nil {
		return nil, nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	runIDs := make([]ids.DeploymentRunID, 0, len(runs))
	for _, r := range runs {
		runIDs = append(runIDs, r.Run)
	}
	phases, err := t.RunPhases(ctx, app.app.Target, runIDs)
	return runs, phases, err //nolint:wrapcheck // a store error, answered as internal
}

// GetDeployment is one deployment run of this app.
func (s *Server) GetDeployment(ctx context.Context, params gen.GetDeploymentParams) (gen.GetDeploymentRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	app, err := s.findApp(ctx, a, params.Project, params.Environment, params.App)
	if err != nil {
		return nil, err
	}
	if _, err := a.Require(perm.AppRead, app.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	t, err := s.deps.Store.Tenant(ctx, app.env.project.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	summary, found, err := t.RunOfTarget(ctx, app.app.Target, ids.From[ids.DeploymentRun](params.Run))
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found {
		return nil, kerrors.New(kerrors.NotFound, "deployment `%s`", params.Run)
	}
	dto := deploymentDto(summary)
	return &dto, nil
}
