package store

// Project and environment lifecycles, and deletion (ADR-032, M1.8b step 2);
// the port of repo/lifecycle.rs.
//
// The API writes intent in SQL and asks the materializer, through durable
// operations, to make the cluster follow: write a project's or an
// environment's resource right away (an environment's namespace holds
// secrets before any app is deployed), and remove the resources of what is
// being deleted. A deletion is soft: the request's transaction marks the row
// `deleting`, and the row gets `deleted_at` once its resources are gone.

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/wire"
)

// The lifecycle operation kinds.
const (
	// ProjectApply writes a project's resource.
	ProjectApply = "project.apply"
	// EnvironmentApply writes an environment's resource (and its project's).
	EnvironmentApply = "environment.apply"
	// ProjectDelete removes a project's resource, then marks the project
	// deleted.
	ProjectDelete = "project.delete"
	// EnvironmentDelete removes an environment's resource (its controller
	// removes the namespace), then marks the environment, its placement and
	// its apps deleted.
	EnvironmentDelete = "environment.delete"
	// TargetDelete removes an app's resource (and, when asked, its volumes),
	// then marks the target deleted.
	TargetDelete = "target.delete"
)

// LifecycleKinds is every lifecycle operation kind the materializer claims.
func LifecycleKinds() []string {
	return []string{ProjectApply, EnvironmentApply, ProjectDelete, EnvironmentDelete, TargetDelete}
}

const lifecycleTopic = "lifecycle.requested"

const (
	selectPayload = "SELECT payload::text FROM operations WHERE id = $1 AND org_id = $2"
	markProject   = "UPDATE projects SET deleting = TRUE " +
		"WHERE id = $1 AND org_id = $2 AND NOT deleting AND deleted_at IS NULL"
	markEnvironment = "UPDATE environments SET deleting = TRUE " +
		"WHERE id = $1 AND org_id = $2 AND NOT deleting AND deleted_at IS NULL"
	markTarget = "UPDATE application_targets SET deleting = TRUE " +
		"WHERE id = $1 AND org_id = $2 AND NOT deleting AND deleted_at IS NULL"
	liveEnvironments = "SELECT count(*) FROM environments " +
		"WHERE project_id = $1 AND org_id = $2 AND deleted_at IS NULL"
	finishTarget = "UPDATE application_targets SET deleted_at = kuben_now_ms() " +
		"WHERE id = $1 AND org_id = $2 AND deleting AND deleted_at IS NULL"
	// finishApplications deletes the applications without a live target
	// with their last target.
	finishApplications = "UPDATE applications a SET deleted_at = kuben_now_ms() " +
		"WHERE a.org_id = $1 AND a.project_id = $2 AND a.deleted_at IS NULL " +
		"AND NOT EXISTS (SELECT 1 FROM application_targets t " +
		"WHERE t.application_id = a.id AND t.deleted_at IS NULL)"
	finishEnvironmentTargets = "UPDATE application_targets t " +
		"SET deleting = TRUE, deleted_at = kuben_now_ms() " +
		"FROM environment_placements p " +
		"WHERE p.id = t.placement_id AND p.environment_id = $1 AND t.org_id = $2 AND t.deleted_at IS NULL"
	retirePlacements = "UPDATE environment_placements SET state = 'retired' " +
		"WHERE environment_id = $1 AND org_id = $2 AND state <> 'retired'"
	finishEnvironment = "UPDATE environments SET deleted_at = kuben_now_ms() " +
		"WHERE id = $1 AND org_id = $2 AND deleting AND deleted_at IS NULL " +
		"RETURNING project_id"
	finishProject = "UPDATE projects SET deleted_at = kuben_now_ms() " +
		"WHERE id = $1 AND org_id = $2 AND deleting AND deleted_at IS NULL"
)

// Subject is what a lifecycle operation acts on: its payload. The JSON form
// is Rust's: `project`, `environment` and `target` (left out when absent),
// `delete_volumes` (always) and `detach` (only when set).
type Subject struct {
	Project     ids.ProjectID              `json:"project"`
	Environment opt.Val[ids.EnvironmentID] `json:"environment,omitzero"`
	Target      opt.Val[ids.TargetID]      `json:"target,omitzero"`
	// DeleteVolumes (target.delete) also deletes the app's retained volumes.
	DeleteVolumes bool `json:"delete_volumes"`
	// Detach (target.delete) detaches the app instead (M4.11): its objects
	// are orphaned and stay in the cluster.
	Detach bool `json:"detach,omitzero"`
}

// UnmarshalJSON reads a subject as serde did: `project` is required,
// absent and null are the same for `environment` and `target`, the flags
// default to false when absent (null is refused), other members are
// ignored.
func (s *Subject) UnmarshalJSON(data []byte) error {
	var o wire.Object
	if err := json.Unmarshal(data, &o); err != nil {
		return err
	}
	var out Subject
	if err := wire.Required(o, "project", &out.Project); err != nil {
		return err
	}
	if err := wire.Optional(o, "environment", &out.Environment); err != nil {
		return err
	}
	if err := wire.Optional(o, "target", &out.Target); err != nil {
		return err
	}
	for key, flag := range map[string]*bool{"delete_volumes": &out.DeleteVolumes, "detach": &out.Detach} {
		if raw, ok := o[key]; ok {
			if err := json.Unmarshal(raw, flag); err != nil || wire.IsNull(raw) {
				return &json.UnmarshalTypeError{Value: string(raw), Field: key}
			}
		}
	}
	*s = out
	return nil
}

// ProjectSubject is a project.
func ProjectSubject(project ids.ProjectID) Subject { return Subject{Project: project} }

// EnvironmentSubject is an environment of project.
func EnvironmentSubject(project ids.ProjectID, environment ids.EnvironmentID) Subject {
	return Subject{Project: project, Environment: opt.Some(environment)}
}

// TargetSubject is an app: its target, and the environment whose placement
// it is on.
func TargetSubject(project ids.ProjectID, environment ids.EnvironmentID, target ids.TargetID, deleteVolumes bool) Subject {
	return Subject{
		Project: project, Environment: opt.Some(environment), Target: opt.Some(target), DeleteVolumes: deleteVolumes,
	}
}

// DetachSubject is an app to detach (M4.11).
func DetachSubject(project ids.ProjectID, environment ids.EnvironmentID, target ids.TargetID) Subject {
	s := TargetSubject(project, environment, target, false)
	s.Detach = true
	return s
}

// Request asks the materializer to carry out kind on subject. The
// operation, audit and the outbox message commit with this transaction.
func (t *Tenant) Request(ctx context.Context, kind string, subject Subject, requestedBy string, audit NewAudit) (ids.OperationID, error) {
	const op = "request a lifecycle operation"
	payload, err := canonical(op, subject)
	if err != nil {
		return ids.OperationID{}, err
	}
	target := opt.None[OperationTarget]()
	if tg, ok := subject.Target.Get(); ok {
		target = opt.Some(OperationTarget{Project: subject.Project, Target: tg})
	}
	accepted, err := t.Accept(ctx, NewOperation{
		Kind:        kind,
		Target:      target,
		InputHash:   []byte(payload),
		Payload:     json.RawMessage(payload),
		RequestedBy: requestedBy,
		Topic:       lifecycleTopic,
	}, audit, opt.None[IdempotencyKey]())
	if err != nil {
		return ids.OperationID{}, err
	}
	// Without an idempotency key every request is a new operation.
	switch a := accepted.(type) {
	case AcceptedNew:
		return a.ID, nil
	case AcceptedReplayed:
		return a.ID, nil
	case AcceptedKeyReused:
		return a.ID, nil
	}
	return ids.OperationID{}, nil
}

func (t *Tenant) mark(ctx context.Context, op, sql string, id any) (bool, error) {
	n, err := exec(ctx, t.tx, op, sql, id, t.org.String())
	return n == 1, err
}

// MarkProjectDeleting marks project deleting. False when it already is, or
// is not live.
func (t *Tenant) MarkProjectDeleting(ctx context.Context, project ids.ProjectID) (bool, error) {
	return t.mark(ctx, "mark a project deleting", markProject, project)
}

// MarkEnvironmentDeleting marks environment deleting. False when it
// already is, or is not live.
func (t *Tenant) MarkEnvironmentDeleting(ctx context.Context, environment ids.EnvironmentID) (bool, error) {
	return t.mark(ctx, "mark an environment deleting", markEnvironment, environment)
}

// MarkTargetDeleting marks target deleting. False when it already is, or is
// not live.
func (t *Tenant) MarkTargetDeleting(ctx context.Context, target ids.TargetID) (bool, error) {
	return t.mark(ctx, "mark a target deleting", markTarget, target)
}

// LiveEnvironments is the number of live environments of project, deleting
// or not.
func (t *Tenant) LiveEnvironments(ctx context.Context, project ids.ProjectID) (uint64, error) {
	var n int64
	if err := queryOne(ctx, t.tx, "count live environments", liveEnvironments, []any{&n},
		project, t.org.String()); err != nil {
		return 0, err
	}
	// Rust: u64::try_from(n).unwrap_or_default().
	if n < 0 {
		return 0, nil
	}
	return uint64(n), nil //nolint:gosec // checked above
}

// FinishTargetDeletion marks target deleted now that its resources are
// gone, and its application too when that was its last target.
func (t *Tenant) FinishTargetDeletion(ctx context.Context, project ids.ProjectID, target ids.TargetID) (bool, error) {
	done, err := t.mark(ctx, "finish a target's deletion", finishTarget, target)
	if err != nil {
		return false, err
	}
	if err := t.finishApplications(ctx, project); err != nil {
		return false, err
	}
	return done, nil
}

// FinishEnvironmentDeletion marks environment, its placements, its targets
// and the applications left without one deleted, now that its resource and
// namespace are gone.
func (t *Tenant) FinishEnvironmentDeletion(ctx context.Context, environment ids.EnvironmentID) (bool, error) {
	const op = "finish an environment's deletion"
	org := t.org.String()
	for _, sql := range []string{finishEnvironmentTargets, retirePlacements} {
		if _, err := exec(ctx, t.tx, op, sql, environment, org); err != nil {
			return false, err
		}
	}
	project, ok, err := queryOpt(ctx, t.tx, op, finishEnvironment, scanID[ids.Project], environment, org)
	if err != nil || !ok {
		return false, err
	}
	if err := t.finishApplications(ctx, project); err != nil {
		return false, err
	}
	return true, nil
}

// FinishProjectDeletion marks project deleted now that its resource is
// gone.
func (t *Tenant) FinishProjectDeletion(ctx context.Context, project ids.ProjectID) (bool, error) {
	return t.mark(ctx, "finish a project's deletion", finishProject, project)
}

func (t *Tenant) finishApplications(ctx context.Context, project ids.ProjectID) error {
	_, err := exec(ctx, t.tx, "finish applications", finishApplications, t.org.String(), project)
	return err
}

// LifecycleSubject is the subject of the lifecycle operation claim holds;
// false when its payload is not one.
func (s *Store) LifecycleSubject(ctx context.Context, claim Claim) (Subject, bool, error) {
	payload, ok, err := queryOpt(ctx, s.db, "read a lifecycle subject", selectPayload, pgx.RowTo[string],
		claim.ID, claim.Org.String())
	if err != nil || !ok {
		return Subject{}, false, err
	}
	var subject Subject
	if json.Unmarshal([]byte(payload), &subject) != nil {
		// As Rust's `serde_json::from_str(..).ok()`: an unreadable payload
		// has no subject.
		return Subject{}, false, nil //nolint:nilerr // see above
	}
	return subject, true, nil
}
