package httpapi

// Request bodies for creating and updating apps, their validation, and the
// translation into an App spec (routes/apps/spec.rs).

import (
	"context"
	"errors"
	"math"
	"math/big"
	"slices"
	"strings"
	"unicode"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ascii"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/dnsname"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/render"
	"github.com/Teamtem-dev/kuben/packages/api/v1alpha1"
)

const (
	// webProcess is the process created for single-process web apps.
	webProcess = "web"
	// jobProcess is the process created for scheduled apps.
	jobProcess     = "job"
	maxAppReplicas = 50
	maxAppVolumes  = 5
)

// trimSpace is Rust's str::trim (Unicode White_Space).
func trimSpace(s string) string { return strings.TrimFunc(s, unicode.IsSpace) }

// nonEmpty is a trimmed, non-empty text, if there is one.
func nonEmpty(s gen.OptNilString) (string, bool) {
	v, ok := s.Get()
	if !ok {
		return "", false
	}
	v = trimSpace(v)
	return v, v != ""
}

func validateEnv(env []gen.EnvVarDto) error {
	seen := map[string]bool{}
	for _, e := range env {
		if err := EnvVarName(e.Name); err != nil {
			return err
		}
		if seen[e.Name] {
			return kerrors.New(kerrors.Validation, "environment variable `%s` is set twice", e.Name)
		}
		seen[e.Name] = true
		secret, isSecret := e.Secret.Get()
		if !isSecret {
			continue
		}
		if e.Value.IsSet() && !e.Value.IsNull() {
			return kerrors.New(kerrors.Validation, "`%s`: set either value or secret, not both", e.Name)
		}
		if err := DNSLabel("secret name", secret.Name, 63); err != nil {
			return err
		}
		if err := SecretKey(secret.Key); err != nil {
			return err
		}
	}
	return nil
}

func validateDomains(domains []string) error {
	for _, d := range domains {
		if err := Hostname(d); err != nil {
			return err
		}
	}
	return nil
}

func validateReplicas(minimum, maximum uint32) error {
	if maximum > maxAppReplicas || minimum > maximum {
		return kerrors.New(kerrors.Validation, "replicas must satisfy 0 ≤ replicas ≤ max_replicas ≤ %d", maxAppReplicas)
	}
	return nil
}

func validateHealthPath(path string) error {
	graphic := !strings.ContainsFunc(path, func(r rune) bool { return r <= ' ' || r >= 0x7f })
	if path == "" || (strings.HasPrefix(path, "/") && len(path) <= 256 && graphic) {
		return nil
	}
	return kerrors.New(kerrors.Validation, "health_check_path must be an absolute path such as /healthz")
}

// quantityBytes is the bytes of a Kubernetes storage quantity (`5Gi`,
// `500Mi`, `10G`, `1024`); false when it is none (Rust: u128, checked).
func quantityBytes(q string) (*big.Int, bool) {
	q = trimSpace(q)
	split := strings.IndexFunc(q, func(r rune) bool { return r < '0' || r > '9' })
	if split < 0 {
		split = len(q)
	}
	number, unit := q[:split], q[split:]
	n, ok := new(big.Int).SetString(number, 10)
	if !ok || number == "" {
		return nil, false
	}
	multipliers := map[string]int64{
		"": 1, "Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40,
		"k": 1_000, "M": 1_000_000, "G": 1_000_000_000, "T": 1_000_000_000_000,
	}
	m, known := multipliers[unit]
	if !known {
		return nil, false
	}
	n.Mul(n, big.NewInt(m))
	maxU128 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))
	if n.Cmp(maxU128) > 0 {
		return nil, false
	}
	return n, true
}

func validateVolumes(volumes []gen.VolumeDto) error {
	if len(volumes) > maxAppVolumes {
		return kerrors.New(kerrors.Validation, "at most %d volumes per app", maxAppVolumes)
	}
	names, paths := map[string]bool{}, map[string]bool{}
	for _, v := range volumes {
		if err := DNSLabel("volume name", v.Name, 30); err != nil {
			return err
		}
		p := v.MountPath
		pathOK := strings.HasPrefix(p, "/") && p != "/" && len(p) <= 256 && !strings.Contains(p, "..") &&
			!strings.ContainsFunc(p, unicode.IsSpace)
		if !pathOK {
			return kerrors.New(kerrors.Validation, "volume `%s`: mount_path must be an absolute path other than /", v.Name)
		}
		if b, ok := quantityBytes(volumeSize(v)); !ok || b.Sign() == 0 {
			return kerrors.New(kerrors.Validation, "volume `%s`: size must be a quantity such as 1Gi or 500Mi", v.Name)
		}
		if names[v.Name] || paths[p] {
			return kerrors.New(kerrors.Validation, "volume names and mount paths must be unique")
		}
		names[v.Name], paths[p] = true, true
	}
	return nil
}

// volumeSize is a volume's size, `1Gi` when not given (serde default).
func volumeSize(v gen.VolumeDto) string { return v.Size.Or(v1alpha1.DefaultVolumeSize) }

// checkNoShrink: PersistentVolumeClaims can grow but never shrink.
func checkNoShrink(current []v1alpha1.Volume, next []gen.VolumeDto) error {
	for _, n := range next {
		i := slices.IndexFunc(current, func(c v1alpha1.Volume) bool { return c.Name == n.Name })
		if i < 0 {
			continue
		}
		if shrinks(volumeSize(n), current[i].Size) {
			return kerrors.New(kerrors.Validation, "volume `%s` cannot shrink from %s to %s", n.Name, current[i].Size, volumeSize(n))
		}
	}
	return nil
}

// shrinks is Rust's `quantity_bytes(next) < quantity_bytes(current)` over
// Options: an unreadable quantity is less than any readable one.
func shrinks(next, current string) bool {
	n, nextOK := quantityBytes(next)
	c, currentOK := quantityBytes(current)
	switch {
	case !currentOK:
		return false
	case !nextOK:
		return true
	}
	return n.Cmp(c) < 0
}

func validateSchedule(schedule, timeZone gen.OptNilString) error {
	if s, ok := schedule.Get(); ok && trimSpace(s) != "" && !render.ValidSchedule(s) {
		return kerrors.New(kerrors.Validation, "`%s` is not a cron expression (five fields, or @hourly/@daily/…)", s)
	}
	if tz, ok := timeZone.Get(); ok && tz != "" {
		return TimeZone(tz)
	}
	return nil
}

func validateFsGroup(group gen.OptNilInt64) error {
	if g, ok := group.Get(); ok && (g < 0 || g > math.MaxInt32) {
		return kerrors.New(kerrors.Validation, "fs_group must be a group id between 1 and 2147483647")
	}
	return nil
}

// port is a request's port as the u16 Rust decoded; a larger number is
// refused as serde refused it.
func port(p gen.OptNilInt32) (opt.Val[uint16], error) {
	v, ok := p.Get()
	if !ok {
		return opt.None[uint16](), nil
	}
	if v < 0 || v > math.MaxUint16 {
		return opt.None[uint16](), kerrors.New(kerrors.Validation, "port: invalid value: integer `%d`, expected u16", v)
	}
	return opt.Some(uint16(v)), nil
}

// validateSpec applies the controller's cross-field rules before anything
// is written; a Git app awaiting its build is fine.
func validateSpec(spec *v1alpha1.AppSpec) error {
	probe := v1alpha1.App{Spec: *spec}
	probe.Name = "validation"
	err := render.Validate(&probe)
	var build *render.BuildError
	if err == nil || (errors.As(err, &build) && build != nil && build.Reason == render.ReasonAwaitingBuild) {
		return nil
	}
	return kerrors.New(kerrors.Validation, "%s", err.Error())
}

func toCRDEnv(e gen.EnvVarDto) v1alpha1.EnvVar {
	out := v1alpha1.EnvVar{Name: e.Name}
	if s, ok := e.Secret.Get(); ok {
		out.FromSecret = &v1alpha1.KeyRef{Name: s.Name, Key: s.Key}
		return out
	}
	value := e.Value.Or("")
	out.Value = &value
	return out
}

func fromCRDEnv(e v1alpha1.EnvVar, withValues bool) gen.EnvVarDto {
	secret := e.FromSecret
	if secret == nil {
		secret = e.FromService
	}
	out := gen.EnvVarDto{Name: e.Name}
	if secret != nil {
		out.Secret = gen.NewOptNilSecretRef(gen.SecretRef{Name: secret.Name, Key: secret.Key})
	} else if withValues && e.Value != nil {
		out.Value = gen.NewOptNilString(*e.Value)
	}
	return out
}

func toDomains(hosts []string) []v1alpha1.Domain {
	out := make([]v1alpha1.Domain, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, v1alpha1.Domain{Host: ascii.Lower(trimSpace(h)), TLS: v1alpha1.DefaultTLS})
	}
	return out
}

func toCRDVolumes(volumes []gen.VolumeDto) []v1alpha1.Volume {
	out := make([]v1alpha1.Volume, 0, len(volumes))
	for _, v := range volumes {
		out = append(out, v1alpha1.Volume{Name: v.Name, MountPath: v.MountPath, Size: trimSpace(volumeSize(v))})
	}
	return out
}

// gitSource is source::git_source: the App spec's view of a Git source
// (the binding is the authority).
func gitSource(body gen.PutSource) *v1alpha1.GitSource {
	strategy := v1alpha1.BuildStrategyAuto
	switch body.Strategy.Or(gen.StrategyDtoAuto) {
	case gen.StrategyDtoDockerfile:
		strategy = v1alpha1.BuildStrategyDockerfile
	case gen.StrategyDtoRailpack:
		strategy = v1alpha1.BuildStrategyRailpack
	case gen.StrategyDtoAuto:
	}
	var dockerfile *string
	if d, ok := body.Dockerfile.Get(); ok && trimSpace(d) != "" {
		dockerfile = &d
	}
	return &v1alpha1.GitSource{
		Repo:   ascii.Lower(trimSpace(body.Repository)),
		Branch: trimSpace(body.Branch),
		Path:   trimSpace(body.Context.Or("")),
		Build:  v1alpha1.Build{Strategy: strategy, Dockerfile: dockerfile},
	}
}

// specFromCreate is the App spec a create request asks for.
func specFromCreate(body *gen.CreateApp) (v1alpha1.AppSpec, error) {
	var source v1alpha1.Source
	image, hasImage := nonEmpty(body.Image)
	git, hasGit := body.Git.Get()
	switch {
	case hasImage && !hasGit:
		if err := Image(image); err != nil {
			return v1alpha1.AppSpec{}, err
		}
		source = v1alpha1.SourceFromImage(image)
	case !hasImage && hasGit:
		source.Git = gitSource(git)
	default:
		return v1alpha1.AppSpec{}, kerrors.New(kerrors.Validation, "give exactly one of `image` and `git`")
	}
	size := body.Size.Or(v1alpha1.DefaultProcessSize)
	if err := DNSLabel("size", size, 30); err != nil {
		return v1alpha1.AppSpec{}, err
	}
	minimum := uint32(body.Replicas.Or(1)) //nolint:gosec // ogen checks minimum 0
	maximum := minimum
	if m, ok := body.MaxReplicas.Get(); ok {
		maximum = max(uint32(m), minimum) //nolint:gosec // ogen checks minimum 0
	}
	listens, err := port(body.Port)
	if err != nil {
		return v1alpha1.AppSpec{}, err
	}
	for _, check := range []func() error{
		func() error { return validateReplicas(minimum, maximum) },
		func() error { return validateEnv(body.Env) },
		func() error { return validateDomains(body.Domains) },
		func() error { return validateVolumes(body.Volumes) },
		func() error { return validateSchedule(body.Schedule, body.TimeZone) },
		func() error { return validateFsGroup(body.FsGroup) },
		func() error { return validateHealthPath(body.HealthCheckPath.Or("")) },
	} {
		if err := check(); err != nil {
			return v1alpha1.AppSpec{}, err
		}
	}
	process := v1alpha1.Process{
		Command: body.Command, Port: listens.Ptr(), Size: size,
		Replicas: v1alpha1.Replicas{Min: minimum, Max: maximum},
		Protocol: protocolOf(body.Protocol.Or(gen.ProtocolDtoHTTP)),
	}
	name := webProcess
	if schedule, ok := nonEmpty(body.Schedule); ok {
		process.Schedule, name = &schedule, jobProcess
	}
	if tz, ok := nonEmpty(body.TimeZone); ok {
		process.TimeZone = &tz
	}
	spec := v1alpha1.AppSpec{
		Source:  source,
		Runtime: v1alpha1.Runtime{Processes: map[string]v1alpha1.Process{name: process}},
		Env:     mapSlice(body.Env, toCRDEnv),
		Domains: toDomains(body.Domains),
		Volumes: toCRDVolumes(body.Volumes),
	}
	if path, ok := body.HealthCheckPath.Get(); ok && path != "" {
		spec.Runtime.HealthCheck = &v1alpha1.HealthCheck{Path: path}
	}
	if g, ok := body.FsGroup.Get(); ok && g > 0 {
		spec.Runtime.FSGroup = &g
	}
	return spec, nil
}

func protocolOf(p gen.ProtocolDto) v1alpha1.Protocol {
	switch p {
	case gen.ProtocolDtoTCP:
		return v1alpha1.ProtocolTCP
	case gen.ProtocolDtoHTTP:
	}
	return v1alpha1.ProtocolHTTP
}

func mapSlice[T, U any](in []T, f func(T) U) []U {
	out := make([]U, 0, len(in))
	for _, v := range in {
		out = append(out, f(v))
	}
	return out
}

// updatedProcess is the process an update changes: `web`, else the first.
func updatedProcess(spec *v1alpha1.AppSpec) (string, error) {
	if _, ok := spec.Runtime.Processes[webProcess]; ok {
		return webProcess, nil
	}
	names := make([]string, 0, len(spec.Runtime.Processes))
	for n := range spec.Runtime.Processes {
		names = append(names, n)
	}
	if len(names) == 0 {
		return "", kerrors.New(kerrors.Validation, "app has no processes")
	}
	slices.Sort(names)
	return names[0], nil
}

// applyUpdate applies a partial update to an App spec, validated field by
// field; the caller then runs validateSpec on the result.
func applyUpdate(spec *v1alpha1.AppSpec, u *gen.UpdateApp) error {
	if image, ok := u.Image.Get(); ok {
		if err := Image(image); err != nil {
			return err
		}
		spec.Source = v1alpha1.SourceFromImage(trimSpace(image))
	}
	key, err := updatedProcess(spec)
	if err != nil {
		return err
	}
	if err := validateSchedule(u.Schedule, u.TimeZone); err != nil {
		return err
	}
	if err := updateProcess(spec, key, u); err != nil {
		return err
	}
	return updateSpec(spec, u)
}

func updateProcess(spec *v1alpha1.AppSpec, key string, u *gen.UpdateApp) error {
	p, ok := spec.Runtime.Processes[key]
	if !ok {
		return nil
	}
	if r, ok := u.Replicas.Get(); ok {
		p.Replicas.Min = uint32(r) //nolint:gosec // ogen checks minimum 0
		p.Replicas.Max = max(p.Replicas.Max, p.Replicas.Min)
	}
	if m, ok := u.MaxReplicas.Get(); ok {
		p.Replicas.Max = max(uint32(m), p.Replicas.Min) //nolint:gosec // ogen checks minimum 0
	}
	if err := validateReplicas(p.Replicas.Min, p.Replicas.Max); err != nil {
		return err
	}
	if size, ok := u.Size.Get(); ok {
		if err := DNSLabel("size", size, 30); err != nil {
			return err
		}
		p.Size = size
	}
	if command, ok := u.Command.Get(); ok {
		p.Command = command
	}
	listens, err := port(u.Port)
	if err != nil {
		return err
	}
	if listens.IsSome() {
		p.Port = listens.Ptr()
	}
	if _, given := u.Schedule.Get(); given {
		p.Schedule = nil
		if s, ok := nonEmpty(u.Schedule); ok {
			p.Schedule = &s
		}
	}
	if _, given := u.TimeZone.Get(); given {
		p.TimeZone = nil
		if tz, ok := nonEmpty(u.TimeZone); ok {
			p.TimeZone = &tz
		}
	}
	if protocol, ok := u.Protocol.Get(); ok {
		p.Protocol = protocolOf(protocol)
	}
	spec.Runtime.Processes[key] = p
	return nil
}

func updateSpec(spec *v1alpha1.AppSpec, u *gen.UpdateApp) error {
	if env, ok := u.Env.Get(); ok {
		if err := validateEnv(env); err != nil {
			return err
		}
		spec.Env = mapSlice(env, toCRDEnv)
	}
	if hosts, ok := u.Domains.Get(); ok {
		if err := validateDomains(hosts); err != nil {
			return err
		}
		spec.Domains = toDomains(hosts)
	}
	if path, ok := u.HealthCheckPath.Get(); ok {
		if err := validateHealthPath(path); err != nil {
			return err
		}
		spec.Runtime.HealthCheck = nil
		if path != "" {
			spec.Runtime.HealthCheck = &v1alpha1.HealthCheck{Path: path}
		}
	}
	if volumes, ok := u.Volumes.Get(); ok {
		if err := validateVolumes(volumes); err != nil {
			return err
		}
		if err := checkNoShrink(spec.Volumes, volumes); err != nil {
			return err
		}
		spec.Volumes = toCRDVolumes(volumes)
	}
	if g, ok := u.FsGroup.Get(); ok {
		if err := validateFsGroup(u.FsGroup); err != nil {
			return err
		}
		spec.Runtime.FSGroup = nil
		if g > 0 {
			spec.Runtime.FSGroup = &g
		}
	}
	return nil
}

// ensureDomainsFree: a hostname belongs to exactly one app (first come,
// first served), so one tenant cannot route another tenant's domain. The
// organization's apps are checked in SQL, which answers at once; every
// other App in the cluster through the projection. The owner is not
// revealed.
func (s *Server) ensureDomainsFree(ctx context.Context, org ids.OrgID, namespace, app string, spec *v1alpha1.AppSpec) error {
	if len(spec.Domains) == 0 {
		return nil
	}
	if err := s.ensureDomainsClaimed(ctx, org, spec); err != nil {
		return err
	}
	taken := func(host string) error {
		return kerrors.New(kerrors.Conflict, "domain `%s` is already used by another app", host)
	}
	owned := func(host string) (string, bool) {
		i := slices.IndexFunc(spec.Domains, func(d v1alpha1.Domain) bool { return strings.EqualFold(d.Host, host) })
		if i < 0 {
			return "", false
		}
		return spec.Domains[i].Host, true
	}
	t, err := s.deps.Store.Tenant(ctx, org)
	if err != nil {
		return err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	domains, err := t.Domains(ctx)
	if err != nil {
		return err //nolint:wrapcheck // a store error, answered as internal
	}
	for _, d := range domains {
		if d.Namespace == namespace && d.App == app {
			continue
		}
		if host, ok := owned(d.Host); ok {
			return taken(host)
		}
	}
	for _, other := range s.deps.Projections.Apps() {
		if other.Namespace == namespace && other.Name == app {
			continue
		}
		for _, o := range other.Domains {
			if host, ok := owned(o); ok {
				return taken(host)
			}
		}
	}
	return nil
}

// ensureDomainsClaimed: no app serves a domain another organization
// verified (M5.2); with `domains.require_claim`, only domains its own
// organization verified.
func (s *Server) ensureDomainsClaimed(ctx context.Context, org ids.OrgID, spec *v1alpha1.AppSpec) error {
	for _, d := range spec.Domains {
		host, err := dnsname.Canonical(d.Host)
		if err != nil {
			return kerrors.New(kerrors.Validation, "%s", err.Error())
		}
		_, owner, found, err := s.deps.Store.DomainOwner(ctx, host)
		switch {
		case err != nil:
			return err //nolint:wrapcheck // a store error, answered as internal
		case found && owner != org:
			return kerrors.New(kerrors.Conflict, "domain `%s` is claimed by another organization", d.Host)
		case !found && s.deps.Config.Domains.RequireClaim:
			return kerrors.New(kerrors.Validation, "domain `%s` is not verified for this organization: claim it first", d.Host)
		}
	}
	return nil
}
