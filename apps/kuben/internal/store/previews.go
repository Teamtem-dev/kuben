package store

// Preview environments (M5.1, migration 0030): the per-project policy, the
// previews themselves and what a preview copies from its source
// environment; the port of repo/previews.rs. Whether an environment is an
// untrusted preview lives with the secrets (secrets_revisions.go).

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/source"
)

// previewProvider is the only provider previews come from so far.
const previewProvider = "github"

const (
	selectPreviewPolicy = "SELECT project_id, enabled, source_environment_id, ttl_hours, max_active, allow_forks, " +
		"updated_by, updated_at FROM preview_policies WHERE project_id = $1 AND org_id = $2"
	upsertPreviewPolicy = "INSERT INTO preview_policies " +
		"(project_id, org_id, enabled, source_environment_id, ttl_hours, max_active, allow_forks, updated_by, updated_at) " +
		"VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) " +
		"ON CONFLICT (project_id) DO UPDATE SET enabled = EXCLUDED.enabled, " +
		"source_environment_id = EXCLUDED.source_environment_id, ttl_hours = EXCLUDED.ttl_hours, " +
		"max_active = EXCLUDED.max_active, allow_forks = EXCLUDED.allow_forks, " +
		"updated_by = EXCLUDED.updated_by, updated_at = EXCLUDED.updated_at"

	// selectPreviewColumns is Rust's previews! macro: the previews, with a
	// filter appended.
	selectPreviewColumns = "SELECT p.environment_id, p.project_id, e.slug AS environment, p.provider, p.installation_id, " +
		"p.repository, p.pr_number, p.preview_epoch, p.head_repository, p.branch, p.commit_sha, " +
		"p.trusted, p.state, p.auto_delete, p.expires_at, p.last_event_at, p.last_verified_at, " +
		"p.created_by, p.created_at, p.updated_at, p.closed_at, p.close_reason " +
		"FROM previews p JOIN environments e ON e.id = p.environment_id AND e.org_id = p.org_id "
	selectPreviews = selectPreviewColumns +
		"WHERE p.project_id = $1 AND p.org_id = $2 AND ($3 OR p.state = 'active') ORDER BY p.created_at DESC"
	selectPreviewOf = selectPreviewColumns + "WHERE p.environment_id = $1 AND p.org_id = $2"
	selectActiveFor = selectPreviewColumns +
		"WHERE p.project_id = $1 AND p.org_id = $2 AND p.provider = $3 AND p.repository = $4 " +
		"AND p.pr_number = $5 AND p.state = 'active' FOR UPDATE OF p"
	selectDuePreviews = selectPreviewColumns +
		"WHERE p.org_id = $1 AND p.state = 'active' AND p.auto_delete AND p.expires_at <= $2 " +
		"ORDER BY p.expires_at LIMIT $3 FOR UPDATE OF p SKIP LOCKED"
	selectUnverifiedPreviews = selectPreviewColumns +
		"WHERE p.org_id = $1 AND p.state = 'active' AND p.last_verified_at < $2 " +
		"ORDER BY p.last_verified_at LIMIT $3"

	nextPreviewEpoch = "SELECT coalesce(max(preview_epoch), 0) + 1 FROM previews " +
		"WHERE org_id = $1 AND project_id = $2 AND provider = $3 AND repository = $4 AND pr_number = $5"
	lastClosedPreview = "SELECT max(last_event_at) FROM previews " +
		"WHERE org_id = $1 AND project_id = $2 AND provider = $3 AND repository = $4 AND pr_number = $5 " +
		"AND state = 'closed'"
	activePreviewCount = "SELECT count(*) FROM previews WHERE org_id = $1 AND project_id = $2 AND state = 'active'"
	insertPreview      = "INSERT INTO previews " +
		"(environment_id, org_id, project_id, provider, installation_id, repository, pr_number, preview_epoch, " +
		"head_repository, branch, commit_sha, trusted, expires_at, last_event_at, last_verified_at, " +
		"created_by, created_at, updated_at) " +
		"VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $15, $15)"
	touchPreview = "UPDATE previews SET head_repository = $3, branch = $4, commit_sha = $5, " +
		"last_event_at = $6, expires_at = GREATEST(expires_at, $7), last_verified_at = $8, updated_at = $8 " +
		"WHERE environment_id = $1 AND org_id = $2 AND state = 'active' AND last_event_at <= $6"
	extendPreview = "UPDATE previews SET expires_at = $3, auto_delete = $4, updated_at = $5 " +
		"WHERE environment_id = $1 AND org_id = $2 AND state = 'active'"
	verifiedPreview = "UPDATE previews SET last_verified_at = $3 " +
		"WHERE environment_id = $1 AND org_id = $2 AND state = 'active'"
	selectPreviewSources = "SELECT t.id AS target_id, a.id AS application_id, a.slug, c.config::text AS config, " +
		"b.recipe::text AS recipe, b.image_repository " +
		"FROM source_bindings b " +
		"JOIN application_targets t ON t.id = b.target_id AND t.org_id = b.org_id " +
		"JOIN environment_placements pl ON pl.id = t.placement_id AND pl.org_id = t.org_id " +
		"JOIN applications a ON a.id = t.application_id AND a.org_id = t.org_id " +
		"JOIN LATERAL (SELECT config FROM target_config_revisions r " +
		"WHERE r.target_id = t.id AND r.org_id = t.org_id ORDER BY r.revision DESC LIMIT 1) c ON TRUE " +
		"WHERE b.org_id = $1 AND pl.environment_id = $2 AND b.provider = 'github' AND b.installation_id = $3 " +
		"AND b.repository = $4 AND b.pull_request IS NULL AND t.deleted_at IS NULL AND NOT t.deleting " +
		"ORDER BY a.slug"
	selectPreviewProjects = "SELECT pp.project_id FROM preview_policies pp " +
		"JOIN projects pr ON pr.id = pp.project_id AND pr.org_id = pp.org_id " +
		"WHERE pp.org_id = $1 AND pp.enabled AND pr.deleted_at IS NULL AND NOT pr.deleting " +
		"AND EXISTS (SELECT 1 FROM source_bindings b " +
		"JOIN application_targets t ON t.id = b.target_id AND t.org_id = b.org_id " +
		"JOIN environment_placements pl ON pl.id = t.placement_id AND pl.org_id = t.org_id " +
		"WHERE b.org_id = pp.org_id AND pl.environment_id = pp.source_environment_id " +
		"AND b.installation_id = $2 AND b.repository = $3 AND b.pull_request IS NULL) " +
		"ORDER BY pp.project_id"
	selectPreviewTargets = "SELECT t.id FROM application_targets t " +
		"JOIN environment_placements pl ON pl.id = t.placement_id AND pl.org_id = t.org_id " +
		"WHERE pl.environment_id = $1 AND t.org_id = $2 AND t.deleted_at IS NULL AND NOT t.deleting"
	countActivePreviews = "SELECT count(*) FROM previews WHERE org_id = $1 AND state = 'active'"
)

// PreviewPolicy is a project's preview settings.
type PreviewPolicy struct {
	Project           ids.ProjectID
	Enabled           bool
	SourceEnvironment ids.EnvironmentID
	TTLHours          uint32
	MaxActive         uint32
	AllowForks        bool
	UpdatedBy         string
	UpdatedAt         int64
}

// PreviewRecord is a preview environment as stored (Rust: Preview).
type PreviewRecord struct {
	EnvironmentID ids.EnvironmentID
	ProjectID     ids.ProjectID
	// Environment is the environment's name (`pr<n>-<epoch>`).
	Environment    string
	Provider       string
	InstallationID int64
	Repository     string
	PRNumber       int64
	PreviewEpoch   int64
	HeadRepository string
	Branch         string
	CommitSha      string
	Trusted        bool
	// State is `active` or `closed`.
	State          string
	AutoDelete     bool
	ExpiresAt      int64
	LastEventAt    int64
	LastVerifiedAt int64
	CreatedBy      string
	CreatedAt      int64
	UpdatedAt      int64
	ClosedAt       opt.Val[int64]
	CloseReason    opt.Val[string]
}

// Active reports whether the preview is still open.
func (p PreviewRecord) Active() bool { return p.State == "active" }

// NewPreview is a preview to record.
type NewPreview struct {
	Environment    ids.EnvironmentID
	Project        ids.ProjectID
	InstallationID uint64
	Repository     source.RepoName
	Number         uint64
	Epoch          uint64
	HeadRepository string
	Branch         string
	Commit         source.CommitSha
	Trusted        bool
	ExpiresAt      int64
	EventAt        int64
	CreatedBy      string
}

// PreviewSource is an app of a source environment a preview copies.
type PreviewSource struct {
	Target      ids.TargetID
	Application ids.ApplicationID
	Slug        string
	// Config is the newest configuration of the source app, decoded.
	Config          any
	Recipe          source.BuildRecipe
	ImageRepository string
}

func scanPreviewPolicy(row pgx.CollectableRow) (PreviewPolicy, error) {
	const op = "read a preview policy"
	var (
		p              PreviewPolicy
		ttl, maxActive int32
	)
	if err := row.Scan(&p.Project, &p.Enabled, &p.SourceEnvironment, &ttl, &maxActive, &p.AllowForks,
		&p.UpdatedBy, &p.UpdatedAt); err != nil {
		return PreviewPolicy{}, err
	}
	// Rust: u32::try_from(i32), a decode error when negative.
	if ttl < 0 || maxActive < 0 {
		return PreviewPolicy{}, decodeErr(op, "%s", errOutOfRange)
	}
	p.TTLHours, p.MaxActive = uint32(ttl), uint32(maxActive)
	return p, nil
}

func scanPreview(row pgx.CollectableRow) (PreviewRecord, error) {
	var (
		p           PreviewRecord
		closedAt    *int64
		closeReason *string
	)
	if err := row.Scan(&p.EnvironmentID, &p.ProjectID, &p.Environment, &p.Provider, &p.InstallationID,
		&p.Repository, &p.PRNumber, &p.PreviewEpoch, &p.HeadRepository, &p.Branch, &p.CommitSha,
		&p.Trusted, &p.State, &p.AutoDelete, &p.ExpiresAt, &p.LastEventAt, &p.LastVerifiedAt,
		&p.CreatedBy, &p.CreatedAt, &p.UpdatedAt, &closedAt, &closeReason); err != nil {
		return PreviewRecord{}, err
	}
	p.ClosedAt = opt.FromPtr(closedAt)
	p.CloseReason = opt.FromPtr(closeReason)
	return p, nil
}

func scanPreviewSource(row pgx.CollectableRow) (PreviewSource, error) {
	const op = "read a preview source"
	var (
		s              PreviewSource
		config, recipe string
	)
	if err := row.Scan(&s.Target, &s.Application, &s.Slug, &config, &recipe, &s.ImageRepository); err != nil {
		return PreviewSource{}, err
	}
	var err error
	if s.Config, err = jsonValue(op, config); err != nil {
		return PreviewSource{}, err
	}
	if s.Recipe, err = recipeOf(op, recipe); err != nil {
		return PreviewSource{}, err
	}
	return s, nil
}

// PreviewPolicy is project's preview settings, if set.
func (t *Tenant) PreviewPolicy(ctx context.Context, project ids.ProjectID) (PreviewPolicy, bool, error) {
	return queryOpt(ctx, t.tx, "read a preview policy", selectPreviewPolicy, scanPreviewPolicy, project, t.org.String())
}

// SetPreviewPolicy sets p as its project's preview settings.
func (t *Tenant) SetPreviewPolicy(ctx context.Context, p PreviewPolicy) error {
	const op = "set a preview policy"
	ttl, err := int4(op, p.TTLHours)
	if err != nil {
		return err
	}
	maxActive, err := int4(op, p.MaxActive)
	if err != nil {
		return err
	}
	_, err = exec(ctx, t.tx, op, upsertPreviewPolicy, p.Project, t.org.String(), p.Enabled, p.SourceEnvironment,
		ttl, maxActive, p.AllowForks, p.UpdatedBy, p.UpdatedAt)
	return err
}

// Previews is project's previews, newest first; closed ones only with all.
func (t *Tenant) Previews(ctx context.Context, project ids.ProjectID, all bool) ([]PreviewRecord, error) {
	return queryAll(ctx, t.tx, "list previews", selectPreviews, scanPreview, project, t.org.String(), all)
}

// PreviewOf is the preview environment is, if it is one.
func (t *Tenant) PreviewOf(ctx context.Context, environment ids.EnvironmentID) (PreviewRecord, bool, error) {
	return queryOpt(ctx, t.tx, "read a preview", selectPreviewOf, scanPreview, environment, t.org.String())
}

// ActivePreview is the active preview of pull request number, locked.
func (t *Tenant) ActivePreview(ctx context.Context, project ids.ProjectID, repository source.RepoName, number uint64) (PreviewRecord, bool, error) {
	const op = "read the active preview"
	n, err := signed(op, number)
	if err != nil {
		return PreviewRecord{}, false, err
	}
	return queryOpt(ctx, t.tx, op, selectActiveFor, scanPreview,
		project, t.org.String(), previewProvider, repository.String(), n)
}

// PreviewEpoch is the epoch a new preview of pull request number gets, and
// the newest event a closed one of it saw (an event not newer than that
// must not bring the preview back).
func (t *Tenant) PreviewEpoch(ctx context.Context, project ids.ProjectID, repository source.RepoName, number uint64) (uint64, opt.Val[int64], error) {
	const op = "read a preview epoch"
	n, err := signed(op, number)
	if err != nil {
		return 0, opt.None[int64](), err
	}
	org := t.org.String()
	var epoch int64
	if err := queryOne(ctx, t.tx, op, nextPreviewEpoch, []any{&epoch},
		org, project, previewProvider, repository.String(), n); err != nil {
		return 0, opt.None[int64](), err
	}
	var closed *int64
	if err := queryOne(ctx, t.tx, op, lastClosedPreview, []any{&closed},
		org, project, previewProvider, repository.String(), n); err != nil {
		return 0, opt.None[int64](), err
	}
	next, err := counter(op, epoch)
	if err != nil {
		return 0, opt.None[int64](), err
	}
	return next, opt.FromPtr(closed), nil
}

// ActivePreviewCount is the number of active previews of project.
func (t *Tenant) ActivePreviewCount(ctx context.Context, project ids.ProjectID) (uint64, error) {
	const op = "count active previews"
	var n int64
	if err := queryOne(ctx, t.tx, op, activePreviewCount, []any{&n}, t.org.String(), project); err != nil {
		return 0, err
	}
	return counter(op, n)
}

// InsertPreview records a new preview (its environment exists in this
// transaction).
func (t *Tenant) InsertPreview(ctx context.Context, p NewPreview) error {
	const op = "record a preview"
	installation, err := signed(op, p.InstallationID)
	if err != nil {
		return err
	}
	number, err := signed(op, p.Number)
	if err != nil {
		return err
	}
	epoch, err := signed(op, p.Epoch)
	if err != nil {
		return err
	}
	_, err = exec(ctx, t.tx, op, insertPreview,
		p.Environment, t.org.String(), p.Project, previewProvider, installation, p.Repository.String(), number, epoch,
		p.HeadRepository, p.Branch, p.Commit.String(), p.Trusted, p.ExpiresAt, p.EventAt, t.store.now(), p.CreatedBy)
	return err
}

// TouchPreview records a newer head of an active preview and keeps it alive
// until at least expiresAt. False for an event older than the newest seen.
func (t *Tenant) TouchPreview(
	ctx context.Context, environment ids.EnvironmentID, headRepository, branch string, commit source.CommitSha,
	eventAt, expiresAt int64,
) (bool, error) {
	n, err := exec(ctx, t.tx, "touch a preview", touchPreview, environment, t.org.String(), headRepository, branch,
		commit.String(), eventAt, expiresAt, t.store.now())
	return n == 1, err
}

// ExtendPreview sets an active preview's expiry and whether it is deleted
// then; false when it is not active.
func (t *Tenant) ExtendPreview(ctx context.Context, environment ids.EnvironmentID, expiresAt int64, autoDelete bool) (bool, error) {
	n, err := exec(ctx, t.tx, "extend a preview", extendPreview, environment, t.org.String(), expiresAt, autoDelete,
		t.store.now())
	return n == 1, err
}

// PreviewVerified records that the provider confirmed the preview's pull
// request open at at.
func (t *Tenant) PreviewVerified(ctx context.Context, environment ids.EnvironmentID, at int64) error {
	_, err := exec(ctx, t.tx, "confirm a preview", verifiedPreview, environment, t.org.String(), at)
	return err
}

// DuePreviews is the active previews that expired by now, at most limit,
// locked for this transaction.
func (t *Tenant) DuePreviews(ctx context.Context, now, limit int64) ([]PreviewRecord, error) {
	return queryAll(ctx, t.tx, "list expired previews", selectDuePreviews, scanPreview, t.org.String(), now, limit)
}

// UnverifiedPreviews is the active previews whose pull request was not
// confirmed open since before, at most limit.
func (t *Tenant) UnverifiedPreviews(ctx context.Context, before, limit int64) ([]PreviewRecord, error) {
	return queryAll(ctx, t.tx, "list unconfirmed previews", selectUnverifiedPreviews, scanPreview,
		t.org.String(), before, limit)
}

// PreviewSources is the apps of environment that build from repository
// through installationID: what a preview of it copies.
func (t *Tenant) PreviewSources(
	ctx context.Context, environment ids.EnvironmentID, installationID uint64, repository source.RepoName,
) ([]PreviewSource, error) {
	const op = "list preview sources"
	installation, err := signed(op, installationID)
	if err != nil {
		return nil, err
	}
	return queryAll(ctx, t.tx, op, selectPreviewSources, scanPreviewSource,
		t.org.String(), environment, installation, repository.String())
}

// PreviewProjects is the projects with previews on whose source
// environment builds from repository through installationID.
func (t *Tenant) PreviewProjects(ctx context.Context, installationID uint64, repository source.RepoName) ([]ids.ProjectID, error) {
	const op = "list preview projects"
	installation, err := signed(op, installationID)
	if err != nil {
		return nil, err
	}
	return queryAll(ctx, t.tx, op, selectPreviewProjects, scanID[ids.Project],
		t.org.String(), installation, repository.String())
}

// PreviewTargets is the live apps of a preview environment.
func (t *Tenant) PreviewTargets(ctx context.Context, environment ids.EnvironmentID) ([]ids.TargetID, error) {
	return queryAll(ctx, t.tx, "list a preview's apps", selectPreviewTargets, scanID[ids.Target],
		environment, t.org.String())
}

// PreviewOrgs is the organizations with an active preview, for the
// janitor. `previews` is tenant-scoped, so every organization is asked.
func (s *Store) PreviewOrgs(ctx context.Context) ([]ids.OrgID, error) {
	orgs, err := s.OrgIDs(ctx)
	if err != nil {
		return nil, err
	}
	out := []ids.OrgID{}
	for _, org := range orgs {
		n, err := s.activePreviews(ctx, org)
		if err != nil {
			return nil, err
		}
		if n > 0 {
			out = append(out, org)
		}
	}
	return out, nil
}

func (s *Store) activePreviews(ctx context.Context, org ids.OrgID) (int64, error) {
	t, err := s.Tenant(ctx, org)
	if err != nil {
		return 0, err
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	var n int64
	err = queryOne(ctx, t.tx, "count active previews", countActivePreviews, []any{&n}, org.String())
	return n, err
}

const untrustedTarget = "SELECT EXISTS (SELECT 1 FROM application_targets t " +
	"JOIN environment_placements pl ON pl.id = t.placement_id AND pl.org_id = t.org_id " +
	"JOIN previews p ON p.environment_id = pl.environment_id AND p.org_id = t.org_id " +
	"WHERE t.id = $1 AND t.org_id = $2 AND NOT p.trusted)"

// UntrustedTarget reports whether tgt is an app of an untrusted preview.
func (t *Tenant) UntrustedTarget(ctx context.Context, tgt ids.TargetID) (bool, error) {
	var untrusted bool
	err := queryOne(ctx, t.tx, "check a preview's trust", untrustedTarget, []any{&untrusted}, tgt, t.org.String())
	return untrusted, err
}

// CloseReason is why a preview closed.
type CloseReason string

// The close reasons, with their stored names.
const (
	// CloseClosed: the pull request was closed or merged.
	CloseClosed CloseReason = "closed"
	// CloseExpired: its lifetime ran out.
	CloseExpired CloseReason = "expired"
	// CloseManual: someone destroyed it.
	CloseManual CloseReason = "manual"
	// CloseDeleted: its environment was deleted.
	CloseDeleted CloseReason = "deleted"
)

const closePreview = "UPDATE previews SET state = 'closed', closed_at = $3, close_reason = $4, updated_at = $3, " +
	"last_event_at = GREATEST(last_event_at, $5) " +
	"WHERE environment_id = $1 AND org_id = $2 AND state = 'active'"

// ClosePreview closes the active preview of environment for reason; false
// when it has none.
func (t *Tenant) ClosePreview(ctx context.Context, environment ids.EnvironmentID, reason CloseReason, eventAt int64) (bool, error) {
	n, err := exec(ctx, t.tx, "close a preview", closePreview, environment, t.org.String(), t.store.now(),
		string(reason), eventAt)
	return n == 1, err
}
