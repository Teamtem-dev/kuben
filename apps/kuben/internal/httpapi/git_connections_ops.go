package httpapi

// The operations of /api/v1/git/connections (git_connections.go has the
// rules they share).

import (
	"context"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/source"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/gitprovider"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/gitprovider/connect"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

// ListGitConnections is the connections of the caller's organization.
func (s *Server) ListGitConnections(ctx context.Context) (gen.ListGitConnectionsRes, error) {
	c, err := s.callerOrg(ctx, perm.OrgRead, false)
	if err != nil {
		return nil, err
	}
	t, err := s.deps.Store.Tenant(ctx, c.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	rows, err := t.GitConnections(ctx)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	out := make(gen.ListGitConnectionsOKApplicationJSON, 0, len(rows))
	for _, row := range rows {
		out = append(out, s.gitConnectionDto(row))
	}
	return &out, nil
}

// GetGitConnection is one connection.
func (s *Server) GetGitConnection(ctx context.Context, params gen.GetGitConnectionParams) (gen.GetGitConnectionRes, error) {
	c, err := s.callerOrg(ctx, perm.OrgRead, false)
	if err != nil {
		return nil, err
	}
	t, err := s.deps.Store.Tenant(ctx, c.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	conn, err := gitConnection(ctx, t, params.Connection)
	if err != nil {
		return nil, err
	}
	dto := s.gitConnectionDto(conn)
	return &dto, nil
}

// CreateGitConnection checks a token with its provider and saves it, sealed,
// with a new webhook secret.
func (s *Server) CreateGitConnection(ctx context.Context, req *gen.CreateGitConnection) (gen.CreateGitConnectionRes, error) {
	c, err := s.callerOrg(ctx, perm.OrgAdmin, true)
	if err != nil {
		return nil, err
	}
	ring, err := s.keyring()
	if err != nil {
		return nil, err
	}
	body, err := readNewConnection(req, true)
	if err != nil {
		return nil, err
	}
	account, err := s.verifyToken(ctx, body.provider, body.base, body.token)
	if err != nil {
		return nil, err
	}
	id := ids.New[ids.GitConnection]()
	token, err := sealToken(ring, c.org, id, body.token)
	if err != nil {
		return nil, err
	}
	plainSecret, secret, err := newConnectionSecret(ring, c.org, id)
	if err != nil {
		return nil, err
	}
	_, actor := c.access.Actor()
	t, err := s.deps.Store.Tenant(ctx, c.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	saved, err := t.CreateGitConnection(ctx, store.NewGitConnection{
		ID: id, Provider: body.provider, Name: body.name, BaseURL: body.base, Token: token,
		TokenHint: tokenHint(body.token), Username: gitprovider.NonEmpty(account.Username), WebhookSecret: secret,
		DefaultBranch: body.defaultBranch, CreatedBy: actor,
	})
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	switch saved.(type) {
	case store.GitConnectionStored:
	case store.GitConnectionNameTaken:
		return nil, kerrors.New(kerrors.Conflict, "a Git connection named `%s` exists", body.name)
	case store.GitConnectionNotFound:
		return nil, kerrors.New(kerrors.Internal, "the new connection is missing")
	}
	dto, err := s.recordChecked(ctx, t, id, store.GitConnectionCheck{Username: gitprovider.NonEmpty(account.Username)})
	if err != nil {
		return nil, err
	}
	audit := connectionAudit(c, "git.connection.create", body.name, map[string]any{
		"provider": string(body.provider), "baseUrl": body.base, "username": account.Username,
	})
	if err := t.AppendAudit(ctx, audit); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if err := t.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	// The one answer that shows the secret: the provider's webhook needs it.
	dto.WebhookSecret = gen.NewOptString(plainSecret)
	return &dto, nil
}

// recordChecked records check of connection id in t and reads it back.
func (s *Server) recordChecked(
	ctx context.Context, t *store.Tenant, id ids.GitConnectionID, check store.GitConnectionCheck,
) (gen.GitConnectionDto, error) {
	if _, err := t.RecordGitConnectionCheck(ctx, id, check); err != nil {
		return gen.GitConnectionDto{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	conn, found, err := t.GitConnection(ctx, id)
	if err != nil {
		return gen.GitConnectionDto{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found {
		return gen.GitConnectionDto{}, kerrors.New(kerrors.NotFound, "Git connection `%s`", id)
	}
	return s.gitConnectionDto(conn), nil
}

// connectionChange is a checked change of conn, and the account a new token
// (or a new URL) was checked as.
func (s *Server) connectionChange(
	ctx context.Context, conn store.GitConnection, req *gen.UpdateGitConnection,
) (store.GitConnectionChange, opt.Val[gitprovider.Account], error) {
	var change store.GitConnectionChange
	checked := opt.None[gitprovider.Account]()
	if name, ok := req.Name.Get(); ok && name != conn.Name {
		if err := DNSLabel("the connection name", name, 63); err != nil {
			return change, checked, err
		}
		change.Name = opt.Some(name)
	}
	base := conn.BaseURL
	if given, ok := req.BaseUrl.Get(); ok {
		b, err := gitprovider.BaseURL(conn.Provider, given)
		if err != nil {
			return change, checked, kerrors.New(kerrors.Validation, "`baseUrl`: %s", err.Error())
		}
		if b != conn.BaseURL {
			base, change.BaseURL = b, opt.Some(b)
		}
	}
	if req.DefaultBranch.IsSet() {
		branch, err := defaultBranch(req.DefaultBranch.Or(""))
		if err != nil {
			return change, checked, err
		}
		change.DefaultBranch = opt.Some(branch)
	}
	token, newToken := req.Token.Get()
	if !newToken && change.BaseURL.IsNone() {
		return change, checked, nil
	}
	ring, err := s.keyring()
	if err != nil {
		return change, checked, err
	}
	if newToken {
		if err := checkGitToken(token); err != nil {
			return change, checked, err
		}
	} else if token, err = connect.Token(ring, conn); err != nil {
		return change, checked, kerrors.New(kerrors.Unavailable, "%s", err.Error())
	}
	account, err := s.verifyToken(ctx, conn.Provider, base, token)
	if err != nil {
		return change, checked, err
	}
	if newToken {
		sealed, err := sealToken(ring, conn.Org, conn.ID, token)
		if err != nil {
			return change, checked, err
		}
		change.Token = opt.Some(store.SealedToken{Sealed: sealed, Hint: tokenHint(token)})
	}
	return change, opt.Some(account), nil
}

// UpdateGitConnection changes a connection; a new token or URL is checked
// first.
func (s *Server) UpdateGitConnection(
	ctx context.Context, req *gen.UpdateGitConnection, params gen.UpdateGitConnectionParams,
) (gen.UpdateGitConnectionRes, error) {
	c, err := s.callerOrg(ctx, perm.OrgAdmin, true)
	if err != nil {
		return nil, err
	}
	t, err := s.deps.Store.Tenant(ctx, c.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	conn, err := gitConnection(ctx, t, params.Connection)
	if err != nil {
		return nil, err
	}
	change, checked, err := s.connectionChange(ctx, conn, req)
	if err != nil {
		return nil, err
	}
	saved, err := t.UpdateGitConnection(ctx, conn.ID, change)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	switch saved.(type) {
	case store.GitConnectionStored:
	case store.GitConnectionNameTaken:
		return nil, kerrors.New(kerrors.Conflict, "a Git connection named `%s` exists", change.Name.Or(""))
	case store.GitConnectionNotFound:
		return nil, kerrors.New(kerrors.NotFound, "Git connection `%s`", params.Connection)
	}
	check := store.GitConnectionCheck{Username: opt.None[string]()}
	if account, ok := checked.Get(); ok {
		check.Username = gitprovider.NonEmpty(account.Username)
	}
	var dto gen.GitConnectionDto
	if checked.IsSome() {
		dto, err = s.recordChecked(ctx, t, conn.ID, check)
	} else {
		dto, err = s.connectionDtoOf(ctx, t, conn.ID)
	}
	if err != nil {
		return nil, err
	}
	audit := connectionAudit(c, "git.connection.update", dto.Name, map[string]any{
		"renamed": change.Name.IsSome(), "baseUrl": change.BaseURL.IsSome(), "token": change.Token.IsSome(),
		"defaultBranch": change.DefaultBranch.IsSome(),
	})
	if err := t.AppendAudit(ctx, audit); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if err := t.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	return &dto, nil
}

// connectionDtoOf reads connection id back in t.
func (s *Server) connectionDtoOf(ctx context.Context, t *store.Tenant, id ids.GitConnectionID) (gen.GitConnectionDto, error) {
	conn, found, err := t.GitConnection(ctx, id)
	if err != nil {
		return gen.GitConnectionDto{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found {
		return gen.GitConnectionDto{}, kerrors.New(kerrors.NotFound, "Git connection `%s`", id)
	}
	return s.gitConnectionDto(conn), nil
}

// DeleteGitConnection deletes a connection no app source reads through.
func (s *Server) DeleteGitConnection(ctx context.Context, params gen.DeleteGitConnectionParams) (gen.DeleteGitConnectionRes, error) {
	c, err := s.callerOrg(ctx, perm.OrgAdmin, true)
	if err != nil {
		return nil, err
	}
	t, err := s.deps.Store.Tenant(ctx, c.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	conn, err := gitConnection(ctx, t, params.Connection)
	if err != nil {
		return nil, err
	}
	deleted, err := t.DeleteGitConnection(ctx, conn.ID)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	switch d := deleted.(type) {
	case store.GitConnectionRemoved:
	case store.GitConnectionNotFound:
		return nil, kerrors.New(kerrors.NotFound, "Git connection `%s`", params.Connection)
	case store.GitConnectionInUse:
		return nil, kerrors.New(kerrors.Conflict, "%d app source(s) read through Git connection `%s`; change them first",
			d.Bindings, conn.Name)
	}
	audit := connectionAudit(c, "git.connection.delete", conn.Name, map[string]any{"provider": string(conn.Provider)})
	if err := t.AppendAudit(ctx, audit); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if err := t.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	return &gen.DeleteGitConnectionNoContent{}, nil
}

// TestNewGitConnection checks a token without saving anything.
func (s *Server) TestNewGitConnection(ctx context.Context, req *gen.CreateGitConnection) (gen.TestNewGitConnectionRes, error) {
	if _, err := s.callerOrg(ctx, perm.OrgAdmin, true); err != nil {
		return nil, err
	}
	body, err := readNewConnection(req, false)
	if err != nil {
		return nil, err
	}
	client, err := connect.Open(s.gitOptions(), body.provider, body.base, body.token)
	if err != nil {
		return nil, kerrors.New(kerrors.Validation, "%s", err.Error())
	}
	account, err := client.Whoami(ctx)
	dto := checkDto(account, err, s.deps.Clock.NowMs())
	return &dto, nil
}

// TestGitConnection checks a saved connection's token again and records
// what the provider answered.
func (s *Server) TestGitConnection(ctx context.Context, params gen.TestGitConnectionParams) (gen.TestGitConnectionRes, error) {
	c, err := s.callerOrg(ctx, perm.OrgAdmin, false)
	if err != nil {
		return nil, err
	}
	conn, client, err := s.openConnection(ctx, c.org, params.Connection)
	if err != nil {
		return nil, err
	}
	account, err := client.Whoami(ctx)
	dto := checkDto(account, err, s.deps.Clock.NowMs())
	t, err := s.deps.Store.Tenant(ctx, c.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	if _, err := t.RecordGitConnectionCheck(ctx, conn.ID, storedCheck(dto)); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	audit := connectionAudit(c, "git.connection.test", conn.Name, map[string]any{"ok": dto.Ok})
	if err := t.AppendAudit(ctx, audit); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if err := t.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	return &dto, nil
}

// ListGitConnectionRepositories is a page of the repositories a
// connection's token can read.
func (s *Server) ListGitConnectionRepositories(
	ctx context.Context, params gen.ListGitConnectionRepositoriesParams,
) (gen.ListGitConnectionRepositoriesRes, error) {
	c, err := s.callerOrg(ctx, perm.OrgRead, false)
	if err != nil {
		return nil, err
	}
	page := params.Page.Or(1)
	search := params.Search.Or("")
	if page < 1 || page > maxRepositoryPage {
		return nil, kerrors.New(kerrors.Validation, "`page` must be 1 to %d", maxRepositoryPage)
	}
	if len(search) > maxRepositorySearch {
		return nil, kerrors.New(kerrors.Validation, "`search` must be at most %d characters", maxRepositorySearch)
	}
	_, client, err := s.openConnection(ctx, c.org, params.Connection)
	if err != nil {
		return nil, err
	}
	found, err := client.Repositories(ctx, search, int(page))
	if err != nil {
		return nil, gitProviderError(err)
	}
	out := gen.GitRepositoryListDto{Page: page, HasMore: found.HasMore, Repositories: make([]gen.GitRepositoryDto, 0, len(found.Repositories))}
	for _, r := range found.Repositories {
		out.Repositories = append(out.Repositories, gen.GitRepositoryDto{
			ID: optNilString(gitprovider.NonEmpty(r.ID)), FullName: r.FullName, Private: r.Private,
			DefaultBranch: optNilString(r.DefaultBranch), Description: optNilString(r.Description),
			WebUrl: optNilString(r.WebURL), UpdatedAt: optNilInt64(r.UpdatedAt),
		})
	}
	return &out, nil
}

// ListGitConnectionBranches is the branches of one repository a
// connection's token can read.
func (s *Server) ListGitConnectionBranches(
	ctx context.Context, params gen.ListGitConnectionBranchesParams,
) (gen.ListGitConnectionBranchesRes, error) {
	c, err := s.callerOrg(ctx, perm.OrgRead, false)
	if err != nil {
		return nil, err
	}
	if _, err := source.ParseNestedRepoName(params.Repository); err != nil {
		return nil, kerrors.New(kerrors.Validation, "`repository`: %s", err.Error())
	}
	conn, client, err := s.openConnection(ctx, c.org, params.Connection)
	if err != nil {
		return nil, err
	}
	repository, err := conn.Provider.ParseRepository(params.Repository)
	if err != nil {
		return nil, kerrors.New(kerrors.Validation, "`repository`: %s", err.Error())
	}
	branches, err := client.Branches(ctx, repository)
	if err != nil {
		return nil, gitProviderError(err)
	}
	out := make(gen.ListGitConnectionBranchesOKApplicationJSON, 0, len(branches))
	for _, b := range branches {
		out = append(out, gen.GitBranchDto{
			Name: b.Name, Protected: b.Protected, Default: b.Default, Commit: optNilString(b.Commit),
		})
	}
	return &out, nil
}

// RotateGitConnectionWebhookSecret replaces a connection's webhook secret
// with a new one and shows it, once; deliveries signed with the old one are
// refused from now on.
func (s *Server) RotateGitConnectionWebhookSecret(
	ctx context.Context, params gen.RotateGitConnectionWebhookSecretParams,
) (gen.RotateGitConnectionWebhookSecretRes, error) {
	c, err := s.callerOrg(ctx, perm.OrgAdmin, true)
	if err != nil {
		return nil, err
	}
	ring, err := s.keyring()
	if err != nil {
		return nil, err
	}
	t, err := s.deps.Store.Tenant(ctx, c.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	conn, err := gitConnection(ctx, t, params.Connection)
	if err != nil {
		return nil, err
	}
	plain, sealed, err := newConnectionSecret(ring, c.org, conn.ID)
	if err != nil {
		return nil, err
	}
	saved, err := t.UpdateGitConnection(ctx, conn.ID, store.GitConnectionChange{WebhookSecret: opt.Some(sealed)})
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	stored, ok := saved.(store.GitConnectionStored)
	if !ok {
		return nil, kerrors.New(kerrors.NotFound, "Git connection `%s`", params.Connection)
	}
	audit := connectionAudit(c, "git.connection.rotate_webhook_secret", conn.Name, nil)
	if err := t.AppendAudit(ctx, audit); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if err := t.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	return &gen.GitWebhookSecretDto{
		WebhookSecret: plain,
		WebhookUrl:    s.gitConnectionDto(stored.Connection).WebhookUrl,
	}, nil
}
