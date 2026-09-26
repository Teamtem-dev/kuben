package httpapi

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/internal/core/model"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/internal/httpapi/access"
	"github.com/Teamtem-dev/kuben/internal/httpapi/auth"
	"github.com/Teamtem-dev/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/internal/kube/render"
	"github.com/Teamtem-dev/kuben/internal/store"
)

// Personal API tokens (routes/tokens.rs, scenario 3). The plaintext token
// is returned exactly once; the store keeps only sha256(secret). Tokens
// cannot manage tokens, members or passwords (no privilege persistence
// through a leaked token).

const (
	tokenDefaultTTLDays = 90
	tokenMaxTTLDays     = 365
	dayMs               = 86_400_000
)

// scopeNames names the projects and environments of the caller's
// organizations by SQL id: what token scopes name.
type scopeNames struct {
	projects     map[uuid.UUID]string
	environments map[uuid.UUID]string
}

func (s *Server) scopeNames(ctx context.Context, a access.Access) (scopeNames, error) {
	names := scopeNames{projects: map[uuid.UUID]string{}, environments: map[uuid.UUID]string{}}
	for _, org := range a.OrgIDs() {
		if err := s.addScopeNames(ctx, org, names); err != nil {
			return scopeNames{}, err
		}
	}
	return names, nil
}

func (s *Server) addScopeNames(ctx context.Context, org ids.OrgID, names scopeNames) error {
	t, err := s.deps.Store.Tenant(ctx, org)
	if err != nil {
		return err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	projects, err := t.Projects(ctx)
	if err != nil {
		return err //nolint:wrapcheck // a store error, answered as internal
	}
	for _, p := range projects {
		envs, err := t.Environments(ctx, p.ID)
		if err != nil {
			return err //nolint:wrapcheck // a store error, answered as internal
		}
		for _, e := range envs {
			names.environments[e.ID.UUID()] = render.EnvironmentResourceName(p.Slug, e.Slug)
		}
		names.projects[p.ID.UUID()] = p.Slug
	}
	return nil
}

// nameOf is the name of a scope node, null when it has none (anymore).
func nameOf(names map[uuid.UUID]string, node opt.Val[uuid.UUID]) gen.OptNilString {
	name := opt.None[string]()
	if id, ok := node.Get(); ok {
		if n, found := names[id]; found {
			name = opt.Some(n)
		}
	}
	return optNilString(name)
}

func optNilInt64(v opt.Val[int64]) gen.OptNilInt64 {
	if n, ok := v.Get(); ok {
		return gen.NewOptNilInt64(n)
	}
	var out gen.OptNilInt64
	out.SetToNull()
	return out
}

func tokenDto(names scopeNames, t model.APIToken) gen.TokenDto {
	return gen.TokenDto{
		ID:          t.ID.String(),
		Name:        t.Name,
		Prefix:      t.Prefix,
		Role:        t.Scope.Role.String(),
		Project:     nameOf(names.projects, t.Scope.Project),
		Environment: nameOf(names.environments, t.Scope.Environment),
		ExpiresAt:   optNilInt64(t.ExpiresAt),
		LastUsedAt:  optNilInt64(t.LastUsedAt),
		Revoked:     t.RevokedAt.IsSome(),
		CreatedAt:   t.CreatedAt,
	}
}

// ValidTokenName reports whether a token name has 1–64 characters after
// trimming, none of them a control character.
func ValidTokenName(name string) bool {
	n := utf8.RuneCountInString(name)
	return n > 0 && n <= 64 && !strings.ContainsFunc(name, unicode.IsControl)
}

// tokenRole is the token's role: at most admin, and at most the caller's
// own role in org, the first of the caller's organizations. The checks run
// in Rust's order, so each failure answers as it did there.
func tokenRole(a access.Access, requested string) (perm.Role, ids.OrgID, error) {
	role, err := perm.ParseRole(requested)
	if err != nil {
		return "", ids.OrgID{}, err //nolint:wrapcheck // a kerrors already
	}
	if role == perm.Owner {
		return "", ids.OrgID{}, kerrors.New(kerrors.Validation, "tokens are capped at `admin`")
	}
	orgs := a.OrgIDs()
	if len(orgs) == 0 {
		return "", ids.OrgID{}, kerrors.ErrForbidden
	}
	own, ok := a.OrgRole(orgs[0]).Get()
	if !ok {
		return "", ids.OrgID{}, kerrors.ErrForbidden
	}
	if role.Rank() > own.Rank() {
		return "", ids.OrgID{}, kerrors.New(kerrors.Validation, "a `%s` cannot create a `%s` token", own, role)
	}
	return role, orgs[0], nil
}

// tokenScope resolves the project and environment a token is limited to.
func (s *Server) tokenScope(ctx context.Context, a access.Access, req *gen.CreateToken) (project, env opt.Val[uuid.UUID], err error) {
	p, hasProject := req.Project.Get()
	e, hasEnv := req.Environment.Get()
	switch {
	case !hasProject && !hasEnv:
		return project, env, nil
	case !hasProject:
		return project, env, kerrors.New(kerrors.Validation, "environment requires project")
	case !hasEnv:
		found, err := s.findProject(ctx, a, p)
		if err != nil {
			return project, env, err
		}
		return opt.Some(found.project.ID.UUID()), env, nil
	default:
		found, err := s.findEnvironment(ctx, a, p, e)
		if err != nil {
			return project, env, err
		}
		return opt.Some(found.project.project.ID.UUID()), opt.Some(found.env.ID.UUID()), nil
	}
}

// CreateToken creates a personal API token.
func (s *Server) CreateToken(ctx context.Context, req *gen.CreateToken) (gen.CreateTokenRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	if err := a.ForbidToken(); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	name := strings.TrimSpace(req.Name)
	if !ValidTokenName(name) {
		return nil, kerrors.New(kerrors.Validation, "name must be 1–64 printable characters")
	}
	role, org, err := tokenRole(a, req.Role.Or(string(perm.Developer)))
	if err != nil {
		return nil, err
	}
	project, env, err := s.tokenScope(ctx, a, req)
	if err != nil {
		return nil, err
	}
	days := int64(req.ExpiresInDays.Or(tokenDefaultTTLDays))
	if days < 1 || days > tokenMaxTTLDays {
		return nil, kerrors.New(kerrors.Validation, "expires_in_days must be between 1 and %d", tokenMaxTTLDays)
	}
	plaintext, id, secretHash := auth.NewAPIToken()
	token, err := s.deps.Store.CreateToken(ctx, store.NewToken{
		ID:         id,
		OrgID:      org,
		Owner:      a.Current.User.ID,
		Name:       name,
		Prefix:     auth.TokenDisplayPrefix(plaintext),
		SecretHash: secretHash,
		Scope:      model.TokenScope{Role: role, Project: project, Environment: env},
		ExpiresAt:  opt.Some(clock.SaturatingAdd(s.deps.Clock.NowMs(), days*dayMs)),
	})
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	names, err := s.scopeNames(ctx, a)
	if err != nil {
		return nil, err
	}
	return &gen.CreatedToken{Token: plaintext, Info: tokenDto(names, token)}, nil
}

// ListTokens is the caller's API tokens.
func (s *Server) ListTokens(ctx context.Context) ([]gen.TokenDto, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	if err := a.ForbidToken(); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	tokens, err := s.deps.Store.ListTokens(ctx, a.Current.User.ID)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	names, err := s.scopeNames(ctx, a)
	if err != nil {
		return nil, err
	}
	out := make([]gen.TokenDto, 0, len(tokens))
	for _, t := range tokens {
		out = append(out, tokenDto(names, t))
	}
	return out, nil
}

// RevokeToken revokes one of the caller's tokens (immediately effective).
func (s *Server) RevokeToken(ctx context.Context, params gen.RevokeTokenParams) (gen.RevokeTokenRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	if err := a.ForbidToken(); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	notFound := kerrors.New(kerrors.NotFound, "token `%s`", params.Token)
	id, err := ids.Parse[ids.Token](params.Token)
	if err != nil {
		return nil, notFound
	}
	revoked, err := s.deps.Store.RevokeToken(ctx, id, a.Current.User.ID)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !revoked {
		return nil, notFound
	}
	return &gen.RevokeTokenNoContent{}, nil
}
