// Package connect opens an organization's stored Git connection (2.1): its
// sealed token and webhook secret with the keyring, and a
// [gitprovider.Client] for its provider. [Source] is the build worker's
// [build.Connections] on top of it.
package connect

import (
	"context"
	"fmt"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/build"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/source"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/gitprovider"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/integrations/gitea"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/integrations/gitlab"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/keyring"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

// Open is the client of token at provider's base URL (as [gitprovider.BaseURL]
// stores it).
func Open(o gitprovider.Options, provider store.GitProvider, base, token string) (gitprovider.Client, error) {
	switch provider {
	case store.GitProviderGitHub:
		return gitprovider.NewGitHub(o, base, token), nil
	case store.GitProviderGitLab:
		return gitlab.New(o, base, token), nil
	case store.GitProviderGitea:
		return gitea.New(o, base, token), nil
	}
	return nil, fmt.Errorf("unknown Git provider %q", provider)
}

// Token is c's token, opened.
func Token(k *keyring.Keyring, c store.GitConnection) (string, error) {
	token, err := k.Open(keyring.GitTokenIdentity(c.Org, c.ID), c.Token)
	if err != nil {
		return "", fmt.Errorf("opening the token of Git connection %s: %w", c.Name, err)
	}
	return string(token), nil
}

// WebhookSecret is c's webhook secret, opened.
func WebhookSecret(k *keyring.Keyring, c store.GitConnection) ([]byte, error) {
	secret, err := k.Open(keyring.GitWebhookIdentity(c.Org, c.ID), c.WebhookSecret)
	if err != nil {
		return nil, fmt.Errorf("opening the webhook secret of Git connection %s: %w", c.Name, err)
	}
	return secret, nil
}

// Client is c's client with its opened token.
func Client(o gitprovider.Options, k *keyring.Keyring, c store.GitConnection) (gitprovider.Client, error) {
	token, err := Token(k, c)
	if err != nil {
		return nil, err
	}
	return Open(o, c.Provider, c.BaseURL, token)
}

// Source reads build sources through the connections in Store.
type Source struct {
	Store   *store.Store
	Keyring *keyring.Keyring
	Options gitprovider.Options
}

// opened is connection of org and its client with its token.
type opened struct {
	client gitprovider.Client
	token  string
}

func (s Source) open(ctx context.Context, org ids.OrgID, connection ids.GitConnectionID) (opened, error) {
	t, err := s.Store.Tenant(ctx, org)
	if err != nil {
		return opened{}, build.Unavailable{Reason: err.Error()}
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	c, found, err := t.GitConnection(ctx, connection)
	if err != nil {
		return opened{}, build.Unavailable{Reason: err.Error()}
	}
	if !found {
		return opened{}, build.NotFound{What: "Git connection " + connection.String()}
	}
	token, err := Token(s.Keyring, c)
	if err != nil {
		return opened{}, build.Refused{Reason: err.Error()}
	}
	client, err := Open(s.Options, c.Provider, c.BaseURL, token)
	if err != nil {
		return opened{}, build.Refused{Reason: err.Error()}
	}
	return opened{client: client, token: token}, nil
}

// Head reads the head of branch through the connection.
func (s Source) Head(
	ctx context.Context, org ids.OrgID, connection ids.GitConnectionID, repository source.RepoName, branch source.BranchName,
) (build.Head, error) {
	o, err := s.open(ctx, org, connection)
	if err != nil {
		return build.Head{}, err
	}
	return o.client.Head(ctx, repository, branch) //nolint:wrapcheck // a build.ProviderError
}

// Access is the connection's stored token and the repository's clone URL.
// The token is the organization's: it does not expire with the build.
func (s Source) Access(
	ctx context.Context, org ids.OrgID, connection ids.GitConnectionID, repository source.RepoName,
) (build.ConnectionAccess, error) {
	o, err := s.open(ctx, org, connection)
	if err != nil {
		return build.ConnectionAccess{}, err
	}
	return build.ConnectionAccess{
		Token:    build.FetchToken{Token: o.token},
		CloneURL: o.client.CloneURL(repository),
	}, nil
}

// CommitStatus sets status on commit of repository through the connection.
func (s Source) CommitStatus(
	ctx context.Context, org ids.OrgID, connection ids.GitConnectionID, repository source.RepoName, commit string,
	status gitprovider.CommitStatus,
) error {
	o, err := s.open(ctx, org, connection)
	if err != nil {
		return err
	}
	return o.client.CommitStatus(ctx, repository, commit, status) //nolint:wrapcheck // a build.ProviderError
}
