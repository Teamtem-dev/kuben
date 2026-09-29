package httpapi

// Registry logins of an organization (2.1): one login per registry host
// that every environment pulls with unless it has its own login for that
// host (registries.go), which wins. The password is sealed with the keyring
// for the login's id and never returned; a new password rotates it. Reading
// needs secret-read on the organization, writing and checking secret-write,
// as an environment's logins do on the environment.
//
// A check logs in to the registry live (oci.LoginChecker: the /v2/ ping, the
// token exchange a Bearer challenge asks for, over HTTPS only, within 10 s);
// its outcome is an answer, not an error: `ok` false with the reason.

import (
	"context"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/integrations/oci"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/keyring"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

// maxRegistryUsername is the longest username an organization login takes
// (the column's bound).
const maxRegistryUsername = 255

// registryPreset is a registry Kuben knows how to log in to.
type registryPreset struct {
	id    store.RegistryPreset
	label string
	// server is the registry's name in image references; "" when the user
	// names it.
	server      string
	accountHint string
	accessHint  string
	docsURL     string
}

// registryPresets are the presets in the order the console shows them.
var registryPresets = []registryPreset{
	{
		id: store.PresetDockerHub, label: "Docker Hub", server: keyring.DockerHub,
		accountHint: "Docker ID",
		accessHint:  "A personal access token with read access (Account settings → Personal access tokens)",
		docsURL:     "https://docs.docker.com/security/for-developers/access-tokens/",
	},
	{
		id: store.PresetGHCR, label: "GitHub Container Registry", server: "ghcr.io",
		accountHint: "GitHub username",
		accessHint:  "A personal access token (classic) with the read:packages scope",
		docsURL:     "https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry",
	},
	{
		id: store.PresetGitLab, label: "GitLab Container Registry", server: "registry.gitlab.com",
		accountHint: "GitLab username, or the deploy token's username",
		accessHint:  "A deploy token or personal access token with the read_registry scope",
		docsURL:     "https://docs.gitlab.com/user/packages/container_registry/authenticate_with_container_registry/",
	},
	{
		id: store.PresetQuay, label: "Quay.io", server: "quay.io",
		accountHint: "Robot account (organization+robot)",
		accessHint:  "The robot account's token",
		docsURL:     "https://docs.quay.io/glossary/robot-accounts.html",
	},
	{
		id: store.PresetHarbor, label: "Harbor",
		accountHint: "Robot account (robot$project+name)",
		accessHint:  "The robot account's secret",
		docsURL:     "https://goharbor.io/docs/main/working-with-projects/project-configuration/create-robot-accounts/",
	},
	{
		id: store.PresetCustom, label: "Other registry",
		accountHint: "Username",
		accessHint:  "Password or access token",
	},
}

// presetOf is the preset id names; false for none.
func presetOf(id store.RegistryPreset) (registryPreset, bool) {
	for _, p := range registryPresets {
		if p.id == id {
			return p, true
		}
	}
	return registryPreset{}, false
}

// orgRegistryDto is OrgRegistryDto of r: never its password.
func orgRegistryDto(r store.OrgRegistry) gen.OrgRegistryDto {
	return gen.OrgRegistryDto{
		ID:            r.ID.String(),
		Name:          r.Name,
		Preset:        gen.RegistryPresetIdDto(r.Preset),
		Server:        r.Server,
		Username:      r.Username,
		HasPassword:   len(r.Password.Ciphertext) > 0,
		CreatedAt:     r.CreatedAt,
		UpdatedAt:     r.UpdatedAt,
		LastCheckedAt: optNilInt64(r.LastCheckedAt),
		LastError:     optNilString(r.LastError),
	}
}

// registryServer is the server of a login of preset: the preset's own
// (given must be empty or name it), or given, which Harbor and custom
// logins need.
func registryServer(preset registryPreset, given string) (string, error) {
	given = strings.TrimSpace(given)
	if preset.server != "" {
		if given == "" {
			return preset.server, nil
		}
		server, err := registryName(given)
		if err != nil || server != preset.server {
			return "", kerrors.New(kerrors.Validation, "the %s preset logs in to `%s` only; use the custom preset for `%s`",
				preset.label, preset.server, given)
		}
		return server, nil
	}
	if given == "" {
		return "", kerrors.New(kerrors.Validation, "the %s preset needs the registry's server, such as registry.example.com", preset.label)
	}
	return registryName(given)
}

// checkRegistryUsername checks a login's username.
func checkRegistryUsername(username string) error {
	if username == "" || utf8.RuneCountInString(username) > maxRegistryUsername ||
		strings.ContainsFunc(username, unicode.IsControl) {
		return kerrors.New(kerrors.Validation, "the username must be 1 to %d printable characters", maxRegistryUsername)
	}
	if strings.Contains(username, ":") {
		return kerrors.New(kerrors.Validation, "the username cannot contain `:`")
	}
	return nil
}

// checkRegistryPassword checks a login's password.
func checkRegistryPassword(password string) error {
	if password == "" || len(password) > maxLoginField || strings.ContainsFunc(password, unicode.IsControl) {
		return kerrors.New(kerrors.Validation, "the password must be 1 to %d printable characters", maxLoginField)
	}
	return nil
}

// newRegistryLogin is a checked login to add.
type newRegistryLogin struct {
	name   string
	preset store.RegistryPreset
	server string
	login  oci.Login
}

// checkNewRegistry checks a CreateOrgRegistry body.
func checkNewRegistry(req *gen.CreateOrgRegistry) (newRegistryLogin, error) {
	if err := DNSLabel("registry name", req.Name, 63); err != nil {
		return newRegistryLogin{}, err
	}
	preset, ok := presetOf(store.RegistryPreset(req.Preset))
	if !ok {
		return newRegistryLogin{}, kerrors.New(kerrors.Validation, "unknown registry preset `%s`", req.Preset)
	}
	server, err := registryServer(preset, req.Server.Or(""))
	if err != nil {
		return newRegistryLogin{}, err
	}
	if err := checkRegistryUsername(req.Username); err != nil {
		return newRegistryLogin{}, err
	}
	if err := checkRegistryPassword(req.Password); err != nil {
		return newRegistryLogin{}, err
	}
	return newRegistryLogin{
		name: req.Name, preset: preset.id, server: server,
		login: oci.Login{Username: req.Username, Password: req.Password},
	}, nil
}

// registryReference is what audit records name a login by.
func registryReference(r store.OrgRegistry) string { return r.Name + " (" + r.Server + ")" }

// registryAudit is the domain record of a change of login r.
func registryAudit(c accessOrg, action string, r store.OrgRegistry, data map[string]any) store.NewAudit {
	audit := requestAudit(c.access, action, "registry", registryReference(r))
	data["name"], data["server"], data["preset"] = r.Name, r.Server, string(r.Preset)
	audit.Data = opt.Some[any](data)
	return audit
}

// findOrgRegistry is the login given names, by id or by name.
func findOrgRegistry(ctx context.Context, t *store.Tenant, given string) (store.OrgRegistry, error) {
	var (
		r     store.OrgRegistry
		found bool
		err   error
	)
	if u, parseErr := uuid.Parse(given); parseErr == nil {
		r, found, err = t.OrgRegistry(ctx, ids.From[ids.OrgRegistry](u))
	} else {
		r, found, err = t.OrgRegistryByName(ctx, given)
	}
	if err != nil {
		return store.OrgRegistry{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found {
		return store.OrgRegistry{}, kerrors.New(kerrors.NotFound, "registry `%s`", given)
	}
	return r, nil
}

// registryTaken is the conflict of a login whose name or server another
// login of the organization has.
func registryTaken(name, server string) error {
	return kerrors.New(kerrors.Conflict, "the organization has a registry login named `%s` or for `%s` already", name, server)
}

// loginChecker is what checks logins (Deps.Registries).
func (s *Server) loginChecker() oci.LoginChecker {
	return s.deps.Registries
}

// checkRegistryLogin logs in to server as login; the reason it failed,
// none when it passed.
func (s *Server) checkRegistryLogin(ctx context.Context, server string, login oci.Login) opt.Val[string] {
	err := s.loginChecker().CheckLogin(ctx, server, login)
	if err == nil {
		return opt.None[string]()
	}
	var (
		unauthorized oci.Unauthorized
		unreachable  oci.Unreachable
		limited      oci.RateLimited
		notFound     oci.NotFound
	)
	switch {
	case errors.As(err, &unauthorized):
		return opt.Some("the registry refused the username or password")
	case errors.As(err, &unreachable):
		return opt.Some("cannot reach the registry: " + unreachable.Reason)
	case errors.As(err, &limited):
		return opt.Some(limited.Error())
	case errors.As(err, &notFound):
		return opt.Some("the registry has no registry API at /v2/")
	}
	return opt.Some(err.Error())
}

// registryCheckDto is the answer of a check made now.
func (s *Server) registryCheckDto(failure opt.Val[string]) *gen.RegistryCheckDto {
	return &gen.RegistryCheckDto{Ok: failure.IsNone(), Error: optNilString(failure), CheckedAt: s.deps.Clock.NowMs()}
}

// ListRegistryPresets is the registries Kuben knows how to log in to.
func (s *Server) ListRegistryPresets(ctx context.Context) (gen.ListRegistryPresetsRes, error) {
	if _, err := s.callerOrg(ctx, perm.OrgRead, false); err != nil {
		return nil, err
	}
	out := make(gen.ListRegistryPresetsOKApplicationJSON, 0, len(registryPresets))
	for _, p := range registryPresets {
		dto := gen.RegistryPresetDto{
			ID:           gen.RegistryPresetIdDto(p.id),
			Label:        p.label,
			UsernameHint: p.accountHint,
			PasswordHint: p.accessHint,
		}
		dto.Server.SetToNull()
		if p.server != "" {
			dto.Server = gen.NewOptNilString(p.server)
		}
		dto.DocsUrl.SetToNull()
		if p.docsURL != "" {
			dto.DocsUrl = gen.NewOptNilString(p.docsURL)
		}
		out = append(out, dto)
	}
	return &out, nil
}

// ListOrgRegistries is the organization's registry logins by name.
func (s *Server) ListOrgRegistries(ctx context.Context) (gen.ListOrgRegistriesRes, error) {
	c, err := s.callerOrg(ctx, perm.SecretRead, false)
	if err != nil {
		return nil, err
	}
	t, err := s.deps.Store.Tenant(ctx, c.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	found, err := t.OrgRegistries(ctx)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	out := make(gen.ListOrgRegistriesOKApplicationJSON, 0, len(found))
	for _, r := range found {
		out = append(out, orgRegistryDto(r))
	}
	return &out, nil
}

// GetOrgRegistry is one registry login of the organization.
func (s *Server) GetOrgRegistry(ctx context.Context, params gen.GetOrgRegistryParams) (gen.GetOrgRegistryRes, error) {
	c, err := s.callerOrg(ctx, perm.SecretRead, false)
	if err != nil {
		return nil, err
	}
	t, err := s.deps.Store.Tenant(ctx, c.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	r, err := findOrgRegistry(ctx, t, params.Registry)
	if err != nil {
		return nil, err
	}
	dto := orgRegistryDto(r)
	return &dto, nil
}

// CreateOrgRegistry adds a registry login for every environment of the
// organization. It is not checked: the console checks it first
// (testNewOrgRegistry).
func (s *Server) CreateOrgRegistry(ctx context.Context, req *gen.CreateOrgRegistry) (gen.CreateOrgRegistryRes, error) {
	c, err := s.callerOrg(ctx, perm.SecretWrite, false)
	if err != nil {
		return nil, err
	}
	checked, err := checkNewRegistry(req)
	if err != nil {
		return nil, err
	}
	ring, err := s.keyring()
	if err != nil {
		return nil, err
	}
	id := ids.New[ids.OrgRegistry]()
	sealed, err := ring.Seal(keyring.OrgRegistryIdentity(c.org, id), []byte(checked.login.Password))
	if err != nil {
		return nil, kerrors.New(kerrors.Internal, "sealing the password failed: %s", err.Error())
	}
	t, err := s.deps.Store.Tenant(ctx, c.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	saved, err := t.CreateOrgRegistry(ctx, store.NewOrgRegistry{
		ID: id, Name: checked.name, Preset: checked.preset, Server: checked.server,
		Username: checked.login.Username, Password: sealed, CreatedBy: requestedBy(c.access),
	})
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	stored, err := registrySaved(saved, checked.name, checked.server)
	if err != nil {
		return nil, err
	}
	if err := t.AppendAudit(ctx, registryAudit(c, "registry.created", stored, map[string]any{})); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if err := t.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	dto := orgRegistryDto(stored)
	return &dto, nil
}

// registrySaved is the login a create or update saved, or why it did not.
func registrySaved(saved store.OrgRegistrySaved, name, server string) (store.OrgRegistry, error) {
	switch s := saved.(type) {
	case store.OrgRegistryStored:
		return s.Registry, nil
	case store.OrgRegistryTaken:
		return store.OrgRegistry{}, registryTaken(name, server)
	case store.OrgRegistryNotFound, nil:
	}
	return store.OrgRegistry{}, kerrors.New(kerrors.NotFound, "registry `%s`", name)
}

// UpdateOrgRegistry changes a registry login; a new password rotates it.
// Runs pull with the login as it is when they are delivered.
func (s *Server) UpdateOrgRegistry(
	ctx context.Context, req *gen.UpdateOrgRegistry, params gen.UpdateOrgRegistryParams,
) (gen.UpdateOrgRegistryRes, error) {
	c, err := s.callerOrg(ctx, perm.SecretWrite, false)
	if err != nil {
		return nil, err
	}
	t, err := s.deps.Store.Tenant(ctx, c.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	current, err := findOrgRegistry(ctx, t, params.Registry)
	if err != nil {
		return nil, err
	}
	change, err := s.registryChange(c.org, current, req)
	if err != nil {
		return nil, err
	}
	saved, err := t.UpdateOrgRegistry(ctx, current.ID, change)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	stored, err := registrySaved(saved, change.Name.Or(current.Name), change.Server.Or(current.Server))
	if err != nil {
		return nil, err
	}
	data := map[string]any{"rotated": change.Password.IsSome()}
	if stored.Name != current.Name {
		data["previousName"] = current.Name
	}
	if stored.Server != current.Server {
		data["previousServer"] = current.Server
	}
	if err := t.AppendAudit(ctx, registryAudit(c, "registry.updated", stored, data)); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if err := t.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	dto := orgRegistryDto(stored)
	return &dto, nil
}

// registryChange is the checked change req asks of current; a new password
// is sealed for current's id.
func (s *Server) registryChange(org ids.OrgID, current store.OrgRegistry, req *gen.UpdateOrgRegistry) (store.OrgRegistryChange, error) {
	var change store.OrgRegistryChange
	if name, ok := req.Name.Get(); ok && name != current.Name {
		if err := DNSLabel("registry name", name, 63); err != nil {
			return change, err
		}
		change.Name = opt.Some(name)
	}
	if given, ok := req.Server.Get(); ok {
		preset, known := presetOf(current.Preset)
		if !known {
			preset = registryPreset{id: current.Preset, label: string(current.Preset)}
		}
		server, err := registryServer(preset, given)
		if err != nil {
			return change, err
		}
		if server != current.Server {
			change.Server = opt.Some(server)
		}
	}
	if username, ok := req.Username.Get(); ok && username != current.Username {
		if err := checkRegistryUsername(username); err != nil {
			return change, err
		}
		change.Username = opt.Some(username)
	}
	if password, ok := req.Password.Get(); ok {
		if err := checkRegistryPassword(password); err != nil {
			return change, err
		}
		ring, err := s.keyring()
		if err != nil {
			return change, err
		}
		sealed, err := ring.Seal(keyring.OrgRegistryIdentity(org, current.ID), []byte(password))
		if err != nil {
			return change, kerrors.New(kerrors.Internal, "sealing the password failed: %s", err.Error())
		}
		change.Password = opt.Some(sealed)
	}
	return change, nil
}

// DeleteOrgRegistry deletes a registry login. Pods running keep pulling
// with what they were delivered with until their next deployment.
func (s *Server) DeleteOrgRegistry(ctx context.Context, params gen.DeleteOrgRegistryParams) (gen.DeleteOrgRegistryRes, error) {
	c, err := s.callerOrg(ctx, perm.SecretWrite, false)
	if err != nil {
		return nil, err
	}
	t, err := s.deps.Store.Tenant(ctx, c.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	current, err := findOrgRegistry(ctx, t, params.Registry)
	if err != nil {
		return nil, err
	}
	deleted, err := t.DeleteOrgRegistry(ctx, current.ID)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !deleted {
		return nil, kerrors.New(kerrors.NotFound, "registry `%s`", params.Registry)
	}
	if err := t.AppendAudit(ctx, registryAudit(c, "registry.deleted", current, map[string]any{})); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if err := t.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	return &gen.DeleteOrgRegistryNoContent{}, nil
}

// TestNewOrgRegistry logs in to a registry without saving the login.
func (s *Server) TestNewOrgRegistry(ctx context.Context, req *gen.CreateOrgRegistry) (gen.TestNewOrgRegistryRes, error) {
	if _, err := s.callerOrg(ctx, perm.SecretWrite, false); err != nil {
		return nil, err
	}
	checked, err := checkNewRegistry(req)
	if err != nil {
		return nil, err
	}
	return s.registryCheckDto(s.checkRegistryLogin(ctx, checked.server, checked.login)), nil
}

// TestOrgRegistry logs in with a saved login again and records the
// outcome on it.
func (s *Server) TestOrgRegistry(ctx context.Context, params gen.TestOrgRegistryParams) (gen.TestOrgRegistryRes, error) {
	c, err := s.callerOrg(ctx, perm.SecretWrite, false)
	if err != nil {
		return nil, err
	}
	ring, err := s.keyring()
	if err != nil {
		return nil, err
	}
	// The registry is asked outside any transaction.
	current, err := s.readOrgRegistry(ctx, c.org, params.Registry)
	if err != nil {
		return nil, err
	}
	login, err := ring.OpenOrgRegistry(current)
	if err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	failure := s.checkRegistryLogin(ctx, current.Server, oci.Login{Username: login.Username, Password: login.Password})
	t, err := s.deps.Store.Tenant(ctx, c.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	recorded, err := t.RecordOrgRegistryCheck(ctx, current.ID, failure)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !recorded {
		return nil, kerrors.New(kerrors.NotFound, "registry `%s`", params.Registry)
	}
	if err := t.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	return s.registryCheckDto(failure), nil
}

// readOrgRegistry is the login given names, read in a transaction of its
// own.
func (s *Server) readOrgRegistry(ctx context.Context, org ids.OrgID, given string) (store.OrgRegistry, error) {
	t, err := s.deps.Store.Tenant(ctx, org)
	if err != nil {
		return store.OrgRegistry{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	return findOrgRegistry(ctx, t, given)
}
