package store

// Image update policies (M5.4, migration 0033) and the deployment a new
// digest starts; the port of repo/image_policies.rs.

import (
	"context"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/artifact"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
)

// policiesSelect is the policies, before a filter.
const policiesSelect = "SELECT p.target_id, p.project_id, t.application_id, pl.environment_id, p.repository, p.pattern, " +
	"p.enabled, p.interval_secs, p.next_check_at, p.failures, p.last_checked_at, p.last_tag, " +
	"p.last_digest, p.last_error, p.last_run_id, p.updated_by, p.updated_at " +
	"FROM image_policies p " +
	"JOIN application_targets t ON t.id = p.target_id AND t.org_id = p.org_id " +
	"JOIN environment_placements pl ON pl.id = t.placement_id AND pl.org_id = t.org_id "

const (
	policyOf    = policiesSelect + "WHERE p.target_id = $1 AND p.org_id = $2"
	duePolicies = policiesSelect +
		"WHERE p.org_id = $1 AND p.enabled AND p.next_check_at <= $2 AND t.deleted_at IS NULL AND NOT t.deleting " +
		"ORDER BY p.next_check_at LIMIT $3 FOR UPDATE OF p SKIP LOCKED"
	upsertPolicy = "INSERT INTO image_policies " +
		"(target_id, org_id, project_id, repository, pattern, enabled, interval_secs, next_check_at, updated_by, updated_at) " +
		"VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $8) " +
		"ON CONFLICT (target_id) DO UPDATE SET repository = EXCLUDED.repository, pattern = EXCLUDED.pattern, " +
		"enabled = EXCLUDED.enabled, interval_secs = EXCLUDED.interval_secs, next_check_at = EXCLUDED.next_check_at, " +
		"failures = 0, last_error = NULL, updated_by = EXCLUDED.updated_by, updated_at = EXCLUDED.updated_at"
	deletePolicy  = "DELETE FROM image_policies WHERE target_id = $1 AND org_id = $2"
	policyChecked = "UPDATE image_policies SET last_checked_at = $3, next_check_at = $4, failures = $5, " +
		"last_error = $6, last_tag = coalesce($7, last_tag), last_digest = coalesce($8, last_digest), " +
		"last_run_id = coalesce($9, last_run_id) WHERE target_id = $1 AND org_id = $2"
	currentDigest = "SELECT rel.artifacts ->> 'web' FROM deployment_runs r " +
		"JOIN releases rel ON rel.id = r.release_id AND rel.org_id = r.org_id " +
		"WHERE r.target_id = $1 AND r.org_id = $2 " +
		"AND r.phase NOT IN ('failed', 'cancelled', 'superseded', 'recoveryFailed') " +
		"ORDER BY r.generation DESC LIMIT 1"
	enabledPolicies = "SELECT count(*) FROM image_policies WHERE org_id = $1 AND enabled"
)

// ImagePolicy is an app's image update policy.
type ImagePolicy struct {
	Target        ids.TargetID
	Project       ids.ProjectID
	Application   ids.ApplicationID
	Environment   ids.EnvironmentID
	Repository    string
	Pattern       string
	Enabled       bool
	IntervalSecs  int32
	NextCheckAt   int64
	Failures      int32
	LastCheckedAt opt.Val[int64]
	LastTag       opt.Val[string]
	LastDigest    opt.Val[string]
	LastError     opt.Val[string]
	LastRun       opt.Val[uuid.UUID]
	UpdatedBy     string
	UpdatedAt     int64
}

// NewImagePolicy is a policy to set.
type NewImagePolicy struct {
	Project      ids.ProjectID
	Target       ids.TargetID
	Repository   string
	Pattern      string
	Enabled      bool
	IntervalSecs uint32
	By           string
}

// PolicyCheck is what one check of a policy found (Rust's Checked).
type PolicyCheck struct {
	NextCheckAt int64
	Failures    uint32
	Error       opt.Val[string]
	Tag         opt.Val[string]
	Digest      opt.Val[string]
	Run         opt.Val[ids.DeploymentRunID]
}

func scanImagePolicy(r pgx.CollectableRow) (ImagePolicy, error) {
	var (
		p                    ImagePolicy
		checkedAt            *int64
		tag, digest, lastErr *string
		lastRun              *uuid.UUID
	)
	err := r.Scan(&p.Target, &p.Project, &p.Application, &p.Environment, &p.Repository, &p.Pattern,
		&p.Enabled, &p.IntervalSecs, &p.NextCheckAt, &p.Failures, &checkedAt, &tag,
		&digest, &lastErr, &lastRun, &p.UpdatedBy, &p.UpdatedAt)
	p.LastCheckedAt, p.LastTag, p.LastDigest = opt.FromPtr(checkedAt), opt.FromPtr(tag), opt.FromPtr(digest)
	p.LastError, p.LastRun = opt.FromPtr(lastErr), opt.FromPtr(lastRun)
	return p, err
}

// SetImagePolicy sets p as its app's policy; it is checked at once.
func (t *Tenant) SetImagePolicy(ctx context.Context, p NewImagePolicy) error {
	const op = "set an image policy"
	interval, err := int4(op, p.IntervalSecs)
	if err != nil {
		return err
	}
	_, err = exec(ctx, t.tx, op, upsertPolicy, p.Target, t.org.String(), p.Project, p.Repository, p.Pattern,
		p.Enabled, interval, t.store.now(), p.By)
	return err
}

// ImagePolicy is tgt's policy.
func (t *Tenant) ImagePolicy(ctx context.Context, tgt ids.TargetID) (ImagePolicy, bool, error) {
	return queryOpt(ctx, t.tx, "read an image policy", policyOf, scanImagePolicy, tgt, t.org.String())
}

// DeleteImagePolicy removes tgt's policy; false when it has none.
func (t *Tenant) DeleteImagePolicy(ctx context.Context, tgt ids.TargetID) (bool, error) {
	n, err := exec(ctx, t.tx, "delete an image policy", deletePolicy, tgt, t.org.String())
	return n == 1, err
}

// DueImagePolicies are the enabled policies due at now, locked for this
// transaction.
func (t *Tenant) DueImagePolicies(ctx context.Context, now, limit int64) ([]ImagePolicy, error) {
	return queryAll(ctx, t.tx, "read the due image policies", duePolicies, scanImagePolicy, t.org.String(), now, limit)
}

// ImagePolicyChecked records a check of tgt's policy.
func (t *Tenant) ImagePolicyChecked(ctx context.Context, tgt ids.TargetID, c PolicyCheck) error {
	const op = "record an image policy check"
	failures, err := int4(op, min(c.Failures, 1_000))
	if err != nil {
		return err
	}
	var message opt.Val[string]
	if e, ok := c.Error.Get(); ok {
		message = opt.Some(truncateChars(e, 1024))
	}
	var run any
	if r, ok := c.Run.Get(); ok {
		run = r
	}
	_, err = exec(ctx, t.tx, op, policyChecked, tgt, t.org.String(), t.store.now(), c.NextCheckAt, failures,
		message.Ptr(), c.Tag.Ptr(), c.Digest.Ptr(), run)
	return err
}

// CurrentDigest is the digest tgt runs or is about to run.
func (t *Tenant) CurrentDigest(ctx context.Context, tgt ids.TargetID) (string, bool, error) {
	found, ok, err := queryOpt(ctx, t.tx, "read the current digest", currentDigest, pgx.RowTo[*string], tgt, t.org.String())
	if err != nil || !ok || found == nil {
		return "", false, err
	}
	return *found, true, nil
}

// DeployFollowedImage deploys digest of policy's repository to its app with
// the app's newest configuration. The environment's policy decides whether
// the run waits for approval.
func (t *Tenant) DeployFollowedImage(ctx context.Context, policy ImagePolicy, digest artifact.Digest, given string) (Started, error) {
	config, ok, err := t.LatestConfigRevision(ctx, policy.Target)
	if err != nil || !ok {
		return StartedNotFound{}, err
	}
	state, ok, err := t.TargetState(ctx, policy.Target)
	if err != nil || !ok {
		return StartedNotFound{}, err
	}
	by := "image-policy:" + policy.Target.String()
	release, _, err := t.CreateRelease(ctx, policy.Project, PortableRelease{
		Application:     policy.Application,
		Artifacts:       map[string]artifact.Digest{"web": digest},
		ProcessContract: map[string]any{},
		PortableConfig:  map[string]any{},
		RendererSchema:  1,
		Source: opt.Some[any](map[string]any{
			"image_repository": policy.Repository, "image": given, "policy": policy.Pattern,
		}),
		CreatedBy: by,
	})
	if err != nil {
		return nil, err
	}
	input := release.String() + "/" + config.String() + "/" + strconv.FormatUint(uint64(state.DesiredGeneration), 10)
	request := StartDeployment{
		Project: policy.Project, Target: policy.Target, Release: release, ConfigRevision: config,
		ExpectedGeneration: state.DesiredGeneration, LifecycleUID: state.LifecycleUID, Reason: ReasonDeploy,
		RequestedBy: by, InputHash: []byte(input),
	}
	audit := NewAudit{
		ActorKind: "system", ActorID: opt.Some(by), Action: "deployment.accepted",
		TargetKind: opt.Some("app"), TargetRef: opt.Some(policy.Target.String()), Outcome: "accepted",
		Data: opt.Some[any](map[string]any{"image": given, "digest": digest.String(), "pattern": policy.Pattern}),
	}
	return t.StartDeployment(ctx, request, audit, opt.None[IdempotencyKey]())
}

// ImagePolicyOrgs are the organizations with an enabled image policy, for
// the watcher.
func (s *Store) ImagePolicyOrgs(ctx context.Context) ([]ids.OrgID, error) {
	orgs, err := s.OrgIDs(ctx)
	if err != nil {
		return nil, err
	}
	out := []ids.OrgID{}
	for _, org := range orgs {
		n, err := s.enabledPolicies(ctx, org)
		if err != nil {
			return nil, err
		}
		if n > 0 {
			out = append(out, org)
		}
	}
	return out, nil
}

func (s *Store) enabledPolicies(ctx context.Context, org ids.OrgID) (int64, error) {
	t, err := s.Tenant(ctx, org)
	if err != nil {
		return 0, err
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	var n int64
	err = queryOne(ctx, t.tx, "count the enabled image policies", enabledPolicies, []any{&n}, org.String())
	return n, err
}
