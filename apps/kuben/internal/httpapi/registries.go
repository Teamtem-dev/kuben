package httpapi

// Private registry logins of an environment (routes/registries.rs, M4.4,
// ADR-030).
//
// A login is a managed secret of kind `registry`: encrypted revisions of a
// username and password for one registry. Kuben resolves image tags from
// that registry with it, and every run whose release comes from it pulls
// with the revision it was accepted with (an immutable
// `kubernetes.io/dockerconfigjson` Secret). The password is never returned.
// Revisions and revocation are those of `/secrets/{name}`.

import (
	"context"
	"strings"
	"unicode"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ascii"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/integrations/oci"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/keyring"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

// maxLoginField is the longest username or password accepted.
const maxLoginField = 4096

// registryLoginDto is RegistryLoginDto::of; false for a secret that is not
// a login.
func registryLoginDto(s store.SecretSummary) (gen.RegistryLoginDto, bool) {
	kind, ok := s.Kind.(store.SecretRegistry)
	if !ok {
		return gen.RegistryLoginDto{}, false
	}
	return gen.RegistryLoginDto{
		Name:      s.Name,
		Registry:  kind.Host,
		Revision:  revisionNumber(s.CurrentRevision),
		Revoked:   s.Revoked,
		UpdatedAt: Timestamp(s.UpdatedAt),
	}, true
}

// registryName is the registry name of given, normalized as image
// references carry it.
func registryName(given string) (string, error) {
	given = ascii.Lower(strings.TrimSpace(given))
	invalid := kerrors.New(kerrors.Validation, "`%s` is not a registry name such as ghcr.io", given)
	if given == "" || strings.ContainsAny(given, "/@") {
		return "", invalid
	}
	parsed, err := oci.Parse(given + "/kuben/probe")
	// A first part without a dot, port or `localhost` is a Docker Hub path.
	if err != nil || parsed.Registry != given {
		return "", invalid
	}
	return parsed.Registry, nil
}

// checkLogin checks the username and password of a login.
func checkLogin(body *gen.PutRegistryLogin) error {
	for _, field := range []struct{ name, value string }{
		{"username", body.Username}, {"password", body.Password},
	} {
		if field.value == "" || len(field.value) > maxLoginField || strings.ContainsFunc(field.value, unicode.IsControl) {
			return kerrors.New(kerrors.Validation, "the %s must be 1 to %d printable characters", field.name, maxLoginField)
		}
	}
	if strings.Contains(body.Username, ":") {
		return kerrors.New(kerrors.Validation, "the username cannot contain `:`")
	}
	return nil
}

// ListRegistryLogins lists the registry logins of an environment (never
// their passwords).
func (s *Server) ListRegistryLogins(ctx context.Context, params gen.ListRegistryLoginsParams) ([]gen.RegistryLoginDto, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	e, err := s.findEnvironment(ctx, a, params.Project, params.Environment)
	if err != nil {
		return nil, err
	}
	if _, err := a.Require(perm.SecretRead, e.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	stored, err := s.storedSecrets(ctx, e)
	if err != nil {
		return nil, err
	}
	out := make([]gen.RegistryLoginDto, 0, len(stored))
	for _, sec := range stored {
		if dto, ok := registryLoginDto(sec); ok {
			out = append(out, dto)
		}
	}
	return out, nil
}

// PutRegistryLogin sets the login of a registry: a new revision, used by
// the next runs.
func (s *Server) PutRegistryLogin(
	ctx context.Context, req *gen.PutRegistryLogin, params gen.PutRegistryLoginParams,
) (gen.PutRegistryLoginRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	e, err := s.findEnvironment(ctx, a, params.Project, params.Environment)
	if err != nil {
		return nil, err
	}
	if _, err := a.Require(perm.SecretWrite, e.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	if err := DNSLabel("login name", params.Name, 63); err != nil {
		return nil, err
	}
	registry, err := registryName(req.Registry)
	if err != nil {
		return nil, err
	}
	if err := checkLogin(req); err != nil {
		return nil, err
	}
	values := keyring.RegistryLogin{Username: req.Username, Password: req.Password}.Values()
	t, err := s.deps.Store.Tenant(ctx, e.project.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	n := newRevision{name: params.Name, kind: store.SecretRegistry{Host: registry}, values: values}
	if err := s.storeRevision(ctx, t, a, e, n); err != nil {
		return nil, err
	}
	summary, found, err := t.Secret(ctx, e.env.ID, params.Name)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	saved, ok := registryLoginDto(summary)
	if !found || !ok {
		return nil, kerrors.New(kerrors.Internal, "the new registry login is missing")
	}
	if err := t.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	return &saved, nil
}

// DeleteRegistryLogin deletes a registry login. Runs accepted with it keep
// their revision.
func (s *Server) DeleteRegistryLogin(ctx context.Context, params gen.DeleteRegistryLoginParams) (gen.DeleteRegistryLoginRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	e, err := s.findEnvironment(ctx, a, params.Project, params.Environment)
	if err != nil {
		return nil, err
	}
	if _, err := a.Require(perm.SecretWrite, e.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	name := params.Name
	t, err := s.deps.Store.Tenant(ctx, e.project.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	notFound := kerrors.New(kerrors.NotFound, "registry login `%s`", name)
	current, found, err := t.Secret(ctx, e.env.ID, name)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if _, isLogin := current.Kind.(store.SecretRegistry); !found || !isLogin {
		return nil, notFound
	}
	audit := requestAudit(a, "secret.deleted", "secret", secretReference(e, name))
	deleted, err := t.DeleteSecret(ctx, e.env.ID, name, audit)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	switch d := deleted.(type) {
	case store.SecretDeletedDone:
		if err := t.Commit(ctx); err != nil {
			return nil, err //nolint:wrapcheck // a store error, answered as internal
		}
		return &gen.DeleteRegistryLoginNoContent{}, nil
	case store.SecretDeletedInUse:
		return nil, kerrors.New(kerrors.Conflict, "registry login `%s` is read as a secret by %s", name, strings.Join(d.Apps, ", "))
	case store.SecretDeletedNotFound, nil:
	}
	return nil, notFound
}
