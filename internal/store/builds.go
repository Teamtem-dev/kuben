package store

// Git sources and build attempts (M3; ADR-028, migration 0018); the port of
// repo/builds.rs.
//
// The flow, every step a durable operation:
//
//	webhook / API ──► source.sync ──(provider head read)──► observe head
//	                                                        ├─ same head: nothing
//	                                                        └─ new head: epoch+1, older builds asked to
//	                                                           stop, build operation + attempt queued
//	build ──► claim build slot ──► advance build (Job phases) ──► complete build
//	                                                              ├─ release (digest verified)
//	                                                              └─ CAS on epoch/config/policy → run
//
// Every write a worker makes is conditional on the fence of its claim and
// runs in a [Tenant] transaction, so row-level security applies to the
// worker too.

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strconv"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/internal/core/artifact"
	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/ops"
	"github.com/Teamtem-dev/kuben/internal/core/ops/build"
	"github.com/Teamtem-dev/kuben/internal/core/ops/target"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/source"
)

// GitHub is the provider name of GitHub App installations and deliveries.
const GitHub = "github"

// SourceSyncKind is the operation kind that reads a binding's branch head
// from the provider.
const SourceSyncKind = "source.sync"

// BuildKind is the operation kind of one build attempt.
const BuildKind = "build"

// BuildProcess is the process a build's image becomes in its release.
const BuildProcess = "web"

// MaxBuildAttempts is how many infrastructure retries one build gets (a
// lost worker), counting the first.
const MaxBuildAttempts uint32 = 3

const (
	syncTopic  = "source.sync.requested"
	buildTopic = "build.queued"
	slotLock   = int64(0x6b75_6265_6e62_6c64) // "kubenbld"
)

// The SQL of repo/builds.rs. Rust assembled the SELECTs of bindings and
// attempts with format! over the column lists and a filter and passed them
// through sqlx::AssertSqlSafe: every piece was a &'static str of the module,
// nothing from a caller. Here the same text is a constant expression, so the
// compiler proves what AssertSqlSafe asserted; values are always $n
// parameters.
const (
	upsertInstallation = "INSERT INTO git_installations " +
		"(provider, installation_id, org_id, account, suspended, created_at, updated_at) " +
		"VALUES ($1, $2, $3, $4, FALSE, $5, $5) " +
		"ON CONFLICT (provider, installation_id) DO UPDATE " +
		"SET account = EXCLUDED.account, updated_at = EXCLUDED.updated_at " +
		"WHERE git_installations.org_id = EXCLUDED.org_id " +
		"RETURNING org_id"
	selectInstallations = "SELECT installation_id, account, suspended FROM git_installations " +
		"WHERE provider = $1 AND org_id = $2 ORDER BY installation_id"
	gitInstallationOrg = "SELECT org_id, suspended FROM git_installations WHERE provider = $1 AND installation_id = $2"
	setSuspended       = "UPDATE git_installations SET suspended = $3, updated_at = $4 " +
		"WHERE provider = $1 AND installation_id = $2"
	ownsInstallation = "SELECT EXISTS (SELECT 1 FROM git_installations " +
		"WHERE provider = $1 AND installation_id = $2 AND org_id = $3)"
	lockBindingTarget = "SELECT application_id, build_config_revision, deleting FROM application_targets " +
		"WHERE id = $1 AND org_id = $2 AND project_id = $3 FOR UPDATE"
	bindingColumns = "b.id, b.org_id, b.project_id, b.application_id, b.target_id, " +
		"b.installation_id, b.repository, b.repository_id, b.branch, b.recipe::text AS recipe, " +
		"b.image_repository, b.head_sha, b.head_epoch, b.pull_request"
	selectBindingOfTarget = "SELECT " + bindingColumns +
		" FROM source_bindings b WHERE b.target_id = $1 AND b.org_id = $2"
	selectBinding = "SELECT " + bindingColumns +
		" FROM source_bindings b WHERE b.id = $1 AND b.org_id = $2"
	selectBindingsForPush = "SELECT " + bindingColumns + " FROM source_bindings b " +
		"WHERE b.provider = $1 AND b.installation_id = $2 AND b.repository = $3 AND b.branch = $4 " +
		"AND b.org_id = $5 AND b.pull_request IS NULL ORDER BY b.id"
	insertSourceBinding = "INSERT INTO source_bindings " +
		"(id, org_id, project_id, application_id, target_id, provider, installation_id, repository, " +
		"branch, recipe, image_repository, pull_request, created_at, updated_at) " +
		"VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11, $13, $12, $12)"
	updateBinding = "UPDATE source_bindings " +
		"SET installation_id = $2, repository = $3, branch = $4, recipe = $5::jsonb, image_repository = $6, " +
		"repository_id = CASE WHEN repository = $3 THEN repository_id END, " +
		"head_sha = NULL, updated_at = $7, pull_request = $8 " +
		"WHERE id = $1"
	raiseBuildConfig = "UPDATE application_targets SET build_config_revision = build_config_revision + 1 WHERE id = $1"
	pendingSync      = "SELECT id FROM operations " +
		"WHERE kind = $1 AND org_id = $2 AND target_id = $3 AND NOT done ORDER BY requested_at LIMIT 1"
	lockFence = "SELECT payload::text FROM operations " +
		"WHERE id = $1 AND fence = $2 AND NOT done AND org_id = $3 FOR UPDATE"
	lockBindingAndTarget = "SELECT b.head_sha, b.head_epoch, b.repository_id, " +
		"t.source_epoch, t.build_config_revision, t.lifecycle_uid, t.deleting " +
		"FROM source_bindings b JOIN application_targets t ON t.id = b.target_id " +
		"WHERE b.id = $1 FOR UPDATE OF b, t"
	recordHead = "UPDATE source_bindings " +
		"SET head_sha = $2, head_epoch = $3, repository_id = COALESCE(repository_id, $4), updated_at = $5 " +
		"WHERE id = $1"
	raiseEpoch     = "UPDATE application_targets SET source_epoch = $2 WHERE id = $1 AND source_epoch = $2 - 1"
	askOlderToStop = "UPDATE build_attempts " +
		"SET cancel_requested_at = COALESCE(cancel_requested_at, $2), updated_at = $2 " +
		"WHERE binding_id = $1 AND phase <> ALL($3) RETURNING operation_id"
	wakeOperations = "UPDATE operations SET next_attempt_at = kuben_now_ms() WHERE id = ANY($1) AND NOT done"
	insertAttempt  = "INSERT INTO build_attempts " +
		"(id, org_id, project_id, application_id, target_id, binding_id, attempt_no, commit_sha, source_epoch, " +
		"build_config_revision, lifecycle_uid, repository, recipe, image_repository, operation_id, " +
		"created_at, updated_at) " +
		"VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13::jsonb, $14, $15, $16, $16)"
	attemptColumns = "a.id, a.org_id, a.project_id, a.application_id, a.target_id, a.binding_id, " +
		"a.attempt_no, a.commit_sha, a.source_epoch, a.build_config_revision, a.lifecycle_uid, a.repository, " +
		"a.recipe::text AS recipe, a.image_repository, a.phase, a.blocked_reason, a.failure, a.failure_detail, " +
		"a.reported_digest, a.digest, a.job_name, a.release_id, a.deployment_run_id, a.deploy_decision, " +
		"a.operation_id, a.cancel_requested_at, a.created_at, a.started_at, a.finished_at, " +
		"b.installation_id, b.branch"
	attemptsFrom = "SELECT " + attemptColumns +
		" FROM build_attempts a JOIN source_bindings b ON b.id = a.binding_id WHERE "
	// attemptByOperation and attemptByID are Rust's attempt_where with its
	// two filters.
	attemptByOperation = attemptsFrom + "a.operation_id = $1" + " AND a.org_id = $2"
	attemptByID        = attemptsFrom + "a.id = $1" + " AND a.org_id = $2"
	attemptsOfTarget   = attemptsFrom +
		"a.target_id = $1 AND a.org_id = $2 ORDER BY a.created_at DESC, a.id DESC LIMIT $3"
	lockAttemptUnderFence = "SELECT a.phase FROM build_attempts a " +
		"JOIN operations o ON o.id = a.operation_id " +
		"WHERE a.id = $1 AND o.id = $2 AND o.fence = $3 AND NOT o.done " +
		"FOR UPDATE OF a"
	setAttempt = "UPDATE build_attempts SET phase = $2, updated_at = $3, " +
		"job_name = COALESCE(job_name, $4), reported_digest = COALESCE(reported_digest, $5), " +
		"failure = COALESCE(failure, $6), failure_detail = COALESCE(failure_detail, $7), " +
		"blocked_reason = $8, digest = COALESCE(digest, $9), " +
		"started_at = CASE WHEN $2 = 'preparing' THEN COALESCE(started_at, $3) ELSE started_at END, " +
		"finished_at = CASE WHEN $2 = ANY($10) THEN $3 ELSE finished_at END " +
		"WHERE id = $1"
	releaseSlot          = "DELETE FROM build_slots WHERE attempt_id = $1"
	hasSlot              = "SELECT EXISTS (SELECT 1 FROM build_slots WHERE attempt_id = $1)"
	slotsInUse           = "SELECT count(*), count(*) FILTER (WHERE org_id = $1) FROM build_slots"
	insertSlot           = "INSERT INTO build_slots (attempt_id, org_id, claimed_at) VALUES ($1, $2, $3)"
	lockSlots            = "SELECT pg_advisory_xact_lock($1)"
	setOutput            = "UPDATE build_attempts SET release_id = $2, deployment_run_id = $3, deploy_decision = $4, updated_at = $5 WHERE id = $1"
	lockTargetGeneration = "SELECT desired_generation FROM application_targets WHERE id = $1 FOR UPDATE"
	requestCancel        = "UPDATE build_attempts " +
		"SET cancel_requested_at = COALESCE(cancel_requested_at, $3), updated_at = $3 " +
		"WHERE id = $1 AND target_id = $2 AND phase <> ALL($4) RETURNING operation_id"
	nextAttemptNo = "SELECT COALESCE(max(attempt_no), 0) + 1 FROM build_attempts " +
		"WHERE binding_id = $1 AND source_epoch = $2 AND build_config_revision = $3"
)

// terminalPhases are the final build phases, as the SQL arrays compare them.
func terminalPhases() []string {
	return []string{string(build.Succeeded), string(build.Failed), string(build.Cancelled)}
}

// NewBinding is a new or changed source binding of a target.
type NewBinding struct {
	InstallationID uint64
	Repository     source.RepoName
	Branch         source.BranchName
	Recipe         source.BuildRecipe
	// ImageRepository is where builds push, without tag or digest.
	ImageRepository string
	// PullRequest is followed instead of Branch (a preview, M5.1).
	PullRequest opt.Val[uint64]
}

// SourceBinding is a target's Git source.
type SourceBinding struct {
	ID             ids.SourceBindingID
	Org            ids.OrgID
	Project        ids.ProjectID
	Application    ids.ApplicationID
	Target         ids.TargetID
	InstallationID uint64
	Repository     source.RepoName
	// RepositoryID is the provider's id, pinned on the first verified read.
	RepositoryID    opt.Val[uint64]
	Branch          source.BranchName
	Recipe          source.BuildRecipe
	ImageRepository string
	// HeadSha is the last head read from the provider, and HeadEpoch the
	// source epoch it got.
	HeadSha   opt.Val[source.CommitSha]
	HeadEpoch target.SourceEpoch
	// PullRequest is the pull request a preview's binding follows (M5.1).
	PullRequest opt.Val[uint64]
}

// Bound is the outcome of [Tenant.BindSource].
//
//sumtype:decl
type Bound interface{ bound() }

type (
	// BoundCreated is a new binding.
	BoundCreated struct{ ID ids.SourceBindingID }
	// BoundChanged means the binding changed: the build configuration
	// revision was raised, so builds of the old binding no longer deploy
	// themselves.
	BoundChanged struct{ ID ids.SourceBindingID }
	// BoundUnchanged means the binding was already this one.
	BoundUnchanged struct{ ID ids.SourceBindingID }
	// BoundInstallationMissing means the installation is not linked to this
	// organization.
	BoundInstallationMissing struct{}
	// BoundNotFound means no such target, or it is being deleted.
	BoundNotFound struct{}
)

func (BoundCreated) bound()             {}
func (BoundChanged) bound()             {}
func (BoundUnchanged) bound()           {}
func (BoundInstallationMissing) bound() {}
func (BoundNotFound) bound()            {}

// BindingOf is the binding a [Bound] names, if it names one.
func BindingOf(b Bound) (ids.SourceBindingID, bool) {
	switch b := b.(type) {
	case BoundCreated:
		return b.ID, true
	case BoundChanged:
		return b.ID, true
	case BoundUnchanged:
		return b.ID, true
	case BoundInstallationMissing, BoundNotFound:
		return ids.SourceBindingID{}, false
	}
	return ids.SourceBindingID{}, false
}

// BuildAttempt is one build attempt with its binding's installation and
// branch.
type BuildAttempt struct {
	ID                  ids.BuildAttemptID
	Org                 ids.OrgID
	Project             ids.ProjectID
	Application         ids.ApplicationID
	Target              ids.TargetID
	Binding             ids.SourceBindingID
	AttemptNo           uint32
	Commit              source.CommitSha
	SourceEpoch         target.SourceEpoch
	BuildConfigRevision uint64
	LifecycleUID        uuid.UUID
	Repository          source.RepoName
	Recipe              source.BuildRecipe
	ImageRepository     string
	InstallationID      uint64
	Branch              source.BranchName
	Phase               build.Phase
	BlockedReason       opt.Val[string]
	Failure             opt.Val[string]
	FailureDetail       opt.Val[string]
	ReportedDigest      opt.Val[artifact.Digest]
	Digest              opt.Val[artifact.Digest]
	JobName             opt.Val[string]
	Release             opt.Val[ids.ReleaseID]
	Run                 opt.Val[ids.DeploymentRunID]
	DeployDecision      opt.Val[string]
	Operation           ids.OperationID
	CancelRequested     bool
	CreatedAt           int64
	StartedAt           opt.Val[int64]
	FinishedAt          opt.Val[int64]
}

// PushReference is the image reference the build pushes: the repository
// tagged with the short commit and attempt, so retries never overwrite each
// other.
func (a BuildAttempt) PushReference() string {
	return a.ImageRepository + ":" + a.Commit.Short() + "-" + strconv.FormatUint(uint64(a.AttemptNo), 10)
}

// HeadObserved is the outcome of [Tenant.ObserveHead].
//
//sumtype:decl
type HeadObserved interface{ headObserved() }

type (
	// HeadUnchanged means the provider's head is the one recorded: nothing to
	// build.
	HeadUnchanged struct{}
	// HeadQueued is a new source epoch and a queued build of it.
	HeadQueued struct {
		Attempt   ids.BuildAttemptID
		Operation ids.OperationID
		Epoch     target.SourceEpoch
	}
	// HeadRepositoryChanged means the provider's repository id differs from
	// the pinned one.
	HeadRepositoryChanged struct{}
	// HeadGone means the target is being deleted, or the binding is gone.
	HeadGone struct{}
	// HeadFenced means the claim's fence moved on: nothing was written.
	HeadFenced struct{}
)

func (HeadUnchanged) headObserved()         {}
func (HeadQueued) headObserved()            {}
func (HeadRepositoryChanged) headObserved() {}
func (HeadGone) headObserved()              {}
func (HeadFenced) headObserved()            {}

// BuildFailureReport is a failure a worker reports: a build.Failure code
// (kuben_core::ops::BuildFailure) and a detail, bounded when stored.
type BuildFailureReport struct {
	Code   string
	Detail string
}

// BuildProgress is what a worker records with a build event.
type BuildProgress struct {
	JobName        opt.Val[string]
	ReportedDigest opt.Val[artifact.Digest]
	Failure        opt.Val[BuildFailureReport]
	BlockedReason  opt.Val[string]
}

// BuildAdvance is the outcome of [Tenant.AdvanceBuild].
//
//sumtype:decl
type BuildAdvance interface{ buildAdvance() }

type (
	// BuildMoved means the attempt is in Phase now.
	BuildMoved struct{ Phase build.Phase }
	// BuildIllegal means the event is not allowed in the current phase;
	// nothing changed.
	BuildIllegal struct{ Err *ops.IllegalTransitionError }
	// BuildFenced means the claim's fence moved on or the operation is
	// settled: stop.
	BuildFenced struct{}
)

func (BuildMoved) buildAdvance()   {}
func (BuildIllegal) buildAdvance() {}
func (BuildFenced) buildAdvance()  {}

// SlotLimits are the build admission limits (ADR-028). Zero means no build
// may start.
type SlotLimits struct {
	Total  uint32
	PerOrg uint32
}

// Completed is the outcome of [Tenant.CompleteBuild].
//
//sumtype:decl
type Completed interface{ completed() }

type (
	// CompletedDeployed means the release was deployed by a new run.
	CompletedDeployed struct {
		Release    ids.ReleaseID
		Run        ids.DeploymentRunID
		Generation target.Generation
	}
	// CompletedKept means the release is kept; the target did not take it
	// (Decision is the reason code).
	CompletedKept struct {
		Release  ids.ReleaseID
		Decision string
	}
	// CompletedIllegal means the attempt could not succeed from its phase.
	CompletedIllegal struct{ Err *ops.IllegalTransitionError }
	// CompletedFenced means the claim's fence moved on: nothing was written.
	CompletedFenced struct{}
)

func (CompletedDeployed) completed() {}
func (CompletedKept) completed()     {}
func (CompletedIllegal) completed()  {}
func (CompletedFenced) completed()   {}

// Installation is a GitHub App installation linked to the organization.
type Installation struct {
	ID        uint64
	Account   string
	Suspended bool
}

// InstallationLink is the organization an installation is linked to.
type InstallationLink struct {
	Org       ids.OrgID
	Suspended bool
}

// RejectCode is the stable code of a refused automatic deploy.
func RejectCode(reject target.Reject) string {
	switch reject.(type) {
	case target.LifecycleMismatch:
		return "LifecycleMismatch"
	case target.Deleting:
		return "TargetDeleting"
	case target.StaleSource:
		return "StaleSource"
	case target.BuildConfigChanged:
		return "BuildConfigChanged"
	case target.GenerationMoved:
		return "GenerationMoved"
	case target.NotAutomatic:
		return "NotAutomatic"
	case target.Exhausted:
		return "GenerationExhausted"
	}
	return ""
}

// bounded is at most 2048 bytes of detail, cut on a character boundary.
func bounded(detail string) string {
	const maxBytes = 2048
	if len(detail) <= maxBytes {
		return detail
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(detail[end]) {
		end--
	}
	return detail[:end]
}

// sameRecipe is Rust's derived equality of BuildRecipe; the zero strategy
// reads as auto, as it does everywhere else.
func sameRecipe(a, b source.BuildRecipe) bool {
	return a.Strategy.OrAuto() == b.Strategy.OrAuto() && a.Context == b.Context && a.Dockerfile == b.Dockerfile
}

// optSigned is [signed] of an optional counter.
func optSigned(op string, v opt.Val[uint64]) (opt.Val[int64], error) {
	n, ok := v.Get()
	if !ok {
		return opt.None[int64](), nil
	}
	s, err := signed(op, n)
	if err != nil {
		return opt.None[int64](), err
	}
	return opt.Some(s), nil
}

// optCounter is [counter] of an optional column.
func optCounter(op string, v *int64) (opt.Val[uint64], error) {
	if v == nil {
		return opt.None[uint64](), nil
	}
	n, err := counter(op, *v)
	if err != nil {
		return opt.None[uint64](), err
	}
	return opt.Some(n), nil
}

// parseColumn reads a text column with parse; a refused value is a decode
// error.
func parseColumn[T any](op, value string, parse func(string) (T, error)) (T, error) {
	v, err := parse(value)
	if err != nil {
		var zero T
		return zero, decodeErr(op, "%v", err)
	}
	return v, nil
}

// optColumn is [parseColumn] of a nullable column.
func optColumn[T any](op string, value *string, parse func(string) (T, error)) (opt.Val[T], error) {
	if value == nil {
		return opt.None[T](), nil
	}
	v, err := parseColumn(op, *value, parse)
	if err != nil {
		return opt.None[T](), err
	}
	return opt.Some(v), nil
}

// recipeOf reads a stored recipe.
func recipeOf(op, text string) (source.BuildRecipe, error) {
	var r source.BuildRecipe
	if err := json.Unmarshal([]byte(text), &r); err != nil {
		return source.BuildRecipe{}, decodeErr(op, "%v", err)
	}
	return r, nil
}

// phaseOf reads a stored build phase.
func phaseOf(op, value string) (build.Phase, error) {
	p, err := build.ParsePhase(value)
	if err != nil {
		return "", decodeErr(op, "unknown build phase %s", rustQuote(value))
	}
	return p, nil
}

type bindingRow struct {
	id                              ids.SourceBindingID
	org                             string
	project                         ids.ProjectID
	application                     ids.ApplicationID
	target                          ids.TargetID
	installationID                  int64
	repository                      string
	repositoryID                    *int64
	branch, recipe, imageRepository string
	headSha                         *string
	headEpoch                       int64
	pullRequest                     *int64
}

func scanBinding(row pgx.CollectableRow) (SourceBinding, error) {
	var r bindingRow
	if err := row.Scan(&r.id, &r.org, &r.project, &r.application, &r.target, &r.installationID, &r.repository,
		&r.repositoryID, &r.branch, &r.recipe, &r.imageRepository, &r.headSha, &r.headEpoch, &r.pullRequest); err != nil {
		return SourceBinding{}, err
	}
	return r.binding()
}

func (r bindingRow) binding() (SourceBinding, error) {
	const op = "read a source binding"
	b := SourceBinding{
		ID: r.id, Project: r.project, Application: r.application, Target: r.target,
		ImageRepository: r.imageRepository,
	}
	var err error
	if b.Org, err = orgID(op, r.org); err != nil {
		return SourceBinding{}, err
	}
	if b.InstallationID, err = counter(op, r.installationID); err != nil {
		return SourceBinding{}, err
	}
	if b.Repository, err = parseColumn(op, r.repository, source.ParseRepoName); err != nil {
		return SourceBinding{}, err
	}
	if b.RepositoryID, err = optCounter(op, r.repositoryID); err != nil {
		return SourceBinding{}, err
	}
	if b.Branch, err = parseColumn(op, r.branch, source.ParseBranchName); err != nil {
		return SourceBinding{}, err
	}
	if b.Recipe, err = recipeOf(op, r.recipe); err != nil {
		return SourceBinding{}, err
	}
	if b.HeadSha, err = optColumn(op, r.headSha, source.ParseCommitSha); err != nil {
		return SourceBinding{}, err
	}
	epoch, err := counter(op, r.headEpoch)
	if err != nil {
		return SourceBinding{}, err
	}
	b.HeadEpoch = target.SourceEpoch(epoch)
	if b.PullRequest, err = optCounter(op, r.pullRequest); err != nil {
		return SourceBinding{}, err
	}
	return b, nil
}

type attemptRow struct {
	id                                         ids.BuildAttemptID
	org                                        string
	project                                    ids.ProjectID
	application                                ids.ApplicationID
	target                                     ids.TargetID
	binding                                    ids.SourceBindingID
	attemptNo                                  int32
	commit                                     string
	sourceEpoch, buildConfigRevision           int64
	lifecycleUID                               uuid.UUID
	repository, recipe, imageRepository, phase string
	blockedReason, failure, failureDetail      *string
	reportedDigest, digest, jobName            *string
	release                                    *uuid.UUID
	run                                        *uuid.UUID
	deployDecision                             *string
	operation                                  ids.OperationID
	cancelRequestedAt                          *int64
	createdAt                                  int64
	startedAt, finishedAt                      *int64
	installationID                             int64
	branch                                     string
}

func scanAttempt(row pgx.CollectableRow) (BuildAttempt, error) {
	var r attemptRow
	if err := row.Scan(&r.id, &r.org, &r.project, &r.application, &r.target, &r.binding, &r.attemptNo, &r.commit,
		&r.sourceEpoch, &r.buildConfigRevision, &r.lifecycleUID, &r.repository, &r.recipe, &r.imageRepository,
		&r.phase, &r.blockedReason, &r.failure, &r.failureDetail, &r.reportedDigest, &r.digest, &r.jobName,
		&r.release, &r.run, &r.deployDecision, &r.operation, &r.cancelRequestedAt, &r.createdAt, &r.startedAt,
		&r.finishedAt, &r.installationID, &r.branch); err != nil {
		return BuildAttempt{}, err
	}
	return r.attempt()
}

func (r attemptRow) attempt() (BuildAttempt, error) {
	const op = "read a build attempt"
	a := BuildAttempt{
		ID: r.id, Project: r.project, Application: r.application, Target: r.target, Binding: r.binding,
		LifecycleUID: r.lifecycleUID, ImageRepository: r.imageRepository,
		BlockedReason: opt.FromPtr(r.blockedReason), Failure: opt.FromPtr(r.failure),
		FailureDetail: opt.FromPtr(r.failureDetail), JobName: opt.FromPtr(r.jobName),
		DeployDecision: opt.FromPtr(r.deployDecision), Operation: r.operation,
		CancelRequested: r.cancelRequestedAt != nil, CreatedAt: r.createdAt,
		StartedAt: opt.FromPtr(r.startedAt), FinishedAt: opt.FromPtr(r.finishedAt),
	}
	if r.release != nil {
		a.Release = opt.Some(ids.From[ids.Release](*r.release))
	}
	if r.run != nil {
		a.Run = opt.Some(ids.From[ids.DeploymentRun](*r.run))
	}
	if r.attemptNo < 0 {
		return BuildAttempt{}, decodeErr(op, errOutOfRange)
	}
	a.AttemptNo = uint32(r.attemptNo)
	var err error
	if a.Org, err = orgID(op, r.org); err != nil {
		return BuildAttempt{}, err
	}
	if err := r.inputs(op, &a); err != nil {
		return BuildAttempt{}, err
	}
	if a.Phase, err = phaseOf(op, r.phase); err != nil {
		return BuildAttempt{}, err
	}
	if a.ReportedDigest, err = optColumn(op, r.reportedDigest, artifact.ParseDigest); err != nil {
		return BuildAttempt{}, err
	}
	if a.Digest, err = optColumn(op, r.digest, artifact.ParseDigest); err != nil {
		return BuildAttempt{}, err
	}
	return a, nil
}

// inputs reads the attempt's recorded inputs into a.
func (r attemptRow) inputs(op string, a *BuildAttempt) error {
	var err error
	if a.Commit, err = parseColumn(op, r.commit, source.ParseCommitSha); err != nil {
		return err
	}
	epoch, err := counter(op, r.sourceEpoch)
	if err != nil {
		return err
	}
	a.SourceEpoch = target.SourceEpoch(epoch)
	if a.BuildConfigRevision, err = counter(op, r.buildConfigRevision); err != nil {
		return err
	}
	if a.Repository, err = parseColumn(op, r.repository, source.ParseRepoName); err != nil {
		return err
	}
	if a.Recipe, err = recipeOf(op, r.recipe); err != nil {
		return err
	}
	if a.InstallationID, err = counter(op, r.installationID); err != nil {
		return err
	}
	a.Branch, err = parseColumn(op, r.branch, source.ParseBranchName)
	return err
}

// headRow is a binding and its target, locked.
type headRow struct {
	headSha             *string
	headEpoch           int64
	repositoryID        *int64
	sourceEpoch         int64
	buildConfigRevision int64
	lifecycleUID        uuid.UUID
	deleting            bool
}

func scanHead(row pgx.CollectableRow) (headRow, error) {
	var r headRow
	err := row.Scan(&r.headSha, &r.headEpoch, &r.repositoryID, &r.sourceEpoch, &r.buildConfigRevision,
		&r.lifecycleUID, &r.deleting)
	return r, err
}

// GitInstallationOrg is the organization an installation is linked to, and
// whether it is suspended; false when it is not linked.
func (s *Store) GitInstallationOrg(ctx context.Context, installationID uint64) (InstallationLink, bool, error) {
	const op = "read an installation"
	id, err := signed(op, installationID)
	if err != nil {
		return InstallationLink{}, false, err
	}
	type row struct {
		org       string
		suspended bool
	}
	r, ok, err := queryOpt(ctx, s.db, op, gitInstallationOrg, func(cr pgx.CollectableRow) (row, error) {
		var r row
		err := cr.Scan(&r.org, &r.suspended)
		return r, err
	}, GitHub, id)
	if err != nil || !ok {
		return InstallationLink{}, false, err
	}
	org, err := orgID(op, r.org)
	if err != nil {
		return InstallationLink{}, false, err
	}
	return InstallationLink{Org: org, Suspended: r.suspended}, true, nil
}

// SetInstallationSuspended records a suspension or its end, told by the
// provider. False when the installation is not linked.
func (s *Store) SetInstallationSuspended(ctx context.Context, installationID uint64, suspended bool) (bool, error) {
	const op = "suspend an installation"
	id, err := signed(op, installationID)
	if err != nil {
		return false, err
	}
	n, err := exec(ctx, s.db, op, setSuspended, GitHub, id, suspended, s.now())
	return n == 1, err
}

// LinkInstallation links a GitHub App installation to this organization.
// False when another organization holds it; the link never moves silently.
func (t *Tenant) LinkInstallation(ctx context.Context, installationID uint64, account string) (bool, error) {
	const op = "link an installation"
	id, err := signed(op, installationID)
	if err != nil {
		return false, err
	}
	_, ok, err := queryOpt(ctx, t.tx, op, upsertInstallation, pgx.RowTo[string],
		GitHub, id, t.org.String(), account, t.store.now())
	return ok, err
}

// Installations is this organization's installations.
func (t *Tenant) Installations(ctx context.Context) ([]Installation, error) {
	const op = "list installations"
	return queryAll(ctx, t.tx, op, selectInstallations, func(row pgx.CollectableRow) (Installation, error) {
		var id int64
		var i Installation
		if err := row.Scan(&id, &i.Account, &i.Suspended); err != nil {
			return Installation{}, err
		}
		n, err := counter(op, id)
		i.ID = n
		return i, err
	}, GitHub, t.org.String())
}

// BindSource binds tgt to a repository and branch, or changes its binding.
// A change raises the target's build configuration revision and forgets the
// recorded head, so the next sync builds again.
func (t *Tenant) BindSource(ctx context.Context, project ids.ProjectID, tgt ids.TargetID, binding NewBinding) (Bound, error) {
	const op = "bind a source"
	org := t.org.String()
	installation, err := signed(op, binding.InstallationID)
	if err != nil {
		return nil, err
	}
	var owns bool
	if err := queryOne(ctx, t.tx, op, ownsInstallation, []any{&owns}, GitHub, installation, org); err != nil {
		return nil, err
	}
	if !owns {
		return BoundInstallationMissing{}, nil
	}
	type locked struct {
		application ids.ApplicationID
		revision    int64
		deleting    bool
	}
	l, ok, err := queryOpt(ctx, t.tx, op, lockBindingTarget, func(row pgx.CollectableRow) (locked, error) {
		var l locked
		err := row.Scan(&l.application, &l.revision, &l.deleting)
		return l, err
	}, tgt, org, project)
	if err != nil {
		return nil, err
	}
	if !ok || l.deleting {
		return BoundNotFound{}, nil
	}
	recipe, err := canonical(op, binding.Recipe)
	if err != nil {
		return nil, err
	}
	pullRequest, err := optSigned(op, binding.PullRequest)
	if err != nil {
		return nil, err
	}
	now := t.store.now()
	existing, found, err := t.BindingOfTarget(ctx, tgt)
	if err != nil {
		return nil, err
	}
	if !found {
		id := ids.New[ids.SourceBinding]()
		_, err := exec(ctx, t.tx, op, insertSourceBinding, id, org, project, l.application, tgt, GitHub, installation,
			binding.Repository.String(), binding.Branch.String(), recipe, binding.ImageRepository, now,
			pullRequest.Ptr())
		if err != nil {
			return nil, err
		}
		return BoundCreated{ID: id}, nil
	}
	if existing.InstallationID == binding.InstallationID && existing.Repository == binding.Repository &&
		existing.Branch == binding.Branch && sameRecipe(existing.Recipe, binding.Recipe) &&
		existing.ImageRepository == binding.ImageRepository && existing.PullRequest == binding.PullRequest {
		return BoundUnchanged{ID: existing.ID}, nil
	}
	if _, err := exec(ctx, t.tx, op, updateBinding, existing.ID, installation, binding.Repository.String(),
		binding.Branch.String(), recipe, binding.ImageRepository, now, pullRequest.Ptr()); err != nil {
		return nil, err
	}
	if _, err := exec(ctx, t.tx, op, raiseBuildConfig, tgt); err != nil {
		return nil, err
	}
	return BoundChanged{ID: existing.ID}, nil
}

// BindingOfTarget is the source binding of tgt.
func (t *Tenant) BindingOfTarget(ctx context.Context, tgt ids.TargetID) (SourceBinding, bool, error) {
	return queryOpt(ctx, t.tx, "read a target's source binding", selectBindingOfTarget, scanBinding,
		tgt, t.org.String())
}

// Binding is a binding by id.
func (t *Tenant) Binding(ctx context.Context, id ids.SourceBindingID) (SourceBinding, bool, error) {
	return queryOpt(ctx, t.tx, "read a source binding", selectBinding, scanBinding, id, t.org.String())
}

// BindingsForPush is this organization's bindings a push to repository and
// branch through installationID concerns.
func (t *Tenant) BindingsForPush(
	ctx context.Context, installationID uint64, repository source.RepoName, branch source.BranchName,
) ([]SourceBinding, error) {
	const op = "read the bindings of a push"
	installation, err := signed(op, installationID)
	if err != nil {
		return nil, err
	}
	return queryAll(ctx, t.tx, op, selectBindingsForPush, scanBinding,
		GitHub, installation, repository.String(), branch.String(), t.org.String())
}

// RequestSync asks for binding's head to be read from the provider. A sync
// that is still pending answers instead of a second one.
func (t *Tenant) RequestSync(ctx context.Context, binding SourceBinding, requestedBy string, audit NewAudit) (ids.OperationID, error) {
	const op = "request a source sync"
	pending, ok, err := queryOpt(ctx, t.tx, op, pendingSync, scanID[ids.Operation],
		SourceSyncKind, t.org.String(), binding.Target)
	if err != nil {
		return ids.OperationID{}, err
	}
	if ok {
		return pending, nil
	}
	nonce, err := uuid.NewV7()
	if err != nil {
		return ids.OperationID{}, dbErr(op, err)
	}
	bindingUUID := binding.ID.UUID()
	inputHash := slices.Concat(bindingUUID[:], nonce[:])
	accepted, err := t.Accept(ctx, NewOperation{
		Kind:        SourceSyncKind,
		Target:      opt.Some(OperationTarget{Project: binding.Project, Target: binding.Target}),
		InputHash:   inputHash,
		Payload:     map[string]any{"binding": binding.ID},
		RequestedBy: requestedBy,
		Topic:       syncTopic,
	}, audit, opt.None[IdempotencyKey]())
	if err != nil {
		return ids.OperationID{}, err
	}
	switch a := accepted.(type) {
	case AcceptedNew:
		return a.ID, nil
	case AcceptedReplayed:
		return a.ID, nil
	case AcceptedKeyReused:
		return a.ID, nil
	}
	return ids.OperationID{}, protocolErr(op, "unknown acceptance %T", accepted)
}

// SyncBinding is the binding a claimed `source.sync` operation reads.
func (t *Tenant) SyncBinding(ctx context.Context, claim Claim) (SourceBinding, bool, error) {
	payload, ok, err := t.fencePayload(ctx, claim)
	if err != nil || !ok {
		return SourceBinding{}, false, err
	}
	fields, ok := payload.(map[string]any)
	if !ok {
		return SourceBinding{}, false, nil
	}
	text, ok := fields["binding"].(string)
	if !ok {
		return SourceBinding{}, false, nil
	}
	// Rust: `.parse().ok()`; an unreadable id is no binding.
	if id, err := uuid.Parse(text); err == nil {
		return t.Binding(ctx, ids.From[ids.SourceBinding](id))
	}
	return SourceBinding{}, false, nil
}

// fencePayload locks claim's operation if the claim still holds it; its
// payload.
func (t *Tenant) fencePayload(ctx context.Context, claim Claim) (any, bool, error) {
	const op = "lock a claimed operation"
	text, ok, err := queryOpt(ctx, t.tx, op, lockFence, pgx.RowTo[string], claim.ID, claim.Fence, t.org.String())
	if err != nil || !ok {
		return nil, false, err
	}
	payload, err := jsonValue(op, text)
	if err != nil {
		return nil, false, err
	}
	return payload, true, nil
}

// ObserveHead records head as read from the provider for binding (plan
// §14.1). The same head changes nothing; a new one (a push, a force-push or
// a changed binding) raises the target's source epoch, asks the binding's
// unfinished builds to stop and queues a build of head.
func (t *Tenant) ObserveHead(
	ctx context.Context, claim Claim, binding SourceBinding, head source.CommitSha, repositoryID uint64,
) (HeadObserved, error) {
	const op = "observe a source head"
	if _, live, err := t.fencePayload(ctx, claim); err != nil {
		return nil, err
	} else if !live {
		return HeadFenced{}, nil
	}
	row, ok, err := queryOpt(ctx, t.tx, op, lockBindingAndTarget, scanHead, binding.ID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return HeadGone{}, nil
	}
	// Read under the lock: a concurrent rebinding cannot slip between.
	current, ok, err := t.Binding(ctx, binding.ID)
	if err != nil {
		return nil, err
	}
	if !ok || row.deleting {
		return HeadGone{}, nil
	}
	pinned, err := optCounter(op, row.repositoryID)
	if err != nil {
		return nil, err
	}
	if id, ok := pinned.Get(); ok && id != repositoryID {
		return HeadRepositoryChanged{}, nil
	}
	if row.headSha != nil && *row.headSha == head.String() && row.headEpoch == row.sourceEpoch {
		return HeadUnchanged{}, nil
	}
	sourceEpoch, err := counter(op, row.sourceEpoch)
	if err != nil {
		return nil, err
	}
	epoch := target.SourceEpoch(sourceEpoch)
	if epoch < math.MaxUint64 {
		epoch++
	}
	if err := t.recordHead(ctx, current, head, epoch, repositoryID); err != nil {
		return nil, err
	}
	config, err := counter(op, row.buildConfigRevision)
	if err != nil {
		return nil, err
	}
	attempt, operation, err := t.queueAttempt(ctx, current, head, epoch, config, row.lifecycleUID, 1)
	if err != nil {
		return nil, err
	}
	return HeadQueued{Attempt: attempt, Operation: operation, Epoch: epoch}, nil
}

// recordHead raises the target to epoch, records head as binding's and asks
// the binding's unfinished builds to stop.
func (t *Tenant) recordHead(
	ctx context.Context, binding SourceBinding, head source.CommitSha, epoch target.SourceEpoch, repositoryID uint64,
) error {
	const op = "observe a source head"
	now := t.store.now()
	signedEpoch, err := signed(op, uint64(epoch))
	if err != nil {
		return err
	}
	raised, err := exec(ctx, t.tx, op, raiseEpoch, binding.Target, signedEpoch)
	if err != nil {
		return err
	}
	if raised != 1 {
		return dbErr(op, pgx.ErrNoRows)
	}
	repository, err := signed(op, repositoryID)
	if err != nil {
		return err
	}
	if _, err := exec(ctx, t.tx, op, recordHead, binding.ID, head.String(), signedEpoch, repository, now); err != nil {
		return err
	}
	return t.stopOlderBuilds(ctx, binding.ID, now)
}

// stopOlderBuilds: a newer head supersedes every unfinished build of the
// binding.
func (t *Tenant) stopOlderBuilds(ctx context.Context, binding ids.SourceBindingID, now int64) error {
	const op = "stop older builds"
	operations, err := queryAll(ctx, t.tx, op, askOlderToStop, pgx.RowTo[uuid.UUID], binding, now, terminalPhases())
	if err != nil {
		return err
	}
	if len(operations) > 0 {
		if _, err := exec(ctx, t.tx, op, wakeOperations, operations); err != nil {
			return err
		}
	}
	return nil
}

func (t *Tenant) queueAttempt(
	ctx context.Context, binding SourceBinding, commit source.CommitSha, epoch target.SourceEpoch,
	buildConfigRevision uint64, lifecycleUID uuid.UUID, attemptNo uint32,
) (ids.BuildAttemptID, ids.OperationID, error) {
	const op = "queue a build"
	attempt := ids.New[ids.BuildAttempt]()
	attemptUUID := attempt.UUID()
	requestedBy := "source:" + binding.ID.String()
	accepted, err := t.Accept(ctx, NewOperation{
		Kind:         BuildKind,
		Target:       opt.Some(OperationTarget{Project: binding.Project, Target: binding.Target}),
		LifecycleUID: opt.Some(lifecycleUID),
		InputHash:    attemptUUID[:],
		Payload:      map[string]any{"attempt": attempt, "commit": commit, "source_epoch": epoch},
		RequestedBy:  requestedBy,
		Topic:        buildTopic,
	}, NewAudit{
		ActorKind:  "system",
		ActorID:    opt.Some(requestedBy),
		Action:     "queueBuild",
		TargetKind: opt.Some("build"),
		TargetRef:  opt.Some(attempt.String()),
		Outcome:    "accepted",
		Data:       opt.Some[any](map[string]any{"commit": commit, "repository": binding.Repository, "attempt": attemptNo}),
	}, opt.None[IdempotencyKey]())
	if err != nil {
		return ids.BuildAttemptID{}, ids.OperationID{}, err
	}
	operation, ok := accepted.(AcceptedNew)
	if !ok {
		return ids.BuildAttemptID{}, ids.OperationID{}, protocolErr(op, "a build operation without a key was replayed")
	}
	recipe, err := canonical(op, binding.Recipe)
	if err != nil {
		return ids.BuildAttemptID{}, ids.OperationID{}, err
	}
	number, err := int4(op, attemptNo)
	if err != nil {
		return ids.BuildAttemptID{}, ids.OperationID{}, err
	}
	signedEpoch, err := signed(op, uint64(epoch))
	if err != nil {
		return ids.BuildAttemptID{}, ids.OperationID{}, err
	}
	revision, err := signed(op, buildConfigRevision)
	if err != nil {
		return ids.BuildAttemptID{}, ids.OperationID{}, err
	}
	if _, err := exec(ctx, t.tx, op, insertAttempt, attempt, t.org.String(), binding.Project, binding.Application,
		binding.Target, binding.ID, number, commit.String(), signedEpoch, revision, lifecycleUID,
		binding.Repository.String(), recipe, binding.ImageRepository, operation.ID, t.store.now()); err != nil {
		return ids.BuildAttemptID{}, ids.OperationID{}, err
	}
	return attempt, operation.ID, nil
}

// RetryBuild queues a new attempt with the inputs of failed after an
// infrastructure failure (a lost worker), unless the source moved on or
// [MaxBuildAttempts] were made. False when no retry was queued.
func (t *Tenant) RetryBuild(ctx context.Context, failed BuildAttempt) (ids.BuildAttemptID, bool, error) {
	const op = "retry a build"
	binding, ok, err := t.Binding(ctx, failed.Binding)
	if err != nil || !ok {
		return ids.BuildAttemptID{}, false, err
	}
	row, ok, err := queryOpt(ctx, t.tx, op, lockBindingAndTarget, scanHead, binding.ID)
	if err != nil {
		return ids.BuildAttemptID{}, false, err
	}
	epoch, epochErr := signed(op, uint64(failed.SourceEpoch))
	revision, revisionErr := signed(op, failed.BuildConfigRevision)
	current := ok && !row.deleting && epochErr == nil && row.sourceEpoch == epoch &&
		revisionErr == nil && row.buildConfigRevision == revision
	if !current {
		return ids.BuildAttemptID{}, false, nil
	}
	var next int32
	if err := queryOne(ctx, t.tx, op, nextAttemptNo, []any{&next}, binding.ID, epoch, revision); err != nil {
		return ids.BuildAttemptID{}, false, err
	}
	if next < 0 {
		return ids.BuildAttemptID{}, false, decodeErr(op, errOutOfRange)
	}
	if uint32(next) > MaxBuildAttempts {
		return ids.BuildAttemptID{}, false, nil
	}
	// The binding's recipe may have changed only with a new revision, which
	// `current` rules out; the attempt keeps the recorded inputs.
	binding.Recipe = failed.Recipe
	binding.ImageRepository = failed.ImageRepository
	binding.Repository = failed.Repository
	attempt, _, err := t.queueAttempt(ctx, binding, failed.Commit, failed.SourceEpoch, failed.BuildConfigRevision,
		failed.LifecycleUID, uint32(next))
	if err != nil {
		return ids.BuildAttemptID{}, false, err
	}
	return attempt, true, nil
}

// BuildOfOperation is the build attempt of a claimed `build` operation.
func (t *Tenant) BuildOfOperation(ctx context.Context, operation ids.OperationID) (BuildAttempt, bool, error) {
	return t.attemptWhere(ctx, attemptByOperation, operation.UUID())
}

// BuildOfTarget is build attempt id of tgt.
func (t *Tenant) BuildOfTarget(ctx context.Context, tgt ids.TargetID, id ids.BuildAttemptID) (BuildAttempt, bool, error) {
	a, ok, err := t.attemptWhere(ctx, attemptByID, id.UUID())
	if err != nil || !ok || a.Target != tgt {
		return BuildAttempt{}, false, err
	}
	return a, true, nil
}

// attemptWhere reads one attempt by query, attemptByOperation or
// attemptByID: constants of this file, as Rust's filters were.
func (t *Tenant) attemptWhere(ctx context.Context, query string, id uuid.UUID) (BuildAttempt, bool, error) {
	return queryOpt(ctx, t.tx, "read a build attempt", query, scanAttempt, id, t.org.String())
}

// BuildsOfTarget is the newest limit build attempts of tgt.
func (t *Tenant) BuildsOfTarget(ctx context.Context, tgt ids.TargetID, limit int64) ([]BuildAttempt, error) {
	return queryAll(ctx, t.tx, "list a target's builds", attemptsOfTarget, scanAttempt, tgt, t.org.String(), limit)
}

// RequestBuildCancel asks build to stop. The worker applies the request on
// its next claim, which this wakes. False when the build is unknown or
// finished.
func (t *Tenant) RequestBuildCancel(ctx context.Context, tgt ids.TargetID, id ids.BuildAttemptID) (ids.OperationID, bool, error) {
	const op = "cancel a build"
	operation, ok, err := queryOpt(ctx, t.tx, op, requestCancel, pgx.RowTo[uuid.UUID],
		id, tgt, t.store.now(), terminalPhases())
	if err != nil || !ok {
		return ids.OperationID{}, false, err
	}
	if _, err := exec(ctx, t.tx, op, wakeOperations, []uuid.UUID{operation}); err != nil {
		return ids.OperationID{}, false, err
	}
	return ids.From[ids.Operation](operation), true, nil
}

// AdvanceBuild applies event to attempt for the worker holding claim, by
// the rules of [build.Phase.Apply]. A terminal phase releases the build slot
// in the same transaction. Reaching `succeeded` needs [Tenant.CompleteBuild].
func (t *Tenant) AdvanceBuild(
	ctx context.Context, claim Claim, attempt ids.BuildAttemptID, event build.Event, progress BuildProgress,
) (BuildAdvance, error) {
	return t.moveAttempt(ctx, claim, attempt, event, progress, opt.None[artifact.Digest]())
}

func (t *Tenant) moveAttempt(
	ctx context.Context, claim Claim, attempt ids.BuildAttemptID, event build.Event, progress BuildProgress,
	verified opt.Val[artifact.Digest],
) (BuildAdvance, error) {
	const op = "advance a build"
	phase, ok, err := queryOpt(ctx, t.tx, op, lockAttemptUnderFence, pgx.RowTo[string], attempt, claim.ID, claim.Fence)
	if err != nil {
		return nil, err
	}
	if !ok {
		return BuildFenced{}, nil
	}
	from, err := phaseOf(op, phase)
	if err != nil {
		return nil, err
	}
	next, err := from.Apply(event)
	if err != nil {
		var illegal *ops.IllegalTransitionError
		if errors.As(err, &illegal) {
			return BuildIllegal{Err: illegal}, nil
		}
		return nil, err
	}
	if (next == build.Succeeded) != verified.IsSome() {
		return nil, protocolErr(op, "a build succeeds exactly with a verified digest")
	}
	failure, detail := opt.None[string](), opt.None[string]()
	if f, ok := progress.Failure.Get(); ok {
		failure, detail = opt.Some(f.Code), opt.Some(bounded(f.Detail))
	}
	if _, err := exec(ctx, t.tx, op, setAttempt, attempt, next.String(), t.store.now(), progress.JobName.Ptr(),
		digestText(progress.ReportedDigest).Ptr(), failure.Ptr(), detail.Ptr(), progress.BlockedReason.Ptr(),
		digestText(verified).Ptr(), terminalPhases()); err != nil {
		return nil, err
	}
	if next.IsTerminal() {
		if _, err := exec(ctx, t.tx, op, releaseSlot, attempt); err != nil {
			return nil, err
		}
	}
	return BuildMoved{Phase: next}, nil
}

// digestText is the text of an optional digest.
func digestText(d opt.Val[artifact.Digest]) opt.Val[string] {
	v, ok := d.Get()
	if !ok {
		return opt.None[string]()
	}
	return opt.Some(v.String())
}

// ClaimBuildSlot takes a build slot for attempt (ADR-028): atomic across
// workers, and idempotent for an attempt that holds one. granted is false
// when the limits are reached, or zero; live is false when the claim's
// fence moved on.
func (t *Tenant) ClaimBuildSlot(
	ctx context.Context, claim Claim, attempt ids.BuildAttemptID, limits SlotLimits,
) (granted, live bool, err error) {
	const op = "claim a build slot"
	if _, live, err := t.fencePayload(ctx, claim); err != nil || !live {
		return false, false, err
	}
	if _, err := exec(ctx, t.tx, op, lockSlots, slotLock); err != nil {
		return false, false, err
	}
	var held bool
	if err := queryOne(ctx, t.tx, op, hasSlot, []any{&held}, attempt); err != nil {
		return false, false, err
	}
	if held {
		return true, true, nil
	}
	org := t.org.String()
	var total, ofOrg int64
	if err := queryOne(ctx, t.tx, op, slotsInUse, []any{&total, &ofOrg}, org); err != nil {
		return false, false, err
	}
	if total >= int64(limits.Total) || ofOrg >= int64(limits.PerOrg) {
		return false, true, nil
	}
	if _, err := exec(ctx, t.tx, op, insertSlot, attempt, org, t.store.now()); err != nil {
		return false, false, err
	}
	return true, true, nil
}

// CompleteBuild settles attempt as `succeeded` with its verified digest,
// records its release and, if the target still follows this source epoch and
// build configuration automatically, accepts a deployment run of it; all in
// this transaction (plan §8.2, R04).
func (t *Tenant) CompleteBuild(ctx context.Context, claim Claim, attempt BuildAttempt, digest artifact.Digest) (Completed, error) {
	const op = "complete a build"
	moved, err := t.moveAttempt(ctx, claim, attempt.ID, build.EventVerified, BuildProgress{}, opt.Some(digest))
	if err != nil {
		return nil, err
	}
	switch m := moved.(type) {
	case BuildMoved:
	case BuildIllegal:
		return CompletedIllegal(m), nil
	case BuildFenced:
		return CompletedFenced{}, nil
	}
	createdBy := "build:" + attempt.ID.String()
	release, _, err := t.CreateRelease(ctx, attempt.Project, PortableRelease{
		Application:     attempt.Application,
		Artifacts:       map[string]artifact.Digest{BuildProcess: digest},
		ProcessContract: map[string]any{},
		PortableConfig:  map[string]any{},
		RendererSchema:  1,
		Source: opt.Some[any](map[string]any{
			"image_repository": attempt.ImageRepository,
			"repository":       attempt.Repository,
			"branch":           attempt.Branch,
			"commit":           attempt.Commit,
			"build":            attempt.ID,
		}),
		CreatedBy: createdBy,
	})
	if err != nil {
		return nil, err
	}
	deployed, err := t.autodeploy(ctx, attempt, release, createdBy)
	if err != nil {
		return nil, err
	}
	run := opt.None[uuid.UUID]()
	if r, ok := deployed.run.Get(); ok {
		run = opt.Some(r.UUID())
	}
	if _, err := exec(ctx, t.tx, op, setOutput, attempt.ID, release, run.Ptr(), deployed.decision,
		t.store.now()); err != nil {
		return nil, err
	}
	r, hasRun := deployed.run.Get()
	generation, hasGeneration := deployed.generation.Get()
	if hasRun && hasGeneration {
		return CompletedDeployed{Release: release, Run: r, Generation: generation}, nil
	}
	return CompletedKept{Release: release, Decision: deployed.decision}, nil
}

// deployment is what an automatic deploy of a build decided.
type deployment struct {
	run        opt.Val[ids.DeploymentRunID]
	decision   string
	generation opt.Val[target.Generation]
}

func (t *Tenant) autodeploy(ctx context.Context, attempt BuildAttempt, release ids.ReleaseID, createdBy string) (deployment, error) {
	const op = "deploy a build"
	var generation int64
	if err := queryOne(ctx, t.tx, op, lockTargetGeneration, []any{&generation}, attempt.Target); err != nil {
		return deployment{}, err
	}
	revision, ok, err := t.LatestConfigRevision(ctx, attempt.Target)
	if err != nil {
		return deployment{}, err
	}
	if !ok {
		return deployment{decision: "NoConfiguration"}, nil
	}
	expected, err := counter(op, generation)
	if err != nil {
		return deployment{}, err
	}
	attemptUUID := attempt.ID.UUID()
	started, err := t.StartBuildDeployment(ctx, StartDeployment{
		Project:            attempt.Project,
		Target:             attempt.Target,
		Release:            release,
		ConfigRevision:     revision,
		ExpectedGeneration: target.Generation(expected),
		LifecycleUID:       attempt.LifecycleUID,
		Reason:             ReasonBuild,
		RequestedBy:        createdBy,
		InputHash:          attemptUUID[:],
	}, attempt.SourceEpoch, attempt.BuildConfigRevision, NewAudit{
		ActorKind:  "system",
		ActorID:    opt.Some(createdBy),
		Action:     "startDeployment",
		TargetKind: opt.Some("app"),
		TargetRef:  opt.Some(attempt.Target.String()),
		Outcome:    "accepted",
		Data:       opt.Some[any](map[string]any{"build": attempt.ID, "commit": attempt.Commit}),
	})
	if err != nil {
		return deployment{}, err
	}
	return deploymentOf(started), nil
}

// deploymentOf is the decision a started (or refused) build deployment
// records.
func deploymentOf(started Started) deployment {
	switch s := started.(type) {
	case StartedAccepted:
		return deployment{run: opt.Some(s.Run), decision: "deployed", generation: opt.Some(s.Generation)}
	case StartedRejected:
		return deployment{decision: RejectCode(s.Reject)}
	case StartedNotFound:
		return deployment{decision: "TargetMissing"}
	case StartedSecretRevoked:
		return deployment{decision: "SecretRevoked"}
	case StartedVulnerabilityBlocked:
		return deployment{decision: "VulnerabilityBlocked"}
	case StartedFrozen:
		return deployment{decision: "EnvironmentFrozen"}
	case StartedUntrusted:
		return deployment{decision: "UntrustedPreview"}
	case StartedReplayed, StartedKeyReused:
		return deployment{decision: "Replayed"}
	}
	return deployment{decision: "Replayed"}
}
