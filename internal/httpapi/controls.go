package api

// Operational controls (M4.9; routes/controls.rs): owners, change freezes,
// paused delivery, alert silences and emergency rollbacks. Each has one
// effect:
//
//   - an owner names who answers for a project or an application;
//   - a freeze refuses new releases to an environment (secret rotations
//     and restarts still pass, and so does an emergency rollback);
//   - a pause holds an app's delivery: runs are accepted but not written,
//     and on resume only the newest is;
//   - a silence keeps alerts quiet, without hiding what happened;
//   - an emergency rollback is a person's break-glass: it returns an app to
//     an earlier release past approvals, a freeze, the scan gate and a
//     pause, with a reason, audited.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/internal/httpapi/access"
	"github.com/Teamtem-dev/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/internal/store"
)

const (
	// maxFreezeMs is the longest freeze.
	maxFreezeMs = 30 * 86_400_000
	// maxSilenceMs is the longest silence.
	maxSilenceMs = 7 * 86_400_000
)

// controlText is value trimmed, when it has 1 to max characters and none
// of them a control character (controls.rs text).
func controlText(name, value string, maxChars int) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || utf8.RuneCountInString(value) > maxChars || strings.ContainsFunc(value, unicode.IsControl) {
		return "", kerrors.New(kerrors.Validation, "%s must be 1 to %d printable characters", name, maxChars)
	}
	return value, nil
}

// controlMillis reads an RFC 3339 time as unix milliseconds.
func controlMillis(name, value string) (int64, error) {
	at, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return 0, kerrors.New(kerrors.Validation, "%s is not an RFC 3339 time", name)
	}
	return at.UnixMilli(), nil
}

// controlWindow is the window body asks for, from now, at most maxMs long.
func controlWindow(body *gen.CreateWindow, now, maxMs int64) (int64, int64, error) {
	starts := now
	if s, ok := body.StartsAt.Get(); ok {
		var err error
		if starts, err = controlMillis("startsAt", s); err != nil {
			return 0, 0, err
		}
	}
	ends, err := controlMillis("endsAt", body.EndsAt)
	if err != nil {
		return 0, 0, err
	}
	if ends <= max(starts, now) {
		return 0, 0, kerrors.New(kerrors.Validation, "endsAt must be in the future and after startsAt")
	}
	if ends-max(starts, now) > maxMs || starts-now > maxMs {
		return 0, 0, kerrors.New(kerrors.Validation, "a window lasts at most %d days", maxMs/86_400_000)
	}
	return starts, ends, nil
}

// checkOwner is the owner body names, checked.
func checkOwner(body *gen.OwnerDto) (store.NewOwner, error) {
	owner, err := controlText("owner", body.Owner, 256)
	if err != nil {
		return store.NewOwner{}, err
	}
	o := store.NewOwner{Owner: owner}
	if c, ok := body.Contact.Get(); ok {
		contact, err := controlText("contact", c, 512)
		if err != nil {
			return store.NewOwner{}, err
		}
		o.Contact = opt.Some(contact)
	}
	if u, ok := body.RunbookUrl.Get(); ok {
		url := strings.TrimSpace(u)
		if (!strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://")) || len(url) > 2048 {
			return store.NewOwner{}, kerrors.New(kerrors.Validation, "runbookUrl must be an http(s) URL")
		}
		o.RunbookURL = opt.Some(url)
	}
	return o, nil
}

// ownerDto is an owner as the contract shows it. Rust also wrote
// updatedBy and updatedAt, which the contract does not name.
func ownerDto(o store.Owner) gen.OwnerDto {
	return gen.OwnerDto{Owner: o.Owner, Contact: optNilString(o.Contact), RunbookUrl: optNilString(o.RunbookURL)}
}

func nilOwnerDto(o store.Owner, found bool) *gen.NilOwnerDto {
	if !found {
		return &gen.NilOwnerDto{Null: true}
	}
	return &gen.NilOwnerDto{Value: ownerDto(o)}
}

// GetProjectOwner is who answers for a project.
func (s *Server) GetProjectOwner(ctx context.Context, params gen.GetProjectOwnerParams) (gen.GetProjectOwnerRes, error) {
	acc, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	p, err := s.findProject(ctx, acc, params.Project)
	if err != nil {
		return nil, err
	}
	if _, err := acc.Require(perm.ProjectRead, p.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	t, err := s.deps.Store.Tenant(ctx, p.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	o, found, err := t.Owner(ctx, p.project.ID, opt.None[ids.ApplicationID]())
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	return nilOwnerDto(o, found), nil
}

// PutProjectOwner names who answers for a project.
func (s *Server) PutProjectOwner(ctx context.Context, req *gen.OwnerDto, params gen.PutProjectOwnerParams) (gen.PutProjectOwnerRes, error) {
	acc, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	p, err := s.findProject(ctx, acc, params.Project)
	if err != nil {
		return nil, err
	}
	if _, err := acc.Require(perm.ProjectWrite, p.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	owner, err := checkOwner(req)
	if err != nil {
		return nil, err
	}
	_, actor := acc.Actor()
	dto, err := s.setOwner(ctx, actor, p.org, p.project.ID, opt.None[ids.ApplicationID](), owner)
	if err != nil {
		return nil, err
	}
	return &dto, nil
}

// setOwner sets an owner and reads it back, in one transaction.
func (s *Server) setOwner(ctx context.Context, actor string, org ids.OrgID, project ids.ProjectID,
	app opt.Val[ids.ApplicationID], owner store.NewOwner,
) (gen.OwnerDto, error) {
	t, err := s.deps.Store.Tenant(ctx, org)
	if err != nil {
		return gen.OwnerDto{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // committed on success
	if err := t.SetOwner(ctx, project, app, opt.Some(owner), actor); err != nil {
		return gen.OwnerDto{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	saved, found, err := t.Owner(ctx, project, app)
	if err != nil {
		return gen.OwnerDto{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found {
		return gen.OwnerDto{}, kerrors.New(kerrors.Internal, "the owner is missing")
	}
	if err := t.Commit(ctx); err != nil {
		return gen.OwnerDto{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	return ownerDto(saved), nil
}

// applicationOf is the application name of p, or 404.
func applicationOf(ctx context.Context, t *store.Tenant, project ids.ProjectID, app string) (ids.ApplicationID, error) {
	id, found, err := t.Application(ctx, project, app)
	switch {
	case err != nil:
		return ids.ApplicationID{}, err //nolint:wrapcheck // a store error, answered as internal
	case !found:
		return ids.ApplicationID{}, kerrors.New(kerrors.NotFound, "app `%s`", app)
	}
	return id, nil
}

// GetApplicationOwner is who answers for an application (in every
// environment).
func (s *Server) GetApplicationOwner(ctx context.Context, params gen.GetApplicationOwnerParams) (gen.GetApplicationOwnerRes, error) {
	acc, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	p, err := s.findProject(ctx, acc, params.Project)
	if err != nil {
		return nil, err
	}
	if _, err := acc.Require(perm.AppRead, p.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	t, err := s.deps.Store.Tenant(ctx, p.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	application, err := applicationOf(ctx, t, p.project.ID, params.App)
	if err != nil {
		return nil, err
	}
	o, found, err := t.Owner(ctx, p.project.ID, opt.Some(application))
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	return nilOwnerDto(o, found), nil
}

// PutApplicationOwner names who answers for an application.
func (s *Server) PutApplicationOwner(ctx context.Context, req *gen.OwnerDto, params gen.PutApplicationOwnerParams) (gen.PutApplicationOwnerRes, error) {
	acc, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	p, err := s.findProject(ctx, acc, params.Project)
	if err != nil {
		return nil, err
	}
	if _, err := acc.Require(perm.AppWrite, p.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	owner, err := checkOwner(req)
	if err != nil {
		return nil, err
	}
	application, err := s.readApplication(ctx, p, params.App)
	if err != nil {
		return nil, err
	}
	_, actor := acc.Actor()
	dto, err := s.setOwner(ctx, actor, p.org, p.project.ID, opt.Some(application), owner)
	if err != nil {
		return nil, err
	}
	return &dto, nil
}

func (s *Server) readApplication(ctx context.Context, p projectScope, app string) (ids.ApplicationID, error) {
	t, err := s.deps.Store.Tenant(ctx, p.org)
	if err != nil {
		return ids.ApplicationID{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	return applicationOf(ctx, t, p.project.ID, app)
}

func nullUUID() gen.OptNilUUID {
	var n gen.OptNilUUID
	n.SetToNull()
	return n
}

func freezeDto(f store.Freeze, now int64) gen.WindowDto {
	return gen.WindowDto{
		Active: f.LiftedAt.IsNone() && f.StartsAt <= now && now < f.EndsAt,
		ID:     f.ID, Reason: f.Reason, App: nullUUID(), CreatedBy: f.CreatedBy,
		StartsAt: Timestamp(f.StartsAt), EndsAt: Timestamp(f.EndsAt), LiftedAt: liftedAt(f.LiftedAt),
	}
}

func silenceDto(sl store.Silence, now int64) gen.WindowDto {
	app := nullUUID()
	if tgt, ok := sl.Target.Get(); ok {
		app = gen.NewOptNilUUID(tgt.UUID())
	}
	return gen.WindowDto{
		Active: sl.LiftedAt.IsNone() && now < sl.EndsAt,
		ID:     sl.ID, Reason: sl.Reason, App: app, CreatedBy: sl.CreatedBy,
		StartsAt: Timestamp(sl.CreatedAt), EndsAt: Timestamp(sl.EndsAt), LiftedAt: liftedAt(sl.LiftedAt),
	}
}

func liftedAt(at opt.Val[int64]) gen.OptNilString {
	if ms, ok := at.Get(); ok {
		return gen.NewOptNilString(Timestamp(ms))
	}
	return optNilString(opt.None[string]())
}

// windowAudit appends an environment's control record, with its reason.
func windowAudit(ctx context.Context, t *store.Tenant, acc access.Access, e envScope, action, reason string) error {
	record := requestAudit(acc, action, "environment", e.project.project.Slug+"/"+e.env.Slug)
	record.Data = opt.Some[any](map[string]any{"reason": reason})
	return t.AppendAudit(ctx, record) //nolint:wrapcheck // a store error, answered as internal
}

// emergencyHash is the input hash of an emergency rollback request.
func emergencyHash(release ids.ReleaseID, config ids.ConfigRevisionID, generation uint64) []byte {
	sum := sha256.Sum256(fmt.Appendf(nil, "emergency/%s/%s/%d", release, config, generation))
	return sum[:]
}

// environmentFor is the environment of a control route with perm checked,
// and a transaction of its organization.
func (s *Server) environmentFor(ctx context.Context, p perm.Perm, project, environment string) (access.Access, envScope, *store.Tenant, error) {
	acc, err := s.access(ctx)
	if err != nil {
		return access.Access{}, envScope{}, nil, err
	}
	e, err := s.findEnvironment(ctx, acc, project, environment)
	if err != nil {
		return access.Access{}, envScope{}, nil, err
	}
	if _, err := acc.Require(p, e.chain()); err != nil {
		return access.Access{}, envScope{}, nil, err //nolint:wrapcheck // a kerrors already
	}
	t, err := s.deps.Store.Tenant(ctx, e.project.org)
	if err != nil {
		return access.Access{}, envScope{}, nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	return acc, e, t, nil
}

// ListFreezes is the environment's change freezes in force or to come.
func (s *Server) ListFreezes(ctx context.Context, params gen.ListFreezesParams) ([]gen.WindowDto, error) {
	_, e, t, err := s.environmentFor(ctx, perm.EnvRead, params.Project, params.Environment)
	if err != nil {
		return nil, err
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	now := s.deps.Clock.NowMs()
	found, err := t.Freezes(ctx, e.env.ID, params.All.Or(false))
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	out := make([]gen.WindowDto, 0, len(found))
	for _, f := range found {
		out = append(out, freezeDto(f, now))
	}
	return out, nil
}

// newWindow is the window a control route makes.
func newWindow(acc access.Access, e envScope, reason string, target opt.Val[ids.TargetID], startsAt, endsAt int64) (store.NewWindow, error) {
	why, err := controlText("reason", reason, 1024)
	if err != nil {
		return store.NewWindow{}, err
	}
	_, actor := acc.Actor()
	return store.NewWindow{
		Project: e.project.project.ID, Environment: e.env.ID, Target: target, Reason: why, CreatedBy: actor,
		StartsAt: startsAt, EndsAt: endsAt,
	}, nil
}

// CreateFreeze freezes an environment: new releases are refused until it
// ends.
func (s *Server) CreateFreeze(ctx context.Context, req *gen.CreateWindow, params gen.CreateFreezeParams) (gen.CreateFreezeRes, error) {
	acc, e, t, err := s.environmentFor(ctx, perm.EnvWrite, params.Project, params.Environment)
	if err != nil {
		return nil, err
	}
	defer t.Rollback(ctx) //nolint:errcheck // committed on success
	if req.App.IsSet() && !req.App.IsNull() {
		return nil, kerrors.New(kerrors.Validation, "a freeze holds the whole environment")
	}
	now := s.deps.Clock.NowMs()
	startsAt, endsAt, err := controlWindow(req, now, maxFreezeMs)
	if err != nil {
		return nil, err
	}
	w, err := newWindow(acc, e, req.Reason, opt.None[ids.TargetID](), startsAt, endsAt)
	if err != nil {
		return nil, err
	}
	id, err := t.CreateFreeze(ctx, w)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if err := windowAudit(ctx, t, acc, e, "environment.frozen", w.Reason); err != nil {
		return nil, err
	}
	all, err := t.Freezes(ctx, e.env.ID, true)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	for _, f := range all {
		if f.ID == id {
			dto := freezeDto(f, now)
			return &dto, t.Commit(ctx) //nolint:wrapcheck // a store error, answered as internal
		}
	}
	return nil, kerrors.New(kerrors.Internal, "the new freeze is missing")
}

// LiftFreeze lifts a freeze early.
func (s *Server) LiftFreeze(ctx context.Context, params gen.LiftFreezeParams) (gen.LiftFreezeRes, error) {
	acc, e, t, err := s.environmentFor(ctx, perm.EnvWrite, params.Project, params.Environment)
	if err != nil {
		return nil, err
	}
	defer t.Rollback(ctx) //nolint:errcheck // committed on success
	_, actor := acc.Actor()
	lifted, err := t.LiftFreeze(ctx, e.env.ID, params.ID, actor)
	switch {
	case err != nil:
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	case !lifted:
		return nil, kerrors.New(kerrors.NotFound, "freeze `%s` in force", params.ID)
	}
	if err := windowAudit(ctx, t, acc, e, "environment.freeze_lifted", params.ID.String()); err != nil {
		return nil, err
	}
	return &gen.LiftFreezeNoContent{}, t.Commit(ctx) //nolint:wrapcheck // a store error, answered as internal
}

// ListSilences is the environment's alert silences in force.
func (s *Server) ListSilences(ctx context.Context, params gen.ListSilencesParams) ([]gen.WindowDto, error) {
	_, e, t, err := s.environmentFor(ctx, perm.EnvRead, params.Project, params.Environment)
	if err != nil {
		return nil, err
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	now := s.deps.Clock.NowMs()
	found, err := t.Silences(ctx, e.env.ID, params.All.Or(false))
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	out := make([]gen.WindowDto, 0, len(found))
	for _, sl := range found {
		out = append(out, silenceDto(sl, now))
	}
	return out, nil
}

// CreateSilence silences the alerts of an environment, or of one of its
// apps.
func (s *Server) CreateSilence(ctx context.Context, req *gen.CreateWindow, params gen.CreateSilenceParams) (gen.CreateSilenceRes, error) {
	acc, e, t, err := s.environmentFor(ctx, perm.AppDeploy, params.Project, params.Environment)
	if err != nil {
		return nil, err
	}
	defer t.Rollback(ctx) //nolint:errcheck // committed on success
	if req.StartsAt.IsSet() && !req.StartsAt.IsNull() {
		return nil, kerrors.New(kerrors.Validation, "a silence starts now")
	}
	now := s.deps.Clock.NowMs()
	_, endsAt, err := controlWindow(req, now, maxSilenceMs)
	if err != nil {
		return nil, err
	}
	target := opt.None[ids.TargetID]()
	if app, ok := req.App.Get(); ok {
		rec, found, err := t.App(ctx, e.env.ID, app)
		switch {
		case err != nil:
			return nil, err //nolint:wrapcheck // a store error, answered as internal
		case !found:
			return nil, kerrors.New(kerrors.NotFound, "app `%s`", app)
		}
		target = opt.Some(rec.Target)
	}
	w, err := newWindow(acc, e, req.Reason, target, now, endsAt)
	if err != nil {
		return nil, err
	}
	id, err := t.CreateSilence(ctx, w)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if err := windowAudit(ctx, t, acc, e, "alerts.silenced", w.Reason); err != nil {
		return nil, err
	}
	all, err := t.Silences(ctx, e.env.ID, true)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	for _, sl := range all {
		if sl.ID == id {
			dto := silenceDto(sl, now)
			return &dto, t.Commit(ctx) //nolint:wrapcheck // a store error, answered as internal
		}
	}
	return nil, kerrors.New(kerrors.Internal, "the new silence is missing")
}

// LiftSilence lifts a silence early.
func (s *Server) LiftSilence(ctx context.Context, params gen.LiftSilenceParams) (gen.LiftSilenceRes, error) {
	acc, e, t, err := s.environmentFor(ctx, perm.AppDeploy, params.Project, params.Environment)
	if err != nil {
		return nil, err
	}
	defer t.Rollback(ctx) //nolint:errcheck // committed on success
	_, actor := acc.Actor()
	lifted, err := t.LiftSilence(ctx, e.env.ID, params.ID, actor)
	switch {
	case err != nil:
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	case !lifted:
		return nil, kerrors.New(kerrors.NotFound, "silence `%s` in force", params.ID)
	}
	if err := windowAudit(ctx, t, acc, e, "alerts.silence_lifted", params.ID.String()); err != nil {
		return nil, err
	}
	return &gen.LiftSilenceNoContent{}, t.Commit(ctx) //nolint:wrapcheck // a store error, answered as internal
}

// appFor is the app of a control route with perm checked, and a
// transaction of its organization.
func (s *Server) appFor(ctx context.Context, p perm.Perm, project, environment, app string) (access.Access, appScope, *store.Tenant, error) {
	acc, err := s.access(ctx)
	if err != nil {
		return access.Access{}, appScope{}, nil, err
	}
	a, err := s.findApp(ctx, acc, project, environment, app)
	if err != nil {
		return access.Access{}, appScope{}, nil, err
	}
	if _, err := acc.Require(p, a.chain()); err != nil {
		return access.Access{}, appScope{}, nil, err //nolint:wrapcheck // a kerrors already
	}
	t, err := s.deps.Store.Tenant(ctx, a.env.project.org)
	if err != nil {
		return access.Access{}, appScope{}, nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	return acc, a, t, nil
}

// PauseApp holds an app's delivery: new runs are accepted and wait.
func (s *Server) PauseApp(ctx context.Context, req *gen.Reason, params gen.PauseAppParams) (gen.PauseAppRes, error) {
	acc, a, t, err := s.appFor(ctx, perm.AppDeploy, params.Project, params.Environment, params.App)
	if err != nil {
		return nil, err
	}
	defer t.Rollback(ctx) //nolint:errcheck // committed on success
	reason, err := controlText("reason", req.Reason, 1024)
	if err != nil {
		return nil, err
	}
	_, actor := acc.Actor()
	paused, err := t.PauseTarget(ctx, a.app.Target, actor, reason)
	switch {
	case err != nil:
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	case !paused:
		return nil, kerrors.New(kerrors.Conflict, "app `%s` is paused already or being deleted", params.App)
	}
	if err := windowAudit(ctx, t, acc, a.env, "app.paused", reason); err != nil {
		return nil, err
	}
	return &gen.PauseAppNoContent{}, t.Commit(ctx) //nolint:wrapcheck // a store error, answered as internal
}

// ResumeApp resumes an app's delivery: its newest waiting run is written.
func (s *Server) ResumeApp(ctx context.Context, params gen.ResumeAppParams) (gen.ResumeAppRes, error) {
	acc, a, t, err := s.appFor(ctx, perm.AppDeploy, params.Project, params.Environment, params.App)
	if err != nil {
		return nil, err
	}
	defer t.Rollback(ctx) //nolint:errcheck // committed on success
	resumed, err := t.ResumeTarget(ctx, a.app.Target)
	switch {
	case err != nil:
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	case !resumed:
		return nil, kerrors.New(kerrors.Conflict, "app `%s` is not paused", params.App)
	}
	if err := windowAudit(ctx, t, acc, a.env, "app.resumed", params.App); err != nil {
		return nil, err
	}
	return &gen.ResumeAppNoContent{}, t.Commit(ctx) //nolint:wrapcheck // a store error, answered as internal
}

// EmergencyRollback returns an app to an earlier release now, past
// approvals, a freeze, the scan gate and a pause. People with approval
// rights only; the reason is kept.
func (s *Server) EmergencyRollback(ctx context.Context, req *gen.EmergencyRollback, params gen.EmergencyRollbackParams) (gen.EmergencyRollbackRes, error) {
	acc, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	if err := acc.ForbidToken(); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	a, err := s.findApp(ctx, acc, params.Project, params.Environment, params.App)
	if err != nil {
		return nil, err
	}
	for _, p := range []perm.Perm{perm.AppDeploy, perm.ReleaseApprove} {
		if _, err := acc.Require(p, a.chain()); err != nil {
			return nil, err //nolint:wrapcheck // a kerrors already
		}
	}
	why, err := controlText("reason", req.Reason, 1024)
	if err != nil {
		return nil, err
	}
	t, err := s.deps.Store.Tenant(ctx, a.env.project.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // committed on success
	summary, release, err := s.startEmergency(ctx, t, acc, a, params, why, req.Release)
	if err != nil {
		return nil, err
	}
	if err := t.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	s.deps.Logger.Warn("emergency rollback", "app", params.Project+"/"+params.Environment+"/"+params.App,
		"release", release.String(), "reason", why)
	dto := deploymentDto(summary)
	return &dto, nil
}

func (s *Server) startEmergency(ctx context.Context, t *store.Tenant, acc access.Access, a appScope,
	params gen.EmergencyRollbackParams, why string, wanted gen.OptNilUUID,
) (store.RunSummary, ids.ReleaseID, error) {
	release := opt.None[ids.ReleaseID]()
	if r, ok := wanted.Get(); ok {
		release = opt.Some(ids.From[ids.Release](r))
	}
	rel, config, found, err := t.RollbackPoint(ctx, a.app.Target, release)
	switch {
	case err != nil:
		return store.RunSummary{}, ids.ReleaseID{}, err //nolint:wrapcheck // a store error, answered as internal
	case !found:
		return store.RunSummary{}, ids.ReleaseID{}, kerrors.New(kerrors.NotFound, "an earlier release of `%s` that ran successfully", params.App)
	}
	_, actor := acc.Actor()
	expected := a.app.DesiredGeneration
	run := store.StartDeployment{
		Project: a.env.project.project.ID, Target: a.app.Target, Release: rel, ConfigRevision: config,
		ExpectedGeneration: expected, LifecycleUID: a.app.LifecycleUID, Reason: store.ReasonEmergency,
		RequestedBy: actor, InputHash: emergencyHash(rel, config, uint64(expected)),
	}
	record := requestAudit(acc, "deployment.emergency_rollback", "app", params.Project+"/"+params.Environment+"/"+params.App)
	record.Data = opt.Some[any](map[string]any{"reason": why, "release": rel.String()})
	started, err := t.StartEmergencyRollback(ctx, run, why, record)
	if err != nil {
		return store.RunSummary{}, ids.ReleaseID{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	var runID ids.DeploymentRunID
	switch st := started.(type) {
	case store.StartedAccepted:
		runID = st.Run
	case store.StartedRejected:
		return store.RunSummary{}, ids.ReleaseID{}, kerrors.New(kerrors.Conflict, "%s", st.Reject.Error())
	case store.StartedSecretRevoked:
		return store.RunSummary{}, ids.ReleaseID{}, errSecretRevoked()
	case store.StartedReplayed, store.StartedKeyReused, store.StartedNotFound, store.StartedVulnerabilityBlocked,
		store.StartedFrozen, store.StartedUntrusted:
		return store.RunSummary{}, ids.ReleaseID{}, kerrors.New(kerrors.Conflict, "the rollback was not accepted: %s", startedDebug(started))
	}
	summary, found, err := t.RunOfTarget(ctx, a.app.Target, runID)
	switch {
	case err != nil:
		return store.RunSummary{}, ids.ReleaseID{}, err //nolint:wrapcheck // a store error, answered as internal
	case !found:
		return store.RunSummary{}, ids.ReleaseID{}, kerrors.New(kerrors.Internal, "the accepted run is missing")
	}
	return summary, rel, nil
}

// startedDebug is a start result as Rust's Debug printed it.
func startedDebug(started store.Started) string {
	switch st := started.(type) {
	case store.StartedAccepted:
		return fmt.Sprintf("Accepted { operation: %s, run: %s, generation: Generation(%d), approvals_required: %d }",
			st.Operation, st.Run, uint64(st.Generation), st.ApprovalsRequired)
	case store.StartedReplayed:
		return fmt.Sprintf("Replayed { operation: %s }", st.Operation)
	case store.StartedKeyReused:
		return fmt.Sprintf("KeyReused { operation: %s }", st.Operation)
	case store.StartedRejected:
		return fmt.Sprintf("Rejected(%v)", st.Reject)
	case store.StartedNotFound:
		return "NotFound"
	case store.StartedSecretRevoked:
		return "SecretRevoked"
	case store.StartedVulnerabilityBlocked:
		return "VulnerabilityBlocked"
	case store.StartedFrozen:
		return "Frozen"
	case store.StartedUntrusted:
		return "Untrusted"
	}
	return "Unknown"
}
