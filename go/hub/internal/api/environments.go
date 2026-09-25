package api

// Environments of a project, on the SQL model (routes/environments.rs,
// ADR-032). SQL holds them and their placement; the materializer writes the
// Environment resource right away (its controller provisions the namespace)
// and removes it on deletion. The projection adds live status.

import (
	"context"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/access"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/gen"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/capacity"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/perm"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/policy"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/projection"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/render"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
)

// primaryCluster is the cluster every placement is on until multi-cluster
// (M1.9 and later).
const primaryCluster = "primary"

// environmentDto is EnvironmentDto::of.
func environmentDto(project string, e store.EnvironmentRecord, view opt.Val[projection.EnvironmentView]) gen.EnvironmentDto {
	resource := render.EnvironmentResourceName(project, e.Slug)
	v, seen := view.Get()
	phase := "Pending"
	if e.Deleting {
		phase = "Terminating"
	}
	if seen {
		phase = v.Phase.Or(phase)
	}
	message, scheduled := opt.None[string](), opt.None[string]()
	if seen {
		message, scheduled = v.Message, v.DeletionScheduledAt
	}
	return gen.EnvironmentDto{
		Name:                e.Slug,
		ResourceName:        resource,
		Project:             project,
		EnvType:             e.EnvType,
		Namespace:           e.Namespace.Or(render.NamespaceName(resource)),
		Phase:               gen.NewOptNilString(phase),
		Ready:               seen && v.Ready && !e.Deleting,
		Message:             optNilString(message),
		Deleting:            e.Deleting,
		DeletionScheduledAt: optNilString(scheduled),
		CreatedAt:           gen.NewOptNilString(Timestamp(e.CreatedAt)),
	}
}

// ListEnvironments lists a project's environments.
func (s *Server) ListEnvironments(ctx context.Context, params gen.ListEnvironmentsParams) (gen.ListEnvironmentsRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	p, err := s.findProject(ctx, a, params.Project)
	if err != nil {
		return nil, err
	}
	if _, err := a.Require(perm.EnvRead, p.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerr already
	}
	t, err := s.deps.Store.Tenant(ctx, p.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	environments, err := t.Environments(ctx, p.project.ID)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	items := gen.ListEnvironmentsOKApplicationJSON{}
	for _, e := range environments {
		view := s.environmentView(render.EnvironmentResourceName(p.project.Slug, e.Slug))
		items = append(items, environmentDto(p.project.Slug, e, view))
	}
	return &items, nil
}

// GetEnvironment is one environment.
func (s *Server) GetEnvironment(ctx context.Context, params gen.GetEnvironmentParams) (gen.GetEnvironmentRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	e, err := s.findEnvironment(ctx, a, params.Project, params.Environment)
	if err != nil {
		return nil, err
	}
	if _, err := a.Require(perm.EnvRead, e.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerr already
	}
	dto := environmentDto(e.project.project.Slug, e.env, e.view)
	return &dto, nil
}

// environmentQuota checks a requested quota and returns it as stored:
// admission reads the same quantities (M4.5).
func environmentQuota(input gen.OptNilQuotaInput) (opt.Val[any], error) {
	q, ok := input.Get()
	if !ok {
		return opt.None[any](), nil
	}
	quota := map[string]any{}
	if cpu, ok := q.CPU.Get(); ok {
		if err := Quantity("quota.cpu", cpu); err != nil {
			return opt.None[any](), err
		}
		if _, ok := capacity.CPUMillis(cpu); !ok {
			return opt.None[any](), kerr.New(kerr.Validation, "quota.cpu `%s` is not a CPU quantity", cpu)
		}
		quota["cpu"] = cpu
	}
	if memory, ok := q.Memory.Get(); ok {
		if err := Quantity("quota.memory", memory); err != nil {
			return opt.None[any](), err
		}
		if _, ok := capacity.Bytes(memory); !ok {
			return opt.None[any](), kerr.New(kerr.Validation, "quota.memory `%s` is not a memory quantity", memory)
		}
		quota["memory"] = memory
	}
	if pods, ok := q.Pods.Get(); ok {
		if pods < 0 {
			return opt.None[any](), kerr.New(kerr.Validation, "quota.pods must not be negative")
		}
		quota["pods"] = pods
	}
	return opt.Some[any](quota), nil
}

// CreateEnvironment creates an environment; its namespace follows.
func (s *Server) CreateEnvironment(
	ctx context.Context, req *gen.CreateEnvironment, params gen.CreateEnvironmentParams,
) (gen.CreateEnvironmentRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	p, err := s.findProject(ctx, a, params.Project)
	if err != nil {
		return nil, err
	}
	if _, err := a.Require(perm.EnvWrite, p.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerr already
	}
	if err := DNSLabel("name", req.Name, 20); err != nil {
		return nil, err
	}
	resource := render.EnvironmentResourceName(p.project.Slug, req.Name)
	namespace := render.NamespaceName(resource)
	if len(namespace) > 63 {
		return nil, kerr.New(kerr.Validation, "project and environment names are too long together")
	}
	quota, err := environmentQuota(req.Quota)
	if err != nil {
		return nil, err
	}
	if p.project.Deleting {
		return nil, kerr.New(kerr.Conflict, "project `%s` is being deleted", p.project.Slug)
	}
	// Environment resources are cluster-wide.
	taken := kerr.New(kerr.Conflict, "environment `%s` already exists", req.Name)
	if v, ok := s.deps.Projections.Environment(resource); ok && v.Org != opt.Some(p.org.String()) {
		return nil, taken
	}
	record, err := s.createEnvironment(ctx, a, p, req.Name, environmentKind(req.EnvType.Or(gen.EnvTypeStandard)), quota, namespace)
	if err != nil {
		return nil, err
	}
	dto := environmentDto(p.project.Slug, record, opt.None[projection.EnvironmentView]())
	return &dto, nil
}

// environmentKind is From<EnvType> for EnvironmentKind.
func environmentKind(t gen.EnvType) store.EnvironmentKind {
	switch t {
	case gen.EnvTypeProduction:
		return store.Production
	case gen.EnvTypePreview:
		return store.Preview
	case gen.EnvTypeStandard:
	}
	return store.Standard
}

// createEnvironment records the environment, its placement and first
// policy, and asks the materializer to write it, in one transaction.
func (s *Server) createEnvironment(
	ctx context.Context, a access.Access, p projectScope, name string, kind store.EnvironmentKind, quota opt.Val[any], namespace string,
) (store.EnvironmentRecord, error) {
	taken := kerr.New(kerr.Conflict, "environment `%s` already exists", name)
	what := "environment `" + name + "`"
	_, actor := a.Actor()
	t, err := s.deps.Store.Tenant(ctx, p.org)
	if err != nil {
		return store.EnvironmentRecord{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	if err := s.admitEnvironment(ctx, t); err != nil {
		return store.EnvironmentRecord{}, err
	}
	id, err := t.CreateEnvironmentTyped(ctx, p.project.ID, name, name, kind, quota)
	if err != nil {
		return store.EnvironmentRecord{}, duplicate(err, what)
	}
	cluster, err := t.EnsureCluster(ctx, primaryCluster)
	if err != nil {
		return store.EnvironmentRecord{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	if _, err := t.CreatePlacement(ctx, p.project.ID, id, cluster, namespace); err != nil {
		return store.EnvironmentRecord{}, duplicate(err, what)
	}
	// Protection is recorded, never inferred later: production starts with
	// one approval by someone other than the requester (M4.1).
	initial := policy.Initial(kind == store.Production)
	if _, set, err := t.SetEnvironmentPolicy(ctx, p.project.ID, id, initial, actor); err != nil || !set {
		return store.EnvironmentRecord{}, orConflict(err, taken)
	}
	resource := render.EnvironmentResourceName(p.project.Slug, name)
	if _, err := t.Request(ctx, store.EnvironmentApply, store.EnvironmentSubject(p.project.ID, id), actor,
		requestAudit(a, store.EnvironmentApply, "environment", resource)); err != nil {
		return store.EnvironmentRecord{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	record, found, err := t.Environment(ctx, p.project.ID, name)
	if err != nil || !found {
		return store.EnvironmentRecord{}, orConflict(err, taken)
	}
	if err := t.Commit(ctx); err != nil {
		return store.EnvironmentRecord{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	return record, nil
}

// orConflict is err, or conflict when there is none.
func orConflict(err, conflict error) error {
	if err != nil {
		return err
	}
	return conflict
}

// admitEnvironment is admission::admit_environment: the organization's
// environment quota (M4.5).
func (s *Server) admitEnvironment(ctx context.Context, t *store.Tenant) error {
	return t.AdmitEnvironment(ctx, s.deps.Config.Quota.OrgEnvironments) //nolint:wrapcheck // a kerr conflict or a store error
}

// DeleteEnvironment deletes an environment. Production environments need
// the `env-delete-protected` permission and are purged after a grace
// period.
func (s *Server) DeleteEnvironment(ctx context.Context, params gen.DeleteEnvironmentParams) (gen.DeleteEnvironmentRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	e, err := s.findEnvironment(ctx, a, params.Project, params.Environment)
	if err != nil {
		return nil, err
	}
	need := perm.EnvWrite
	if e.env.EnvType == "production" {
		need = perm.EnvDeleteProtected
	}
	if _, err := a.Require(need, e.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerr already
	}
	t, err := s.deps.Store.Tenant(ctx, e.project.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	// A preview deleted by hand is closed: its pull request's next event
	// does not bring it back (M5.1).
	if _, err := t.ClosePreview(ctx, e.env.ID, store.CloseDeleted, s.deps.Clock.NowMs()); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	marked, err := t.MarkEnvironmentDeleting(ctx, e.env.ID)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !marked {
		return nil, kerr.New(kerr.Conflict, "environment `%s` is being deleted", params.Environment)
	}
	_, actor := a.Actor()
	if _, err := t.Request(ctx, store.EnvironmentDelete, store.EnvironmentSubject(e.project.project.ID, e.env.ID), actor,
		requestAudit(a, store.EnvironmentDelete, "environment", e.resourceName())); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if err := t.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	return &gen.DeleteEnvironmentAccepted{}, nil
}
