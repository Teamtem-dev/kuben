package httpapi

// Git connections (2.1): an organization's token for one account at GitHub,
// GitLab or Gitea/Forgejo, which app sources read through instead of the
// GitHub App. Organization admins add, change, check and delete them;
// members read them and browse their repositories and branches.
//
// A token is checked with the provider before it is saved (the account it
// acts as, and its scopes where the provider tells them) and sealed with
// the keyring for the connection; the webhook secret is generated here and
// sealed the same way. Neither is ever returned.

import (
	"context"
	"errors"
	"strings"
	"unicode"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/build"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/source"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/gitprovider"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/gitprovider/connect"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/keyring"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

const (
	// maxGitToken is the longest token accepted.
	maxGitToken = 4096
	// maxRepositorySearch is the longest repository search accepted.
	maxRepositorySearch = 200
	// maxRepositoryPage is the last page of repositories that can be asked for.
	maxRepositoryPage = 1000
	// gitConnectionTarget is the target kind of the audit records.
	gitConnectionTarget = "gitConnection"
)

// gitOptions are how the API reaches Git providers.
func (s *Server) gitOptions() gitprovider.Options {
	return gitprovider.Options{Transport: s.deps.GitTransport}
}

// webhookPath is where provider delivers the push events of connection
// id (for GitHub, a repository or organization webhook the user adds).
func webhookPath(provider store.GitProvider, id ids.GitConnectionID) opt.Val[string] {
	switch provider {
	case store.GitProviderGitLab:
		return opt.Some(gitlabWebhookPrefix + id.String())
	case store.GitProviderGitea:
		return opt.Some(giteaWebhookPrefix + id.String())
	case store.GitProviderGitHub:
		return opt.Some(githubWebhookPrefix + id.String())
	}
	return opt.None[string]()
}

// gitConnectionDto is c as the API shows it: never its token or secret.
func (s *Server) gitConnectionDto(c store.GitConnection) gen.GitConnectionDto {
	hook := webhookPath(c.Provider, c.ID)
	if path, ok := hook.Get(); ok {
		if public, ok := s.deps.Config.Server.PublicURL.Get(); ok && public != "" {
			hook = opt.Some(strings.TrimRight(public, "/") + path)
		}
	}
	return gen.GitConnectionDto{
		ID:            c.ID.String(),
		Name:          c.Name,
		Provider:      gen.GitProviderDto(c.Provider),
		BaseUrl:       c.BaseURL,
		AuthKind:      c.AuthKind,
		Username:      optNilString(c.Username),
		DefaultBranch: optNilString(c.DefaultBranch),
		HasToken:      len(c.Token.Ciphertext) > 0,
		TokenHint:     c.TokenHint,
		WebhookUrl:    optNilString(hook),
		CreatedAt:     c.CreatedAt,
		UpdatedAt:     c.UpdatedAt,
		LastCheckedAt: optNilInt64(c.LastCheckedAt),
		LastError:     optNilString(c.LastError),
	}
}

// checkGitToken refuses a token that cannot be one.
func checkGitToken(token string) error {
	if token == "" || len(token) > maxGitToken || strings.ContainsFunc(token, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r) || r > unicode.MaxASCII
	}) {
		return kerrors.New(kerrors.Validation, "the token must be 1 to %d printable ASCII characters", maxGitToken)
	}
	return nil
}

// tokenHint is the last four characters of token at most.
func tokenHint(token string) string {
	if len(token) <= 8 {
		return ""
	}
	return token[len(token)-4:]
}

// defaultBranch reads a connection's default branch; none when empty.
func defaultBranch(given string) (opt.Val[string], error) {
	if given == "" {
		return opt.None[string](), nil
	}
	b, err := source.ParseBranchName(given)
	if err != nil {
		return opt.None[string](), kerrors.New(kerrors.Validation, "`defaultBranch`: %s", err.Error())
	}
	return opt.Some(b.String()), nil
}

// newConnection is a checked create body.
type newConnection struct {
	provider      store.GitProvider
	name          string
	base          string
	token         string
	defaultBranch opt.Val[string]
}

// readNewConnection checks body; with named the name must be valid too.
func readNewConnection(body *gen.CreateGitConnection, named bool) (newConnection, error) {
	provider, err := store.ParseGitProvider(string(body.Provider))
	if err != nil {
		return newConnection{}, kerrors.New(kerrors.Validation, "%s", err.Error())
	}
	if named {
		if err := DNSLabel("the connection name", body.Name, 63); err != nil {
			return newConnection{}, err
		}
	}
	base, err := gitprovider.BaseURL(provider, body.BaseUrl.Or(""))
	if err != nil {
		return newConnection{}, kerrors.New(kerrors.Validation, "`baseUrl`: %s", err.Error())
	}
	if err := checkGitToken(body.Token); err != nil {
		return newConnection{}, err
	}
	branch, err := defaultBranch(body.DefaultBranch.Or(""))
	if err != nil {
		return newConnection{}, err
	}
	return newConnection{provider: provider, name: body.Name, base: base, token: body.Token, defaultBranch: branch}, nil
}

// gitProviderError is what a provider's answer means for an API caller:
// a refused token or unknown repository is the caller's to fix.
func gitProviderError(err error) error {
	var notFound build.NotFound
	var refused build.Refused
	var unavailable build.Unavailable
	switch {
	case errors.As(err, &notFound):
		return kerrors.New(kerrors.NotFound, "the Git provider does not know %s", notFound.What)
	case errors.As(err, &refused):
		return kerrors.New(kerrors.Validation, "the Git provider refused the token: %s", refused.Reason)
	case errors.As(err, &unavailable):
		return kerrors.New(kerrors.Unavailable, "the Git provider: %s", unavailable.Reason)
	}
	return err
}

// verifyToken asks the provider about a token about to be saved: the
// account, or why it will not do.
func (s *Server) verifyToken(ctx context.Context, provider store.GitProvider, base, token string) (gitprovider.Account, error) {
	client, err := connect.Open(s.gitOptions(), provider, base, token)
	if err != nil {
		return gitprovider.Account{}, kerrors.New(kerrors.Validation, "%s", err.Error())
	}
	account, err := client.Whoami(ctx)
	if err != nil {
		var notFound build.NotFound
		if errors.As(err, &notFound) {
			return gitprovider.Account{}, kerrors.New(kerrors.Validation, "%s does not answer as a %s API", base, provider)
		}
		return gitprovider.Account{}, gitProviderError(err)
	}
	if len(account.Missing) > 0 {
		return gitprovider.Account{}, kerrors.New(kerrors.Validation, "the token lacks the scopes %s",
			strings.Join(account.Missing, ", "))
	}
	return account, nil
}

// checkDto is a check that found account, or failed with err, at nowMs.
func checkDto(account gitprovider.Account, err error, nowMs int64) gen.GitConnectionCheckDto {
	dto := gen.GitConnectionCheckDto{CheckedAt: nowMs, Scopes: []string{}, MissingScopes: []string{}}
	if err != nil {
		dto.Error = gen.NewOptNilString(errorText(err))
		dto.Username.SetToNull()
		return dto
	}
	dto.Ok = len(account.Missing) == 0
	dto.Username = gen.NewOptNilString(account.Username)
	dto.Scopes = append(dto.Scopes, account.Scopes...)
	dto.MissingScopes = append(dto.MissingScopes, account.Missing...)
	if dto.Ok {
		dto.Error.SetToNull()
	} else {
		dto.Error = gen.NewOptNilString("the token lacks the scopes " + strings.Join(account.Missing, ", "))
	}
	return dto
}

// errorText is err in the words an API answer uses.
func errorText(err error) string {
	var e *kerrors.Error
	if errors.As(gitProviderError(err), &e) {
		return e.Detail
	}
	return err.Error()
}

// storedCheck is the check of a saved connection as the store records it.
func storedCheck(dto gen.GitConnectionCheckDto) store.GitConnectionCheck {
	check := store.GitConnectionCheck{Username: opt.None[string]()}
	if u, ok := dto.Username.Get(); ok && u != "" {
		check.Username = opt.Some(u)
	}
	if e, ok := dto.Error.Get(); ok {
		check.Error = opt.Some(e)
	}
	return check
}

// connectionAudit is an audit record of action on connection name.
func connectionAudit(c accessOrg, action, name string, data map[string]any) store.NewAudit {
	record := requestAudit(c.access, action, gitConnectionTarget, name)
	if data != nil {
		record.Data = opt.Some[any](data)
	}
	return record
}

// gitConnectionID reads a connection id of a path.
func gitConnectionID(text string) (ids.GitConnectionID, error) {
	id, err := ids.Parse[ids.GitConnection](text)
	if err != nil {
		return ids.GitConnectionID{}, kerrors.New(kerrors.NotFound, "Git connection `%s`", text)
	}
	return id, nil
}

// gitConnection is the caller's organization's connection text, read in t.
func gitConnection(ctx context.Context, t *store.Tenant, text string) (store.GitConnection, error) {
	id, err := gitConnectionID(text)
	if err != nil {
		return store.GitConnection{}, err
	}
	c, found, err := t.GitConnection(ctx, id)
	if err != nil {
		return store.GitConnection{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found {
		return store.GitConnection{}, kerrors.New(kerrors.NotFound, "Git connection `%s`", text)
	}
	return c, nil
}

// openConnection is connection text of org and its client, for a read.
func (s *Server) openConnection(ctx context.Context, org ids.OrgID, text string) (store.GitConnection, gitprovider.Client, error) {
	ring, err := s.keyring()
	if err != nil {
		return store.GitConnection{}, nil, err
	}
	t, err := s.deps.Store.Tenant(ctx, org)
	if err != nil {
		return store.GitConnection{}, nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	c, err := gitConnection(ctx, t, text)
	if err != nil {
		return store.GitConnection{}, nil, err
	}
	client, err := connect.Client(s.gitOptions(), ring, c)
	if err != nil {
		return store.GitConnection{}, nil, kerrors.New(kerrors.Unavailable, "%s", err.Error())
	}
	return c, client, nil
}

// newConnectionSecret is a new webhook secret for connection id of org, and
// it sealed.
func newConnectionSecret(ring *keyring.Keyring, org ids.OrgID, id ids.GitConnectionID) (string, store.SealedBytes, error) {
	secret := randomToken(32)
	sealed, err := ring.Seal(keyring.GitWebhookIdentity(org, id), []byte(secret))
	if err != nil {
		return "", store.SealedBytes{}, err //nolint:wrapcheck // a keyring error, answered as internal
	}
	return secret, sealed, nil
}

// sealToken seals token for connection id of org.
func sealToken(ring *keyring.Keyring, org ids.OrgID, id ids.GitConnectionID, token string) (store.SealedBytes, error) {
	return ring.Seal(keyring.GitTokenIdentity(org, id), []byte(token)) //nolint:wrapcheck // a keyring error, answered as internal
}
