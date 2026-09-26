package store

// Owners, freezes, pauses and silences (M4.9, migration 0027); the port of
// repo/controls.rs.

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
)

const (
	selectOwner = "SELECT owner, contact, runbook_url, updated_by, updated_at FROM owners " +
		"WHERE org_id = $1 AND project_id = $2 AND application_id IS NOT DISTINCT FROM $3"
	deleteOwner = "DELETE FROM owners " +
		"WHERE org_id = $1 AND project_id = $2 AND application_id IS NOT DISTINCT FROM $3"
	insertOwner = "INSERT INTO owners " +
		"(org_id, project_id, application_id, owner, contact, runbook_url, updated_by, updated_at) " +
		"VALUES ($1, $2, $3, $4, $5, $6, $7, $8)"
	targetEnvironment = "SELECT p.environment_id FROM application_targets t " +
		"JOIN environment_placements p ON p.id = t.placement_id AND p.org_id = t.org_id " +
		"WHERE t.id = $1 AND t.org_id = $2"
	activeFreeze = "SELECT reason FROM environment_freezes " +
		"WHERE environment_id = $1 AND org_id = $2 AND lifted_at IS NULL AND starts_at <= $3 AND ends_at > $3 " +
		"ORDER BY ends_at DESC LIMIT 1"
	selectFreezes = "SELECT id, reason, created_by, created_at, starts_at, ends_at, lifted_at, lifted_by " +
		"FROM environment_freezes WHERE environment_id = $1 AND org_id = $2 " +
		"AND ($3 OR (lifted_at IS NULL AND ends_at > $4)) ORDER BY starts_at DESC LIMIT 200"
	insertFreeze = "INSERT INTO environment_freezes " +
		"(id, org_id, project_id, environment_id, reason, created_by, created_at, starts_at, ends_at) " +
		"VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)"
	liftFreeze = "UPDATE environment_freezes SET lifted_at = $4, lifted_by = $5 " +
		"WHERE id = $1 AND environment_id = $2 AND org_id = $3 AND lifted_at IS NULL AND ends_at > $4"
	selectSilences = "SELECT id, target_id, reason, created_by, created_at, ends_at, lifted_at, lifted_by " +
		"FROM silences WHERE environment_id = $1 AND org_id = $2 " +
		"AND ($3 OR (lifted_at IS NULL AND ends_at > $4)) ORDER BY created_at DESC LIMIT 200"
	insertSilence = "INSERT INTO silences " +
		"(id, org_id, project_id, environment_id, target_id, reason, created_by, created_at, ends_at) " +
		"VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)"
	liftSilence = "UPDATE silences SET lifted_at = $4, lifted_by = $5 " +
		"WHERE id = $1 AND environment_id = $2 AND org_id = $3 AND lifted_at IS NULL AND ends_at > $4"
	selectSilenced = "SELECT EXISTS (SELECT 1 FROM silences " +
		"WHERE environment_id = $1 AND org_id = $2 AND lifted_at IS NULL AND ends_at > $4 " +
		"AND (target_id IS NULL OR target_id = $3))"
	pauseTarget = "UPDATE application_targets SET paused_at = $3, paused_by = $4, pause_reason = $5 " +
		"WHERE id = $1 AND org_id = $2 AND paused_at IS NULL AND NOT deleting"
	resumeTarget = "UPDATE application_targets SET paused_at = NULL, paused_by = NULL, pause_reason = NULL " +
		"WHERE id = $1 AND org_id = $2 AND paused_at IS NOT NULL"
	wakeTarget = "UPDATE operations SET next_attempt_at = kuben_now_ms() " +
		"WHERE target_id = $1 AND NOT done AND next_attempt_at > kuben_now_ms()"
	// rollbackPoint is the newest successful run of $1 with release $3, or,
	// without one, of a release other than the one its newest run carries.
	rollbackPoint = "SELECT d.release_id, d.config_revision_id FROM deployment_runs d " +
		"WHERE d.target_id = $1 AND d.org_id = $2 AND d.phase = 'succeeded' " +
		"AND (($3::uuid IS NULL AND d.release_id IS DISTINCT FROM (SELECT n.release_id FROM deployment_runs n " +
		"WHERE n.target_id = $1 AND n.org_id = $2 ORDER BY n.generation DESC LIMIT 1)) " +
		"OR d.release_id = $3::uuid) " +
		"ORDER BY d.generation DESC LIMIT 1"
	paused = "SELECT paused_at IS NOT NULL FROM application_targets WHERE id = $1 AND org_id = $2"
)

// Owner is who answers for a project or an application.
type Owner struct {
	Owner      string
	Contact    opt.Val[string]
	RunbookURL opt.Val[string]
	UpdatedBy  string
	UpdatedAt  int64
}

// NewOwner is an owner to set.
type NewOwner struct {
	Owner      string
	Contact    opt.Val[string]
	RunbookURL opt.Val[string]
}

// Freeze is a change freeze of an environment.
type Freeze struct {
	ID        uuid.UUID
	Reason    string
	CreatedBy string
	CreatedAt int64
	StartsAt  int64
	EndsAt    int64
	LiftedAt  opt.Val[int64]
	LiftedBy  opt.Val[string]
}

// Silence is a silence of an environment's alerts, or one app's.
type Silence struct {
	ID        uuid.UUID
	Target    opt.Val[ids.TargetID]
	Reason    string
	CreatedBy string
	CreatedAt int64
	EndsAt    int64
	LiftedAt  opt.Val[int64]
	LiftedBy  opt.Val[string]
}

// NewWindow is a freeze or silence to make.
type NewWindow struct {
	Project     ids.ProjectID
	Environment ids.EnvironmentID
	// Target: a silence of one app only.
	Target    opt.Val[ids.TargetID]
	Reason    string
	CreatedBy string
	StartsAt  int64
	EndsAt    int64
}

// idArg is an optional id as a query argument (NULL when absent).
func idArg[K ids.Kind](v opt.Val[ids.ID[K]]) any {
	if id, ok := v.Get(); ok {
		return id
	}
	return nil
}

// Owner is the owner of project, or of its application.
func (t *Tenant) Owner(ctx context.Context, project ids.ProjectID, application opt.Val[ids.ApplicationID]) (Owner, bool, error) {
	return queryOpt(ctx, t.tx, "read an owner", selectOwner, func(r pgx.CollectableRow) (Owner, error) {
		var (
			o                Owner
			contact, runbook *string
		)
		err := r.Scan(&o.Owner, &contact, &runbook, &o.UpdatedBy, &o.UpdatedAt)
		o.Contact, o.RunbookURL = opt.FromPtr(contact), opt.FromPtr(runbook)
		return o, err
	}, t.org.String(), project, idArg(application))
}

// SetOwner sets (or, with none, clears) the owner of project or its
// application.
func (t *Tenant) SetOwner(ctx context.Context, project ids.ProjectID, application opt.Val[ids.ApplicationID], owner opt.Val[NewOwner], by string) error {
	const op = "set an owner"
	org := t.org.String()
	if _, err := exec(ctx, t.tx, op, deleteOwner, org, project, idArg(application)); err != nil {
		return err
	}
	o, ok := owner.Get()
	if !ok {
		return nil
	}
	_, err := exec(ctx, t.tx, op, insertOwner, org, project, idArg(application), o.Owner, o.Contact.Ptr(), o.RunbookURL.Ptr(), by, t.store.now())
	return err
}

func (t *Tenant) environmentOf(ctx context.Context, tgt ids.TargetID) (ids.EnvironmentID, bool, error) {
	return queryOpt(ctx, t.tx, "find a target's environment", targetEnvironment, scanID[ids.Environment],
		tgt, t.org.String())
}

// ActiveFreeze is the reason of the freeze on tgt's environment at now, if
// there is one.
func (t *Tenant) ActiveFreeze(ctx context.Context, tgt ids.TargetID, now int64) (string, bool, error) {
	environment, ok, err := t.environmentOf(ctx, tgt)
	if err != nil || !ok {
		return "", false, err
	}
	return queryOpt(ctx, t.tx, "read the active freeze", activeFreeze, pgx.RowTo[string],
		environment, t.org.String(), now)
}

func scanFreeze(r pgx.CollectableRow) (Freeze, error) {
	var (
		f        Freeze
		liftedAt *int64
		liftedBy *string
	)
	err := r.Scan(&f.ID, &f.Reason, &f.CreatedBy, &f.CreatedAt, &f.StartsAt, &f.EndsAt, &liftedAt, &liftedBy)
	f.LiftedAt, f.LiftedBy = opt.FromPtr(liftedAt), opt.FromPtr(liftedBy)
	return f, err
}

// Freezes are the freezes of environment in force or to come, or all of
// them.
func (t *Tenant) Freezes(ctx context.Context, environment ids.EnvironmentID, all bool) ([]Freeze, error) {
	return queryAll(ctx, t.tx, "read the freezes", selectFreezes, scanFreeze, environment, t.org.String(), all, t.store.now())
}

// CreateFreeze freezes an environment for the window of w.
func (t *Tenant) CreateFreeze(ctx context.Context, w NewWindow) (uuid.UUID, error) {
	const op = "create a freeze"
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.UUID{}, fmt.Errorf("%s: %w", op, err)
	}
	_, err = exec(ctx, t.tx, op, insertFreeze, id, t.org.String(), w.Project, w.Environment, w.Reason, w.CreatedBy,
		t.store.now(), w.StartsAt, w.EndsAt)
	return id, err
}

// LiftFreeze lifts freeze id of environment early; false when it is not in
// force or to come.
func (t *Tenant) LiftFreeze(ctx context.Context, environment ids.EnvironmentID, id uuid.UUID, by string) (bool, error) {
	return t.lift(ctx, "lift a freeze", liftFreeze, environment, id, by)
}

func (t *Tenant) lift(ctx context.Context, op, sql string, environment ids.EnvironmentID, id uuid.UUID, by string) (bool, error) {
	n, err := exec(ctx, t.tx, op, sql, id, environment, t.org.String(), t.store.now(), by)
	return n == 1, err
}

func scanSilence(r pgx.CollectableRow) (Silence, error) {
	var (
		s        Silence
		target   *uuid.UUID
		liftedAt *int64
		liftedBy *string
	)
	err := r.Scan(&s.ID, &target, &s.Reason, &s.CreatedBy, &s.CreatedAt, &s.EndsAt, &liftedAt, &liftedBy)
	if target != nil {
		s.Target = opt.Some(ids.From[ids.Target](*target))
	}
	s.LiftedAt, s.LiftedBy = opt.FromPtr(liftedAt), opt.FromPtr(liftedBy)
	return s, err
}

// Silences are the silences of environment in force, or all of them.
func (t *Tenant) Silences(ctx context.Context, environment ids.EnvironmentID, all bool) ([]Silence, error) {
	return queryAll(ctx, t.tx, "read the silences", selectSilences, scanSilence, environment, t.org.String(), all, t.store.now())
}

// CreateSilence silences an environment's alerts, or one app's, until
// w.EndsAt.
func (t *Tenant) CreateSilence(ctx context.Context, w NewWindow) (uuid.UUID, error) {
	const op = "create a silence"
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.UUID{}, fmt.Errorf("%s: %w", op, err)
	}
	_, err = exec(ctx, t.tx, op, insertSilence, id, t.org.String(), w.Project, w.Environment, idArg(w.Target), w.Reason,
		w.CreatedBy, t.store.now(), w.EndsAt)
	return id, err
}

// LiftSilence lifts silence id of environment early.
func (t *Tenant) LiftSilence(ctx context.Context, environment ids.EnvironmentID, id uuid.UUID, by string) (bool, error) {
	return t.lift(ctx, "lift a silence", liftSilence, environment, id, by)
}

// Silenced is whether alerts about target (or the whole environment) are
// silenced at now.
func (t *Tenant) Silenced(ctx context.Context, environment ids.EnvironmentID, target opt.Val[ids.TargetID], now int64) (bool, error) {
	var silenced bool
	err := t.tx.QueryRow(ctx, selectSilenced, environment, t.org.String(), idArg(target), now).Scan(&silenced)
	return silenced, dbErr("read a silence", err)
}

// PauseTarget holds the delivery of tgt; false when it is paused already,
// being deleted, or not the organization's.
func (t *Tenant) PauseTarget(ctx context.Context, tgt ids.TargetID, by, reason string) (bool, error) {
	n, err := exec(ctx, t.tx, "pause a target", pauseTarget, tgt, t.org.String(), t.store.now(), by, reason)
	return n == 1, err
}

// ResumeTarget resumes the delivery of tgt; its waiting runs are woken, and
// only the newest of them writes (older ones are superseded). False when it
// was not paused.
func (t *Tenant) ResumeTarget(ctx context.Context, tgt ids.TargetID) (bool, error) {
	const op = "resume a target"
	n, err := exec(ctx, t.tx, op, resumeTarget, tgt, t.org.String())
	if err != nil || n != 1 {
		return false, err
	}
	if _, err := exec(ctx, t.tx, op, wakeTarget, tgt); err != nil {
		return false, err
	}
	return true, nil
}

// RollbackPoint is the release and configuration an emergency rollback of
// tgt returns to: release as it last ran successfully, or the newest
// successful release other than the current one.
func (t *Tenant) RollbackPoint(ctx context.Context, tgt ids.TargetID, release opt.Val[ids.ReleaseID]) (ids.ReleaseID, ids.ConfigRevisionID, bool, error) {
	type point struct {
		release ids.ReleaseID
		config  ids.ConfigRevisionID
	}
	p, ok, err := queryOpt(ctx, t.tx, "find a rollback point", rollbackPoint, func(r pgx.CollectableRow) (point, error) {
		var p point
		return p, r.Scan(&p.release, &p.config)
	}, tgt, t.org.String(), idArg(release))
	return p.release, p.config, ok, err
}

// TargetPaused is whether the delivery of tgt is held now; false for a
// target that does not exist.
func (t *Tenant) TargetPaused(ctx context.Context, tgt ids.TargetID) (bool, error) {
	held, _, err := queryOpt(ctx, t.tx, "read a target's pause", paused, pgx.RowTo[bool], tgt, t.org.String())
	return held, err
}
