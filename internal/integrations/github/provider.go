package github

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	gh "github.com/google/go-github/v92/github"

	"github.com/Teamtem-dev/kuben/internal/build"
	"github.com/Teamtem-dev/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/source"
	"github.com/Teamtem-dev/kuben/internal/jsonx"
)

// The answers Kuben reads, with the members serde required.
type (
	tokenBody struct {
		Token     jsonx.Must[string] `json:"token"`
		ExpiresAt jsonx.Must[string] `json:"expires_at"`
	}
	repositoryBody struct {
		ID jsonx.Must[uint64] `json:"id"`
	}
	refObject struct {
		Sha jsonx.Must[string] `json:"sha"`
	}
	refBody struct {
		Object jsonx.Must[refObject] `json:"object"`
	}
	installationAccount struct {
		Login jsonx.Must[string] `json:"login"`
	}
	installationBody struct {
		Account     jsonx.Must[installationAccount] `json:"account"`
		SuspendedAt opt.Val[string]                 `json:"suspended_at"`
	}
	pullBody struct {
		State jsonx.Must[string] `json:"state"`
	}
	// anyBody is any JSON value.
	anyBody struct{}
)

func (b tokenBody) check() error {
	if _, err := b.Token.Get("token"); err != nil {
		return err
	}
	_, err := b.ExpiresAt.Get("expires_at")
	return err
}

func (b repositoryBody) check() error {
	_, err := b.ID.Get("id")
	return err
}

func (b refBody) check() error {
	object, err := b.Object.Get("object")
	if err != nil {
		return err
	}
	_, err = object.Sha.Get("sha")
	return err
}

func (b installationBody) check() error {
	account, err := b.Account.Get("account")
	if err != nil {
		return err
	}
	_, err = account.Login.Get("login")
	return err
}

func (b pullBody) check() error {
	_, err := b.State.Get("state")
	return err
}

func (anyBody) check() error { return nil }

// UnmarshalJSON accepts any JSON value (serde_json::Value).
func (*anyBody) UnmarshalJSON([]byte) error { return nil }

// expiry is when an RFC 3339 `expires_at` is, unix milliseconds (whole
// seconds), or an hour after nowMs when it is not one.
func expiry(value string, nowMs int64) int64 {
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || t.Unix() < 0 {
		return clock.SaturatingAdd(nowMs, time.Hour.Milliseconds())
	}
	return t.Unix() * 1000
}

func (a *App) jwt() (string, error) {
	token, err := appJWT(a.sign, a.appID, a.clock.NowMs())
	if err != nil {
		return "", build.Refused{Reason: err.Error()}
	}
	return token, nil
}

// repoPath is `repos/<owner>/<name>`.
func repoPath(repository source.RepoName) string {
	return "repos/" + segment(repository.Owner()) + "/" + segment(repository.Name())
}

// Installation is the installation id, read with the App's JWT.
func (a *App) Installation(ctx context.Context, id uint64) (Installation, error) {
	jwt, err := a.jwt()
	if err != nil {
		return Installation{}, err
	}
	status, body, err := a.call(ctx, http.MethodGet, fmt.Sprintf("app/installations/%d", id), "Bearer "+jwt, nil)
	if err != nil {
		return Installation{}, err
	}
	answer, err := parse[installationBody](status, body, fmt.Sprintf("installation %d", id))
	if err != nil {
		return Installation{}, err
	}
	// parse checked the members.
	login := answer.Account.Or(installationAccount{}).Login.Or("")
	return Installation{Account: login, Suspended: answer.SuspendedAt.IsSome()}, nil
}

// mint is a new installation token for repository with permissions.
func (a *App) mint(ctx context.Context, installation uint64, repository source.RepoName, permissions gh.InstallationPermissions) (build.FetchToken, error) {
	jwt, err := a.jwt()
	if err != nil {
		return build.FetchToken{}, err
	}
	request := gh.InstallationTokenOptions{Repositories: []string{repository.Name()}, Permissions: &permissions}
	path := fmt.Sprintf("app/installations/%d/access_tokens", installation)
	status, body, err := a.call(ctx, http.MethodPost, path, "Bearer "+jwt, &request)
	if err != nil {
		return build.FetchToken{}, err
	}
	answer, err := parse[tokenBody](status, body, fmt.Sprintf("installation %d", installation))
	if err != nil {
		return build.FetchToken{}, err
	}
	// parse checked the members.
	return build.FetchToken{
		Token:     answer.Token.Or(""),
		ExpiresAt: expiry(answer.ExpiresAt.Or(""), a.clock.NowMs()),
	}, nil
}

// CommitStatus reports status for commit of repository (M4.10). The App
// needs the `statuses: write` permission.
func (a *App) CommitStatus(ctx context.Context, installation uint64, repository source.RepoName, commit string, status CommitStatus) error {
	token, err := a.mint(ctx, installation, repository, gh.InstallationPermissions{Statuses: new("write")})
	if err != nil {
		return err
	}
	request := gh.RepoStatus{
		State:       new(status.State),
		Context:     new(status.Context),
		Description: new(firstChars(status.Description, 140)),
	}
	if url, ok := status.TargetURL.Get(); ok {
		request.TargetURL = new(url)
	}
	path := repoPath(repository) + "/statuses/" + segment(commit)
	code, answer, err := a.call(ctx, http.MethodPost, path, "Bearer "+token.Token, &request)
	if err != nil {
		return err
	}
	_, err = parse[anyBody](code, answer, fmt.Sprintf("the status of %s@%s", repository, commit))
	return err
}

// firstChars is the first n characters of s.
func firstChars(s string, n int) string {
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// metadataToken is a token that reads repository's contents and metadata,
// from the cache while it has more than tokenMargin to live.
func (a *App) metadataToken(ctx context.Context, installation uint64, repository source.RepoName) (build.FetchToken, error) {
	key := tokenKey{installation: installation, repository: repository.String()}
	if token, ok := a.tokens.Get(key); ok && token.ExpiresAt > clock.SaturatingAdd(a.clock.NowMs(), tokenMargin.Milliseconds()) {
		return token, nil
	}
	token, err := a.mint(ctx, installation, repository, gh.InstallationPermissions{
		Contents: new("read"),
		Metadata: new("read"),
	})
	if err != nil {
		return build.FetchToken{}, err
	}
	a.tokens.Add(key, token)
	return token, nil
}

// refHead is the commit gitRef (below `refs/`, already escaped) of
// repository points to; what names it in errors.
func (a *App) refHead(ctx context.Context, installation uint64, repository source.RepoName, gitRef, what string) (build.Head, error) {
	token, err := a.metadataToken(ctx, installation, repository)
	if err != nil {
		return build.Head{}, err
	}
	auth := "Bearer " + token.Token
	status, body, err := a.call(ctx, http.MethodGet, repoPath(repository), auth, nil)
	if err != nil {
		return build.Head{}, err
	}
	repo, err := parse[repositoryBody](status, body, repository.String())
	if err != nil {
		return build.Head{}, err
	}
	status, body, err = a.call(ctx, http.MethodGet, repoPath(repository)+"/git/ref/"+gitRef, auth, nil)
	if err != nil {
		return build.Head{}, err
	}
	head, err := parse[refBody](status, body, repository.String()+"@"+what)
	if err != nil {
		return build.Head{}, err
	}
	// parse checked the members.
	commit, err := source.ParseCommitSha(head.Object.Or(refObject{}).Sha.Or(""))
	if err != nil {
		return build.Head{}, build.Unavailable{Reason: err.Error()}
	}
	return build.Head{Commit: commit, RepositoryID: repo.ID.Or(0)}, nil
}

// PullRequestOpen reports whether pull request number of repository is
// open (M5.1). It needs the App's "Pull requests: read" permission;
// without it the answer is Refused, and nobody concludes the pull request
// is closed.
func (a *App) PullRequestOpen(ctx context.Context, installation uint64, repository source.RepoName, number uint64) (bool, error) {
	token, err := a.mint(ctx, installation, repository, gh.InstallationPermissions{
		PullRequests: new("read"),
		Metadata:     new("read"),
	})
	if err != nil {
		return false, err
	}
	path := fmt.Sprintf("%s/pulls/%d", repoPath(repository), number)
	status, body, err := a.call(ctx, http.MethodGet, path, "Bearer "+token.Token, nil)
	_ = a.Revoke(ctx, token) //nolint:errcheck // best effort: the token expires within the hour
	if err != nil {
		return false, err
	}
	pull, err := parse[pullBody](status, body, fmt.Sprintf("%s#%d", repository, number))
	if err != nil {
		return false, err
	}
	return pull.State.Or("") == "open", nil // parse checked the member
}

// Head is the current head of branch.
func (a *App) Head(ctx context.Context, installation uint64, repository source.RepoName, branch source.BranchName) (build.Head, error) {
	parts := strings.Split(branch.String(), "/")
	for i, part := range parts {
		parts[i] = segment(part)
	}
	return a.refHead(ctx, installation, repository, "heads/"+strings.Join(parts, "/"), branch.String())
}

// PullHead is the current head of pull request number
// (`refs/pull/<n>/head`).
func (a *App) PullHead(ctx context.Context, installation uint64, repository source.RepoName, number uint64) (build.Head, error) {
	return a.refHead(ctx, installation, repository, fmt.Sprintf("pull/%d/head", number), fmt.Sprintf("#%d", number))
}

// FetchToken is a token that can only read repository's contents.
func (a *App) FetchToken(ctx context.Context, installation uint64, repository source.RepoName) (build.FetchToken, error) {
	return a.mint(ctx, installation, repository, gh.InstallationPermissions{Contents: new("read")})
}

// Revoke revokes token. An expired or already revoked token (401) is
// revoked.
func (a *App) Revoke(ctx context.Context, token build.FetchToken) error {
	status, _, err := a.call(ctx, http.MethodDelete, "installation/token", "Bearer "+token.Token, nil)
	if err != nil {
		return err
	}
	if status >= 200 && status <= 299 || status == http.StatusUnauthorized {
		return nil
	}
	return build.Unavailable{Reason: "revoking a token: HTTP " + statusText(status)}
}

// CloneURL is the HTTPS URL build pods clone repository from.
func (a *App) CloneURL(repository source.RepoName) string {
	return a.cloneBase + "/" + repository.String() + ".git"
}
