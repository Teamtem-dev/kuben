package api

// Apps of an environment on the SQL model (routes/apps/mod.rs, ADR-032).
//
// An app is a target: one application on its environment's placement. Its
// desired state is its newest configuration revision (the App spec without
// its image) and the release of its newest deployment run. Every change is a
// new run, which the materializer writes and the controllers carry out; the
// projection adds live status (for an app its cluster's agent delivers, the
// agent's last report, from SQL). An image given as a tag is resolved to a
// digest at its registry first (option A).

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/access"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/gen"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/artifact"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/authz"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ops/target"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/integrations/oci"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/projection"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
	"github.com/Teamtem-dev/kuben/go/hub/internal/wire"
	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

// optNilBool is v as a nullable member: null when absent (Rust wrote
// `null` for a None without skip_serializing_if).
func optNilBool(v opt.Val[bool]) gen.OptNilBool {
	if b, ok := v.Get(); ok {
		return gen.NewOptNilBool(b)
	}
	var n gen.OptNilBool
	n.SetToNull()
	return n
}

func optNilPort(v opt.Val[uint16]) gen.OptNilInt32 {
	if p, ok := v.Get(); ok {
		return gen.NewOptNilInt32(int32(p))
	}
	var n gen.OptNilInt32
	n.SetToNull()
	return n
}

func nullExposure() gen.OptNilExposureDto {
	var n gen.OptNilExposureDto
	n.SetToNull()
	return n
}

// appDtoFromView is AppDto::from_view.
func appDtoFromView(v projection.AppView, project, environment string) gen.AppDto {
	dto := gen.AppDto{
		Name: v.Name, Project: project, Environment: environment, Namespace: v.Namespace,
		Image: optNilString(v.Image), GitRepo: optNilString(v.GitRepo), URL: optNilString(v.URL),
		Ready: v.Ready, Reason: optNilString(v.Reason), Message: optNilString(v.Message),
		Processes: []gen.ProcessDto{}, Env: []gen.EnvVarDto{}, Domains: slices.Clone(v.Domains),
		Volumes: []gen.VolumeDto{}, CreatedAt: optNilString(v.CreatedAt), Exposure: nullExposure(),
	}
	if dto.Domains == nil {
		dto.Domains = []string{}
	}
	for _, p := range v.Processes {
		dto.Processes = append(dto.Processes, gen.ProcessDto{
			Name: p.Name, Command: nonNil(p.Command), Port: optNilPort(p.Port), Size: p.Size,
			MinReplicas: int32(min(p.MinReplicas, 1<<31-1)), //nolint:gosec // bounded
			MaxReplicas: int32(min(p.MaxReplicas, 1<<31-1)), //nolint:gosec // bounded
			Schedule:    optNilString(p.Schedule), Protocol: p.Protocol,
		})
	}
	for _, e := range v.Env {
		env := gen.EnvVarDto{Name: e.Name}
		if s, ok := e.Secret.Get(); ok {
			if name, key, found := strings.Cut(s, "/"); found {
				env.Secret = gen.NewOptNilSecretRef(gen.SecretRef{Name: name, Key: key})
			}
		}
		dto.Env = append(dto.Env, env)
	}
	for _, vol := range v.Volumes {
		dto.Volumes = append(dto.Volumes, gen.VolumeDto{Name: vol.Name, MountPath: vol.MountPath, Size: gen.NewOptString(vol.Size)})
	}
	return dto
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// appDto is AppDto::of: the app record's desired state from SQL, its live
// status from view (none until the materializer wrote it), or for an app
// its cluster's agent delivers, from the agent's last report (M1.9).
func appDto(project, environment string, record store.AppRecord, view opt.Val[projection.AppView]) gen.AppDto {
	spec, ok := desiredSpec(record)
	if !ok {
		spec = v1alpha1.AppSpec{Runtime: v1alpha1.Runtime{Processes: map[string]v1alpha1.Process{}}}
	}
	desired := v1alpha1.App{Spec: spec}
	desired.Name, desired.Namespace = record.Slug, record.Namespace
	dto := appDtoFromView(projection.AppViewOf(&desired), project, environment)
	dto.Image = optNilString(record.Image)
	if record.Delivery == store.DeliveryAgent {
		// No App object: the agent reports what it carried out.
		runtime, reported := record.Runtime.Get()
		dto.URL, dto.Reason, dto.Message = optNilString(opt.None[string]()), optNilString(opt.None[string]()),
			optNilString(opt.None[string]())
		dto.Ready = reported && runtime.Ready() && !record.Deleting
		if reported {
			dto.URL, dto.Reason, dto.Message = optNilString(runtime.URL), optNilString(runtimeReason(runtime)),
				optNilString(runtime.Message)
		}
	} else {
		v, seen := view.Get()
		dto.URL, dto.Reason, dto.Message = optNilString(opt.None[string]()), optNilString(opt.None[string]()),
			optNilString(opt.None[string]())
		dto.Ready = seen && v.Ready && !record.Deleting
		if seen {
			dto.URL, dto.Reason, dto.Message = optNilString(v.URL), optNilString(v.Reason), optNilString(v.Message)
		}
	}
	dto.CreatedAt = gen.NewOptNilString(Timestamp(record.CreatedAt))
	if p, ok := record.Paused.Get(); ok {
		dto.Paused = gen.NewOptNilString(p.Reason)
	}
	return dto
}

// runtimeReason is why an agent-delivered app is where it is: the agent's
// reason, else the phase it reported while not ready.
func runtimeReason(r store.RuntimeStatus) opt.Val[string] {
	if reason, ok := r.Reason.Get(); ok {
		return opt.Some(reason)
	}
	if r.Ready() {
		return opt.None[string]()
	}
	phases := map[string]string{"accepted": "Accepted", "applying": "Applying", "failed": "Failed", "rejected": "Rejected"}
	if p, ok := phases[r.Phase]; ok {
		return opt.Some(p)
	}
	return opt.Some("Unknown")
}

// withExposure adds what the cluster says about reaching the app.
func (s *Server) withExposure(dto gen.AppDto) gen.AppDto {
	v, ok := s.deps.Projections.Exposure(dto.Namespace, dto.Name)
	if !ok {
		return dto
	}
	exposure := gen.ExposureDto{Routed: optNilBool(v.Accepted), Message: optNilString(v.Message), Hosts: []gen.HostDto{}}
	for _, h := range v.Hosts {
		exposure.Hosts = append(exposure.Hosts, gen.HostDto{
			Host: h.Host, TLS: h.TLS, CertificateReady: optNilBool(h.CertificateReady),
			CertificateMessage: optNilString(h.CertificateMessage),
		})
	}
	dto.Exposure = gen.NewOptNilExposureDto(exposure)
	return dto
}

// podDto is PodDto::from.
func podDto(p *projection.PodView) gen.PodDto {
	return gen.PodDto{
		Name: p.Name, Process: optNilString(p.Process), Phase: string(p.Phase), Ready: p.Ready,
		Restarts: p.Restarts, Reason: optNilString(p.Reason), Node: optNilString(p.Node),
		StartedAt: optNilString(p.StartedAt),
	}
}

// desiredSpec is the app's desired spec: its newest configuration with the
// image of its newest release, as it was given.
func desiredSpec(record store.AppRecord) (v1alpha1.AppSpec, bool) {
	return specOf(record.Config, record.Image)
}

// configOf is the configuration revision of spec: the App spec without its
// image, the contract the materializer renders from.
func configOf(spec *v1alpha1.AppSpec) (any, error) {
	data, err := json.Marshal(spec)
	if err != nil {
		return nil, kerr.Wrap(err, "an app spec")
	}
	config, err := wire.DecodeAny(data)
	if err != nil {
		return nil, kerr.Wrap(err, "an app spec")
	}
	if object, ok := config.(map[string]any); ok {
		delete(object, "source")
	}
	return config, nil
}

// appScope is AppScope: a live app of an environment.
type appScope struct {
	env envScope
	app store.AppRecord
	// view is the app's projection, once the materializer wrote it.
	view opt.Val[projection.AppView]
}

func (a appScope) chain() authz.ScopeChain {
	c := a.env.chain()
	c.App = opt.Some(a.app.Target.UUID())
	if legacy, ok := a.app.LegacyUID.Get(); ok {
		c.Aliases = append(c.Aliases, authz.ScopeRef{Level: authz.LevelApp, ID: legacy})
	}
	return c
}

// reference is `project/environment/app`, for audit records.
func (a appScope) reference() string {
	return a.env.project.project.Slug + "/" + a.env.env.Slug + "/" + a.app.Slug
}

// findApp is scope::app: app of environment env of project.
func (s *Server) findApp(ctx context.Context, a access.Access, project, env, app string) (appScope, error) {
	e, err := s.findEnvironment(ctx, a, project, env)
	if err != nil {
		return appScope{}, err
	}
	t, err := s.deps.Store.Tenant(ctx, e.project.org)
	if err != nil {
		return appScope{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	record, found, err := t.App(ctx, e.env.ID, app)
	if err != nil {
		return appScope{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found {
		return appScope{}, scopeNotFound("app", app)
	}
	return appScope{env: e, app: record, view: s.appView(record.Namespace, record.Slug)}, nil
}

func (s *Server) appView(namespace, name string) opt.Val[projection.AppView] {
	v, ok := s.deps.Projections.App(namespace, name)
	if !ok {
		return opt.None[projection.AppView]()
	}
	return opt.Some(*v)
}

// resolve resolves image to a digest at its registry (option A), pulling
// with e's login for that registry when it has one.
func (s *Server) resolve(ctx context.Context, e envScope, image string) (oci.Resolved, error) {
	login, err := s.imageLogin(ctx, e, image)
	if err != nil {
		return oci.Resolved{}, err
	}
	resolved, err := s.deps.Images.ResolveAs(ctx, image, login)
	switch {
	case err == nil:
		return resolved, nil
	case oci.IsUnreachable(err) || oci.IsRateLimited(err):
		// Rust met a rate limit as an unreachable registry: 503 either way.
		return oci.Resolved{}, kerr.New(kerr.Unavailable, "%s", err.Error())
	}
	return oci.Resolved{}, kerr.New(kerr.Validation, "%s", err.Error())
}

// imageLogin is e's login for the registry of image, when image is a tag
// (a digest needs no registry) and e has one.
func (s *Server) imageLogin(ctx context.Context, e envScope, image string) (opt.Val[oci.Login], error) {
	reference, err := oci.Parse(image)
	if err != nil {
		return opt.None[oci.Login](), nil //nolint:nilerr // the resolver reports the malformed reference
	}
	if _, isTag := reference.Reference.(oci.Tag); !isTag {
		return opt.None[oci.Login](), nil
	}
	t, err := s.deps.Store.Tenant(ctx, e.project.org)
	if err != nil {
		return opt.None[oci.Login](), err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	login, err := s.registryLogin(ctx, t, e, reference.Registry)
	if err != nil {
		return opt.None[oci.Login](), err
	}
	l, ok := login.Get()
	if !ok {
		return opt.None[oci.Login](), nil
	}
	return opt.Some(oci.Login{Username: l.Username, Password: l.Password}), nil
}

// deployArtifact is what a deployment runs (apps/mod.rs Artifact): a newly
// resolved image, or an existing release.
//
//sumtype:decl
type deployArtifact interface {
	isDeployArtifact()
}

// resolvedArtifact is Artifact::Resolved: an image resolved for this
// deployment, released anew.
type resolvedArtifact struct {
	image oci.Resolved
}

// releaseArtifact is Artifact::Release: an existing release.
type releaseArtifact struct {
	id ids.ReleaseID
}

func (resolvedArtifact) isDeployArtifact() {}
func (releaseArtifact) isDeployArtifact()  {}

// change is one change of an app, to record and deploy.
type change struct {
	project     ids.ProjectID
	application ids.ApplicationID
	target      ids.TargetID
	spec        *v1alpha1.AppSpec
	artifact    deployArtifact
	// expected is the target generation the caller saw.
	expected target.Generation
	reason   store.RunReason
	// reference is `project/environment/app`, for the audit record.
	reference string
	// chain is where the change lands, for the environment's deploy role.
	chain authz.ScopeChain
	// environment and quota are for admission.
	environment ids.EnvironmentID
	quota       opt.Val[any]
}

// deploy records c as the app's newest configuration and starts a
// deployment run of it, in t's transaction.
func (s *Server) deploy(ctx context.Context, t *store.Tenant, a access.Access, c change) error {
	if err := ensureMayDeploy(ctx, t, a, c.target, c.chain); err != nil {
		return err
	}
	warnings, err := s.admit(ctx, t, admissionPlacement{environment: c.environment, quota: c.quota, target: c.target}, c.spec)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		s.deps.Logger.Info("admitted with a warning", "target", c.reference, "warning", w)
	}
	_, actor := a.Actor()
	config, err := configOf(c.spec)
	if err != nil {
		return err
	}
	missing := kerr.New(kerr.NotFound, "the app")
	revision, found, err := t.CreateConfigRevision(ctx, c.project, c.target, config, actor)
	if err != nil {
		return err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found {
		return missing
	}
	release, err := s.releaseOf(ctx, t, c, actor)
	if err != nil {
		return err
	}
	if c.reason.CarriesNewCode() {
		warnings, err := s.scanGate(ctx, t, c.target, release)
		if err != nil {
			return err
		}
		for _, w := range warnings {
			s.deps.Logger.Info("deployed with a vulnerability warning", "target", c.reference, "warning", w)
		}
	}
	state, found, err := t.TargetState(ctx, c.target)
	if err != nil {
		return err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found {
		return missing
	}
	hash := sha256.Sum256(fmt.Appendf(nil, "%s/%s/%d", release, revision.ID, uint64(c.expected)))
	started, err := t.StartDeployment(ctx, store.StartDeployment{
		Project: c.project, Target: c.target, Release: release, ConfigRevision: revision.ID,
		ExpectedGeneration: c.expected, LifecycleUID: state.LifecycleUID, Reason: c.reason,
		RequestedBy: actor, InputHash: hash[:],
	}, requestAudit(a, "deployment.accepted", "app", c.reference), opt.None[store.IdempotencyKey]())
	if err != nil {
		return err //nolint:wrapcheck // a store error, answered as internal
	}
	return startedErr(started)
}

// releaseOf is the release c deploys: the one given, or a new release of
// the resolved image.
func (s *Server) releaseOf(ctx context.Context, t *store.Tenant, c change, actor string) (ids.ReleaseID, error) {
	var image oci.Resolved
	switch a := c.artifact.(type) {
	case releaseArtifact:
		return a.id, nil
	case resolvedArtifact:
		image = a.image
	case nil:
		return ids.ReleaseID{}, kerr.Wrap(nil, "a change without an artifact")
	}
	id, _, err := t.CreateRelease(ctx, c.project, store.PortableRelease{
		Application:     c.application,
		Artifacts:       map[string]artifact.Digest{webProcess: image.Digest},
		ProcessContract: map[string]any{},
		PortableConfig:  map[string]any{},
		RendererSchema:  1,
		Source:          opt.Some[any](map[string]any{"image_repository": image.Repository, "image": image.Given}),
		CreatedBy:       actor,
	})
	return id, err //nolint:wrapcheck // a store error, answered as internal
}

// startedErr is the answer to a run the API started without an
// idempotency key.
func startedErr(started store.Started) error {
	switch st := started.(type) {
	case store.StartedAccepted, store.StartedReplayed:
		return nil
	case store.StartedRejected:
		return kerr.New(kerr.Conflict, "%s", st.Reject.Error())
	case store.StartedNotFound:
		return kerr.New(kerr.NotFound, "that release of this app")
	case store.StartedSecretRevoked:
		return errSecretRevoked()
	case store.StartedVulnerabilityBlocked:
		return kerr.New(kerr.Conflict, "the environment's vulnerability gate refuses this release")
	case store.StartedFrozen:
		return kerr.New(kerr.Conflict, "the environment is frozen: only an emergency rollback passes until the freeze ends")
	case store.StartedUntrusted:
		return errUntrusted()
	case store.StartedKeyReused:
		return kerr.Wrap(nil, "a deployment without a key was a replay")
	}
	return kerr.Wrap(nil, "an unknown start result")
}

// errSecretRevoked refuses a run because a secret it references has a
// revoked current revision.
func errSecretRevoked() error {
	return kerr.New(kerr.Conflict, "a secret this app references has its current revision revoked: set a new value first")
}

// ensureMayDeploy refuses a deployment a may not start on tgt under its
// environment's policy (approvals.rs ensure_may_deploy). Environments
// without a policy follow the role check the route made already.
func ensureMayDeploy(ctx context.Context, t *store.Tenant, a access.Access, tgt ids.TargetID, chain authz.ScopeChain) error {
	revision, found, err := t.PolicyOfTarget(ctx, tgt)
	if err != nil {
		return err //nolint:wrapcheck // a store error, answered as internal
	}
	if found && !revision.Policy.MayDeploy(authz.EffectiveRole(a.Subject, chain)) {
		return kerr.ErrForbidden
	}
	return nil
}

// createApp creates the app name from spec in environment e (shared by
// createApp and deployTemplate). An app of that name in another
// environment of the project is the same application.
func (s *Server) createApp(ctx context.Context, a access.Access, e envScope, name string, spec v1alpha1.AppSpec) (gen.AppDto, error) {
	if err := validateSpec(&spec); err != nil {
		return gen.AppDto{}, err
	}
	if e.deleting() {
		return gen.AppDto{}, kerr.New(kerr.Conflict, "environment `%s` is being deleted", e.env.Slug)
	}
	namespace := e.namespace()
	if err := s.ensureDomainsFree(ctx, e.project.org, namespace, name, &spec); err != nil {
		return gen.AppDto{}, err
	}
	if spec.Source.Image == nil {
		return gen.AppDto{}, kerr.New(kerr.Validation, "an app needs an image")
	}
	resolved, err := s.resolve(ctx, e, *spec.Source.Image)
	if err != nil {
		return gen.AppDto{}, err
	}
	placement, ok := e.env.Placement.Get()
	if !ok {
		return gen.AppDto{}, kerr.New(kerr.Conflict, "environment `%s` has no placement", e.env.Slug)
	}
	t, err := s.deps.Store.Tenant(ctx, e.project.org)
	if err != nil {
		return gen.AppDto{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	record, err := s.newApp(ctx, t, a, e, name, placement, &spec, resolved)
	if err != nil {
		return gen.AppDto{}, err
	}
	if err := t.Commit(ctx); err != nil {
		return gen.AppDto{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	return appDto(e.project.project.Slug, e.env.Slug, record, opt.None[projection.AppView]()), nil
}

// newApp records the application (or finds it), its target on placement
// and the first run, in t.
func (s *Server) newApp(
	ctx context.Context, t *store.Tenant, a access.Access, e envScope, name string, placement ids.PlacementID,
	spec *v1alpha1.AppSpec, resolved oci.Resolved,
) (store.AppRecord, error) {
	project := e.project.project.ID
	what := "app `" + name + "`"
	application, found, err := t.Application(ctx, project, name)
	if err != nil {
		return store.AppRecord{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found {
		if application, err = t.CreateApplication(ctx, project, name, name); err != nil {
			return store.AppRecord{}, duplicate(err, what)
		}
	}
	tgt, err := t.CreateTarget(ctx, project, application, placement)
	if err != nil {
		return store.AppRecord{}, duplicate(err, what)
	}
	if err := s.deploy(ctx, t, a, change{
		project: project, application: application, target: tgt, spec: spec,
		artifact: resolvedArtifact{image: resolved}, reason: store.ReasonDeploy,
		reference: e.project.project.Slug + "/" + e.env.Slug + "/" + name, chain: e.chain(),
		environment: e.env.ID, quota: e.env.Quota,
	}); err != nil {
		return store.AppRecord{}, err
	}
	record, found, err := t.App(ctx, e.env.ID, name)
	if err != nil {
		return store.AppRecord{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found {
		return store.AppRecord{}, kerr.Wrap(nil, "the new app is missing")
	}
	return record, nil
}
