package api

// Promotion of an app to another environment of the same project
// (scenario 10), with a dry-run diff (routes/apps/promote.rs). The target
// runs the source's release, digests and all: nothing is rebuilt.

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Teamtem-dev/kuben/api/v1alpha1"
	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/internal/httpapi/access"
	"github.com/Teamtem-dev/kuben/internal/httpapi/gen"
	secrets "github.com/Teamtem-dev/kuben/internal/keyring"
	"github.com/Teamtem-dev/kuben/internal/kube/projection"
	"github.com/Teamtem-dev/kuben/internal/store"
)

// promoteSpec is apps/promote.rs promote_spec: image, processes, health
// check and env come from the source; the target keeps its domains, its
// scaling (replicas and size per process) and its volumes.
func promoteSpec(source, target *v1alpha1.AppSpec) v1alpha1.AppSpec {
	out := source.DeepCopy()
	if target != nil {
		for name, p := range out.Runtime.Processes {
			if existing, ok := target.Runtime.Processes[name]; ok {
				p.Replicas = existing.Replicas
				p.Size = existing.Size
				out.Runtime.Processes[name] = p
			}
		}
		out.Domains = slices.Clone(target.Domains)
		if len(target.Volumes) > 0 {
			out.Volumes = slices.Clone(target.Volumes)
		}
	} else {
		out.Domains = nil
	}
	out.ImagePullSecrets = nil
	return *out
}

func fmtPort(port *uint16) string {
	if port == nil {
		return "none"
	}
	return strconv.FormatUint(uint64(*port), 10)
}

func portEqual(a, b *uint16) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if a != nil && *a != *b {
		return false
	}
	return true
}

func strPtrEqual(a, b *string) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if a != nil && *a != *b {
		return false
	}
	return true
}

func sourceEqual(a, b v1alpha1.Source) bool {
	if (a.Image == nil) != (b.Image == nil) {
		return false
	}
	if a.Image != nil && *a.Image != *b.Image {
		return false
	}
	return reflect.DeepEqual(a.Git, b.Git)
}

func imageOf(s *v1alpha1.AppSpec) string {
	if s.Source.Image != nil {
		return *s.Source.Image
	}
	return "(git build)"
}

// specChanges reports human-readable differences between two specs (no env values).
func specChanges(old, new *v1alpha1.AppSpec) []string {
	if old == nil {
		return []string{fmt.Sprintf("create the app with image %s", imageOf(new))}
	}
	out := []string{}
	if !sourceEqual(old.Source, new.Source) {
		out = append(out, fmt.Sprintf("image: %s → %s", imageOf(old), imageOf(new)))
	}
	out = append(out, envChanges(old.Env, new.Env)...)
	out = append(out, processChanges(old.Runtime.Processes, new.Runtime.Processes)...)
	if !reflect.DeepEqual(old.Runtime.HealthCheck, new.Runtime.HealthCheck) {
		out = append(out, "health check")
	}
	if !reflect.DeepEqual(old.Volumes, new.Volumes) {
		out = append(out, "volumes")
	}
	return out
}

// envByName is env keyed by variable name.
func envByName(env []v1alpha1.EnvVar) map[string]v1alpha1.EnvVar {
	out := make(map[string]v1alpha1.EnvVar, len(env))
	for _, e := range env {
		out[e.Name] = e
	}
	return out
}

// envChanges is the variables added, removed and changed, each group in
// name order.
func envChanges(oldEnv, newEnv []v1alpha1.EnvVar) []string {
	before, after := envByName(oldEnv), envByName(newEnv)
	afterKeys := slices.Sorted(maps.Keys(after))
	var out []string
	for _, name := range afterKeys {
		if _, ok := before[name]; !ok {
			out = append(out, fmt.Sprintf("env: add %s", name))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(before)) {
		if _, ok := after[name]; !ok {
			out = append(out, fmt.Sprintf("env: remove %s", name))
		}
	}
	for _, name := range afterKeys {
		if b, ok := before[name]; ok && !reflect.DeepEqual(b, after[name]) {
			out = append(out, fmt.Sprintf("env: change %s", name))
		}
	}
	return out
}

// processChanges is the processes added or changed, then those removed,
// in name order.
func processChanges(oldProcs, newProcs map[string]v1alpha1.Process) []string {
	var out []string
	for _, name := range slices.Sorted(maps.Keys(newProcs)) {
		p := newProcs[name]
		o, ok := oldProcs[name]
		if !ok {
			out = append(out, fmt.Sprintf("process %s: add", name))
			continue
		}
		if !slices.Equal(o.Command, p.Command) {
			out = append(out, fmt.Sprintf("process %s: command", name))
		}
		if !portEqual(o.Port, p.Port) {
			out = append(out, fmt.Sprintf("process %s: port %s → %s", name, fmtPort(o.Port), fmtPort(p.Port)))
		}
		if !strPtrEqual(o.Schedule, p.Schedule) {
			out = append(out, fmt.Sprintf("process %s: schedule", name))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(oldProcs)) {
		if _, ok := newProcs[name]; !ok {
			out = append(out, fmt.Sprintf("process %s: remove", name))
		}
	}
	return out
}

// missingSecrets reports secret references that the target environment cannot satisfy.
func missingSecrets(spec *v1alpha1.AppSpec, available map[string]map[string]struct{}) []string {
	out := []string{}
	for _, e := range spec.Env {
		if e.FromSecret == nil {
			continue
		}
		keys, ok := available[e.FromSecret.Name]
		if !ok {
			out = append(out, fmt.Sprintf("%s references secret `%s/%s`, which does not exist in the target environment",
				e.Name, e.FromSecret.Name, e.FromSecret.Key))
			continue
		}
		if _, hasKey := keys[e.FromSecret.Key]; !hasKey {
			out = append(out, fmt.Sprintf("%s references secret `%s/%s`, which does not exist in the target environment",
				e.Name, e.FromSecret.Name, e.FromSecret.Key))
		}
	}
	return out
}

// availableSecretKeys returns the keys of every secret target offers: its
// encrypted secrets with a usable current revision, and the Secrets Kuben
// wrote into its namespace before M4.4.
func (s *Server) availableSecretKeys(
	ctx context.Context, t *store.Tenant, target envScope,
) (map[string]map[string]struct{}, error) {
	stored, err := t.Secrets(ctx, target.env.ID)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	out := make(map[string]map[string]struct{}, len(stored))
	for _, sec := range stored {
		if sec.Revoked {
			continue
		}
		set := make(map[string]struct{}, len(sec.Keys))
		for _, k := range sec.Keys {
			set[k] = struct{}{}
		}
		out[sec.Name] = set
	}
	// Without a cluster, only the store's managed secrets are available.
	registry, ok := s.deps.Cluster.Get()
	if !ok || registry == nil {
		return out, nil
	}
	selector := fmt.Sprintf("%s,!%s", v1alpha1.ManagedSelector, secrets.SecretID)
	list, err := registry.Primary().Typed.CoreV1().Secrets(target.namespace()).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return nil, kubeError(err, "secrets")
	}
	for _, item := range list.Items {
		if _, exists := out[item.Name]; !exists {
			set := make(map[string]struct{}, len(item.Data))
			for k := range item.Data {
				set[k] = struct{}{}
			}
			out[item.Name] = set
		}
	}
	return out, nil
}

// PromoteApp promotes the app to another environment of the same project.
func (s *Server) PromoteApp(ctx context.Context, req *gen.Promote, params gen.PromoteAppParams) (gen.PromoteAppRes, error) {
	acc, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	a, err := s.findApp(ctx, acc, params.Project, params.Environment, params.App)
	if err != nil {
		return nil, err
	}
	if _, err := acc.Require(perm.AppRead, a.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	targetEnv, err := s.promotionTarget(ctx, acc, a, params.Project, req.ToEnvironment)
	if err != nil {
		return nil, err
	}
	source, ok := desiredSpec(a.app)
	if !ok {
		return nil, kerrors.New(kerrors.Conflict, "app `%s` has no configuration yet", params.App)
	}
	release, ok := a.app.Release.Get()
	if !ok {
		return nil, kerrors.New(kerrors.Conflict, "app `%s` has no release yet", params.App)
	}
	t, err := s.deps.Store.Tenant(ctx, a.env.project.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // no-op after commit
	p, err := s.planPromotion(ctx, t, a, targetEnv, &source)
	if err != nil {
		return nil, err
	}
	res := gen.PromoteResult{
		DryRun: req.DryRun.Or(false), Created: !p.found, Changes: p.changes, Warnings: p.warnings,
	}
	if res.DryRun || (p.found && len(p.changes) == 0) {
		res.App.SetToNull()
		return &res, nil
	}
	c := change{
		project: a.env.project.project.ID, application: a.app.Application, spec: &p.spec,
		artifact: releaseArtifact{id: release}, reason: store.ReasonPromotion,
		reference: params.Project + "/" + targetEnv.env.Slug + "/" + params.App,
		chain:     targetEnv.chain(), environment: targetEnv.env.ID, quota: targetEnv.env.Quota,
	}
	if p.found {
		c.target, c.expected = p.existing.Target, p.existing.DesiredGeneration
	} else if c.target, err = createPromotionTarget(ctx, t, targetEnv, c.project, c.application, params.App); err != nil {
		return nil, err
	}
	dto, err := s.deployPromotion(ctx, t, acc, c, targetEnv, a.app.Slug, params.Project)
	if err != nil {
		return nil, err
	}
	res.App = gen.NewOptNilAppDto(dto)
	return &res, nil
}

// promotion is what promoting an app to an environment changes there.
type promotion struct {
	// existing is the app in the target environment, when found.
	existing store.AppRecord
	found    bool
	spec     v1alpha1.AppSpec
	changes  []string
	warnings []string
}

// planPromotion is what promoting app a with its spec source to targetEnv
// would do.
func (s *Server) planPromotion(
	ctx context.Context, t *store.Tenant, a appScope, targetEnv envScope, source *v1alpha1.AppSpec,
) (promotion, error) {
	var p promotion
	var err error
	p.existing, p.found, err = t.App(ctx, targetEnv.env.ID, a.app.Slug)
	if err != nil {
		return p, err //nolint:wrapcheck // a store error, answered as internal
	}
	var current *v1alpha1.AppSpec
	if p.found {
		if cur, ok := desiredSpec(p.existing); ok {
			current = &cur
		}
	}
	p.spec = promoteSpec(source, current)
	if err := validateSpec(&p.spec); err != nil {
		return p, err
	}
	p.changes = specChanges(current, &p.spec)
	avail, err := s.availableSecretKeys(ctx, t, targetEnv)
	if err != nil {
		return p, err
	}
	p.warnings = missingSecrets(&p.spec, avail)
	return p, nil
}

// deployPromotion deploys c, commits t and returns the app slug as it is
// then in targetEnv.
func (s *Server) deployPromotion(
	ctx context.Context, t *store.Tenant, acc access.Access, c change, targetEnv envScope, slug, project string,
) (gen.AppDto, error) {
	if err := s.deploy(ctx, t, acc, c); err != nil {
		return gen.AppDto{}, err
	}
	record, found, err := t.App(ctx, targetEnv.env.ID, slug)
	if err != nil {
		return gen.AppDto{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found {
		return gen.AppDto{}, kerrors.New(kerrors.Internal, "the promoted app is missing")
	}
	if err := t.Commit(ctx); err != nil {
		return gen.AppDto{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	return appDto(project, targetEnv.env.Slug, record, opt.None[projection.AppView]()), nil
}

// promotionTarget is the environment toEnvironment of project that a
// promotes to: another environment, one the caller may promote to, and not
// being deleted (with its project).
func (s *Server) promotionTarget(
	ctx context.Context, acc access.Access, a appScope, project, toEnvironment string,
) (envScope, error) {
	if err := DNSLabel("to_environment", toEnvironment, 63); err != nil {
		return envScope{}, err
	}
	targetEnv, err := s.findEnvironment(ctx, acc, project, toEnvironment)
	if err != nil {
		return envScope{}, err
	}
	if targetEnv.env.ID == a.env.env.ID {
		return envScope{}, kerrors.New(kerrors.Validation, "choose a different target environment")
	}
	if _, err := acc.Require(perm.ReleasePromote, targetEnv.chain()); err != nil {
		return envScope{}, err //nolint:wrapcheck // a kerrors already
	}
	if targetEnv.deleting() {
		return envScope{}, kerrors.New(kerrors.Conflict, "environment `%s` is being deleted", targetEnv.env.Slug)
	}
	return targetEnv, nil
}

// createPromotionTarget records the target of application on
// targetEnv's placement, for an app promoted there for the first time.
func createPromotionTarget(
	ctx context.Context, t *store.Tenant, targetEnv envScope, project ids.ProjectID, application ids.ApplicationID, app string,
) (ids.TargetID, error) {
	placement, ok := targetEnv.env.Placement.Get()
	if !ok {
		return ids.TargetID{}, kerrors.New(kerrors.Conflict, "environment `%s` has no placement", targetEnv.env.Slug)
	}
	id, err := t.CreateTarget(ctx, project, application, placement)
	if err != nil {
		return ids.TargetID{}, duplicate(err, "app `"+app+"`")
	}
	return id, nil
}
