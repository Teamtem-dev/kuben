package build

// Sources read through a Git connection (2.1): an organization's token at
// GitHub, GitLab or Gitea/Forgejo instead of the GitHub App. A sync reads
// the head through the connection, and a build fetches with the
// connection's stored token, handed to the fetch container exactly like an
// installation token; being the organization's own token, it is not
// revoked when the attempt ends.

import (
	"context"
	"fmt"
	"io"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/source"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

// ConnectionAccess is what a build fetches a connection's repository with.
// Printing it never shows the token.
type ConnectionAccess struct {
	Token FetchToken
	// CloneURL is the HTTPS URL of the repository.
	CloneURL string
}

// String leaves the token out.
func (a ConnectionAccess) String() string {
	return fmt.Sprintf("ConnectionAccess{Token: %s, CloneURL: %s}", a.Token.String(), a.CloneURL)
}

// GoString is String.
func (a ConnectionAccess) GoString() string { return a.String() }

// Format prints String for every verb.
func (a ConnectionAccess) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, a.String()) //nolint:errcheck // fmt.Formatter cannot report a write error
}

// Connections reads sources through Git connections. Every error it
// returns is a [ProviderError].
type Connections interface {
	// Head is the current head of branch of repository, read through
	// connection of org.
	Head(ctx context.Context, org ids.OrgID, connection ids.GitConnectionID, repository source.RepoName,
		branch source.BranchName) (Head, error)
	// Access is the token and clone URL a build of repository fetches with.
	Access(ctx context.Context, org ids.OrgID, connection ids.GitConnectionID, repository source.RepoName) (ConnectionAccess, error)
}

// noConnections answers for a worker without connections: they are not
// configured (no keyring).
type noConnections struct{}

func (noConnections) Head(context.Context, ids.OrgID, ids.GitConnectionID, source.RepoName, source.BranchName) (Head, error) {
	return Head{}, Unavailable{Reason: "Git connections are not configured on this server (no secret keyring)"}
}

func (noConnections) Access(context.Context, ids.OrgID, ids.GitConnectionID, source.RepoName) (ConnectionAccess, error) {
	return ConnectionAccess{}, Unavailable{Reason: "Git connections are not configured on this server (no secret keyring)"}
}

// noApp answers for a worker without the GitHub App: sources of an
// installation cannot be read.
type noApp struct{}

const noAppReason = "the GitHub App is not configured on this server (git.github_app_id)"

func (noApp) Head(context.Context, uint64, source.RepoName, source.BranchName) (Head, error) {
	return Head{}, Refused{Reason: noAppReason}
}

func (noApp) PullHead(context.Context, uint64, source.RepoName, uint64) (Head, error) {
	return Head{}, Refused{Reason: noAppReason}
}

func (noApp) FetchToken(context.Context, uint64, source.RepoName) (FetchToken, error) {
	return FetchToken{}, Refused{Reason: noAppReason}
}

func (noApp) Revoke(context.Context, FetchToken) error { return nil }

func (noApp) CloneURL(repository source.RepoName) string {
	return "https://github.com/" + repository.String() + ".git"
}

// readHead is the head a sync of binding reads, from the App or the
// connection.
func (w *Worker) readHead(ctx context.Context, binding store.SourceBinding) (Head, error) {
	if connection, ok := binding.Connection.Get(); ok {
		if _, pull := binding.PullRequest.Get(); pull {
			return Head{}, NotFound{What: "pull request previews through a Git connection"}
		}
		return w.d.Connections.Head(ctx, binding.Org, connection, binding.Repository, binding.Branch)
	}
	if number, ok := binding.PullRequest.Get(); ok {
		return w.d.Provider.PullHead(ctx, binding.InstallationID, binding.Repository, number)
	}
	return w.d.Provider.Head(ctx, binding.InstallationID, binding.Repository, binding.Branch)
}

// fetchAccess is the token and clone URL attempt fetches with.
func (w *Worker) fetchAccess(ctx context.Context, attempt store.BuildAttempt) (ConnectionAccess, error) {
	if connection, ok := attempt.Connection.Get(); ok {
		return w.d.Connections.Access(ctx, attempt.Org, connection, attempt.Repository)
	}
	token, err := w.d.Provider.FetchToken(ctx, attempt.InstallationID, attempt.Repository)
	if err != nil {
		return ConnectionAccess{}, err
	}
	return ConnectionAccess{Token: token, CloneURL: w.d.Provider.CloneURL(attempt.Repository)}, nil
}
