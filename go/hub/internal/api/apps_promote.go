package api

// Promotion of an app to another environment of the same project
// (scenario 10), with a dry-run diff (routes/apps/promote.rs). The target
// runs the source's release, digests and all: nothing is rebuilt.

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strconv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/gen"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ops/target"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/perm"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/projection"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
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
	beforeEnv := make(map[string]v1alpha1.EnvVar, len(old.Env))
	for _, e := range old.Env {
		beforeEnv[e.Name] = e
	}
	afterEnv := make(map[string]v1alpha1.EnvVar, len(new.Env))
	for _, e := range new.Env {
		afterEnv[e.Name] = e
	}
	afterKeys := make([]string, 0, len(afterEnv))
	for k := range afterEnv {
		afterKeys = append(afterKeys, k)
	}
	slices.Sort(afterKeys)
	beforeKeys := make([]string, 0, len(beforeEnv))
	for k := range beforeEnv {
		beforeKeys = append(beforeKeys, k)
	}
	slices.Sort(beforeKeys)

	for _, name := range afterKeys {
		if _, ok := beforeEnv[name]; !ok {
			out = append(out, fmt.Sprintf("env: add %s", name))
		}
	}
	for _, name := range beforeKeys {
		if _, ok := afterEnv[name]; !ok {
			out = append(out, fmt.Sprintf("env: remove %s", name))
		}
	}
	for _, name := range afterKeys {
		if b, ok := beforeEnv[name]; ok {
			if !reflect.DeepEqual(b, afterEnv[name]) {
				out = append(out, fmt.Sprintf("env: change %s", name))
			}
		}
	}

	newProcKeys := make([]string, 0, len(new.Runtime.Processes))
	for k := range new.Runtime.Processes {
		newProcKeys = append(newProcKeys, k)
	}
	slices.Sort(newProcKeys)
	for _, name := range newProcKeys {
		p := new.Runtime.Processes[name]
		o, ok := old.Runtime.Processes[name]
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

	oldProcKeys := make([]string, 0, len(old.Runtime.Processes))
	for k := range old.Runtime.Processes {
		oldProcKeys = append(oldProcKeys, k)
	}
	slices.Sort(oldProcKeys)
	for _, name := range oldProcKeys {
		if _, ok := new.Runtime.Processes[name]; !ok {
			out = append(out, fmt.Sprintf("process %s: remove", name))
		}
	}

	if !reflect.DeepEqual(old.Runtime.HealthCheck, new.Runtime.HealthCheck) {
		out = append(out, "health check")
	}
	if !reflect.DeepEqual(old.Volumes, new.Volumes) {
		out = append(out, "volumes")
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
	secrets, err := t.Secrets(ctx, target.env.ID)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	out := make(map[string]map[string]struct{}, len(secrets))
	for _, sec := range secrets {
		if sec.Revoked {
			continue
		}
		set := make(map[string]struct{}, len(sec.Keys))
		for _, k := range sec.Keys {
			set[k] = struct{}{}
		}
		out[sec.Name] = set
	}
	cluster, err := s.cluster()
	if err != nil {
		// Without a cluster, only the store's managed secrets are available.
		return out, nil
	}
	selector := fmt.Sprintf("%s,!%s", v1alpha1.ManagedSelector, "kuben.dev/secret-id")
	list, err := cluster.Typed.CoreV1().Secrets(target.namespace()).List(ctx, metav1.ListOptions{
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
		return nil, err //nolint:wrapcheck // a kerr already
	}
	if err := DNSLabel("to_environment", req.ToEnvironment, 63); err != nil {
		return nil, err
	}
	targetEnv, err := s.findEnvironment(ctx, acc, params.Project, req.ToEnvironment)
	if err != nil {
		return nil, err
	}
	if targetEnv.env.ID == a.env.env.ID {
		return nil, kerr.New(kerr.Validation, "choose a different target environment")
	}
	if _, err := acc.Require(perm.ReleasePromote, targetEnv.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerr already
	}
	if targetEnv.env.Deleting {
		return nil, kerr.New(kerr.Conflict, "environment `%s` is being deleted", targetEnv.env.Slug)
	}
	source, ok := desiredSpec(a.app)
	if !ok {
		return nil, kerr.New(kerr.Conflict, "app `%s` has no configuration yet", params.App)
	}
	release, ok := a.app.Release.Get()
	if !ok {
		return nil, kerr.New(kerr.Conflict, "app `%s` has no release yet", params.App)
	}
	t, err := s.deps.Store.Tenant(ctx, a.env.project.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // no-op after commit
	existing, found, err := t.App(ctx, targetEnv.env.ID, a.app.Slug)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	var current *v1alpha1.AppSpec
	if found {
		if cur, ok := desiredSpec(existing); ok {
			current = &cur
		}
	}
	spec := promoteSpec(&source, current)
	if err := validateSpec(&spec); err != nil {
		return nil, err
	}
	changes := specChanges(current, &spec)
	avail, err := s.availableSecretKeys(ctx, t, targetEnv)
	if err != nil {
		return nil, err
	}
	warnings := missingSecrets(&spec, avail)
	dryRun := req.DryRun.Or(false)
	if dryRun || (found && len(changes) == 0) {
		res := gen.PromoteResult{
			DryRun:   dryRun,
			Created:  !found,
			Changes:  changes,
			Warnings: warnings,
		}
		res.App.SetToNull()
		return &res, nil
	}
	projectID := a.env.project.project.ID
	var targetID ids.TargetID
	var expected target.Generation
	var created bool
	if found {
		targetID = existing.Target
		expected = existing.DesiredGeneration
		created = false
	} else {
		placement, ok := targetEnv.env.Placement.Get()
		if !ok {
			return nil, kerr.New(kerr.Conflict, "environment `%s` has no placement", targetEnv.env.Slug)
		}
		id, err := t.CreateTarget(ctx, projectID, a.app.Application, placement)
		if err != nil {
			return nil, duplicate(err, "app `"+params.App+"`")
		}
		targetID = id
		expected = target.Generation(0)
		created = true
	}
	c := change{
		project:     projectID,
		application: a.app.Application,
		target:      targetID,
		spec:        &spec,
		artifact:    deployArtifact{release: opt.Some(release)},
		expected:    expected,
		reason:      store.ReasonPromotion,
		reference:   params.Project + "/" + targetEnv.env.Slug + "/" + params.App,
		chain:       targetEnv.chain(),
		environment: targetEnv.env.ID,
		quota:       targetEnv.env.Quota,
	}
	if err := s.deploy(ctx, t, acc, c); err != nil {
		return nil, err
	}
	record, found, err := t.App(ctx, targetEnv.env.ID, a.app.Slug)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found {
		return nil, kerr.New(kerr.Internal, "the promoted app is missing")
	}
	if err := t.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	dto := appDto(params.Project, targetEnv.env.Slug, record, opt.None[projection.AppView]())
	res := gen.PromoteResult{
		DryRun:   false,
		Created:  created,
		Changes:  changes,
		Warnings: warnings,
		App:      gen.NewOptNilAppDto(dto),
	}
	return &res, nil
}
