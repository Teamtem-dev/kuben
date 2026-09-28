// Package gitea is Gitea and Forgejo (which speaks Gitea's API) through an
// organization's access token (2.1): the REST API v1 for the token's
// account, its repositories, branches and heads, and the push webhooks
// they send a connection, signed with HMAC-SHA256 of the connection's
// secret (`X-Gitea-Signature`, `X-Forgejo-Signature`).
//
// A token authenticates as `Authorization: token …`; a build clones with it
// as the password of Basic authentication (Gitea ignores the user name when
// the password is a token). Gitea does not tell a token its own scopes.
package gitea

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/build"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/source"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/gitprovider"
)

// Client is one token at one Gitea or Forgejo.
type Client struct {
	http *gitprovider.HTTP
	base string
}

// New is token at the Gitea at base (without `/api/v1`).
func New(o gitprovider.Options, base, token string) *Client {
	return &Client{
		http: gitprovider.NewHTTP(o, base+"/api/v1", func(h http.Header) { h.Set("Authorization", "token "+token) }),
		base: base,
	}
}

type user struct {
	Login string `json:"login"`
}

type repository struct {
	ID            uint64          `json:"id"`
	FullName      string          `json:"full_name"`
	Private       bool            `json:"private"`
	DefaultBranch opt.Val[string] `json:"default_branch"`
	Description   opt.Val[string] `json:"description"`
	HTMLURL       opt.Val[string] `json:"html_url"`
	UpdatedAt     opt.Val[string] `json:"updated_at"`
}

type searchResult struct {
	OK   bool         `json:"ok"`
	Data []repository `json:"data"`
}

type branch struct {
	Name      string `json:"name"`
	Protected bool   `json:"protected"`
	Commit    struct {
		ID string `json:"id"`
	} `json:"commit"`
}

// Whoami reads `GET /user`. Gitea does not tell a token its scopes.
func (c *Client) Whoami(ctx context.Context) (gitprovider.Account, error) {
	var u user
	if _, err := c.http.Get(ctx, "user", nil, "the token's user", &u); err != nil {
		return gitprovider.Account{}, err
	}
	return gitprovider.Account{Username: u.Login, Scopes: []string{}, Missing: []string{}}, nil
}

// Repositories reads the token's repositories (`GET /user/repos`), or
// searches those it can read (`GET /repos/search`).
func (c *Client) Repositories(ctx context.Context, search string, page int) (gitprovider.RepositoryPage, error) {
	q := url.Values{"limit": {strconv.Itoa(gitprovider.PageSize)}, "page": {strconv.Itoa(max(page, 1))}}
	var repos []repository
	var h http.Header
	var err error
	if search == "" {
		h, err = c.http.Get(ctx, "user/repos", q, "the token's repositories", &repos)
	} else {
		q.Set("q", search)
		q.Set("sort", "updated")
		q.Set("order", "desc")
		var found searchResult
		h, err = c.http.Get(ctx, "repos/search", q, "the token's repositories", &found)
		repos = found.Data
	}
	if err != nil {
		return gitprovider.RepositoryPage{}, err
	}
	out := gitprovider.RepositoryPage{
		Repositories: make([]gitprovider.Repository, 0, len(repos)),
		HasMore:      gitprovider.NextPage(h, len(repos), gitprovider.PageSize),
	}
	for _, r := range repos {
		out.Repositories = append(out.Repositories, gitprovider.Repository{
			ID: strconv.FormatUint(r.ID, 10), FullName: r.FullName, Private: r.Private,
			DefaultBranch: r.DefaultBranch, Description: nonBlank(r.Description), WebURL: r.HTMLURL,
			UpdatedAt: gitprovider.Millis(r.UpdatedAt),
		})
	}
	return out, nil
}

// nonBlank is none for an empty description (Gitea answers "").
func nonBlank(v opt.Val[string]) opt.Val[string] {
	return gitprovider.NonEmpty(v.Or(""))
}

// Branches reads the repository (for its default branch) and up to 50 of
// its branches (Gitea's largest page).
func (c *Client) Branches(ctx context.Context, repo source.RepoName) ([]gitprovider.Branch, error) {
	var r repository
	if _, err := c.http.Get(ctx, "repos/"+gitprovider.RepoPath(repo), nil, repo.String(), &r); err != nil {
		return nil, err
	}
	var branches []branch
	q := url.Values{"limit": {"50"}}
	if _, err := c.http.Get(ctx, "repos/"+gitprovider.RepoPath(repo)+"/branches", q, repo.String(), &branches); err != nil {
		return nil, err
	}
	out := make([]gitprovider.Branch, 0, len(branches))
	for _, b := range branches {
		out = append(out, gitprovider.Branch{
			Name: b.Name, Protected: b.Protected, Default: b.Name == r.DefaultBranch.Or(""),
			Commit: gitprovider.NonEmpty(b.Commit.ID),
		})
	}
	return out, nil
}

// Head reads the repository's id and the branch's commit.
func (c *Client) Head(ctx context.Context, repo source.RepoName, branchName source.BranchName) (build.Head, error) {
	var r repository
	if _, err := c.http.Get(ctx, "repos/"+gitprovider.RepoPath(repo), nil, repo.String(), &r); err != nil {
		return build.Head{}, err
	}
	var b branch
	what := repo.String() + "@" + branchName.String()
	path := "repos/" + gitprovider.RepoPath(repo) + "/branches/" + gitprovider.BranchPath(branchName)
	if _, err := c.http.Get(ctx, path, nil, what, &b); err != nil {
		return build.Head{}, err
	}
	return gitprovider.ParseHead(b.Commit.ID, r.ID)
}

// CloneURL is the repository's HTTPS URL on this server.
func (c *Client) CloneURL(repo source.RepoName) string {
	return c.base + "/" + repo.String() + ".git"
}

// CommitStatus posts `POST /repos/{owner}/{repo}/statuses/{sha}`, whose
// states are GitHub's.
func (c *Client) CommitStatus(
	ctx context.Context, repository source.RepoName, commit string, status gitprovider.CommitStatus,
) error {
	path := "repos/" + gitprovider.RepoPath(repository) + "/statuses/" + gitprovider.PathEscape(commit)
	return gitprovider.PostStatus(ctx, c.http, path, "the status of "+repository.String()+"@"+commit, status) //nolint:wrapcheck // a build.ProviderError
}
