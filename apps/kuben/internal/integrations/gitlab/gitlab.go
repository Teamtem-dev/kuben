// Package gitlab is GitLab (gitlab.com or self-managed) through an
// organization's access token (2.1): the REST API v4 for the token's
// account and scopes, its projects, branches and heads, and the push
// webhooks GitLab sends a connection, which carry the connection's secret
// in `X-Gitlab-Token`.
//
// A personal, project or group access token authenticates with the
// `PRIVATE-TOKEN` header; a build clones with it as the password of Basic
// authentication (GitLab ignores the user name for access tokens).
package gitlab

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/build"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/source"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/gitprovider"
)

// needs are the scopes Kuben needs and the scopes that grant each: the API
// read for repositories, branches and heads, and the repository read for
// the build's clone. `api` grants both.
var needs = map[string][]string{ //nolint:gochecknoglobals // a constant table
	"read_api":        {"read_api", "api"},
	"read_repository": {"read_repository", "write_repository", "api"},
}

// needOrder is the order of needs in answers.
var needOrder = []string{"read_api", "read_repository"} //nolint:gochecknoglobals // a constant table

// Client is one token at one GitLab.
type Client struct {
	http *gitprovider.HTTP
	base string
}

// New is token at the GitLab at base (e.g. https://gitlab.com, without
// `/api/v4`).
func New(o gitprovider.Options, base, token string) *Client {
	return &Client{
		http: gitprovider.NewHTTP(o, base+"/api/v4", func(h http.Header) { h.Set("PRIVATE-TOKEN", token) }),
		base: base,
	}
}

type user struct {
	Username string `json:"username"`
}

type tokenSelf struct {
	Scopes []string `json:"scopes"`
}

type project struct {
	ID                uint64          `json:"id"`
	PathWithNamespace string          `json:"path_with_namespace"`
	Visibility        string          `json:"visibility"`
	DefaultBranch     opt.Val[string] `json:"default_branch"`
	Description       opt.Val[string] `json:"description"`
	WebURL            opt.Val[string] `json:"web_url"`
	LastActivityAt    opt.Val[string] `json:"last_activity_at"`
}

type branch struct {
	Name      string `json:"name"`
	Protected bool   `json:"protected"`
	Default   bool   `json:"default"`
	Commit    struct {
		ID string `json:"id"`
	} `json:"commit"`
}

// projectPath is the project of repository, its path URL-encoded as GitLab
// takes it in place of an id.
func projectPath(repository source.RepoName) string {
	return "projects/" + gitprovider.PathEscape(repository.String())
}

// Whoami reads `GET /user` and, for an access token, its scopes
// (`GET /personal_access_tokens/self`); the scopes stay unknown when
// GitLab does not tell them (an older GitLab, another kind of token).
func (c *Client) Whoami(ctx context.Context) (gitprovider.Account, error) {
	var u user
	if _, err := c.http.Get(ctx, "user", nil, "the token's user", &u); err != nil {
		return gitprovider.Account{}, err
	}
	account := gitprovider.Account{Username: u.Username, Scopes: []string{}, Missing: []string{}}
	var self tokenSelf
	_, err := c.http.Get(ctx, "personal_access_tokens/self", nil, "the token", &self)
	var notFound build.NotFound
	var refused build.Refused
	switch {
	case err == nil:
		account.Scopes = append(account.Scopes, self.Scopes...)
		account.Missing = gitprovider.Missing(self.Scopes, needs, needOrder)
	case errors.As(err, &notFound), errors.As(err, &refused):
		// Not an access token, or a GitLab without the endpoint.
	default:
		return gitprovider.Account{}, err
	}
	return account, nil
}

// Repositories reads the projects the token's account is a member of,
// most recently active first.
func (c *Client) Repositories(ctx context.Context, search string, page int) (gitprovider.RepositoryPage, error) {
	q := url.Values{
		"membership": {"true"}, "simple": {"true"}, "order_by": {"last_activity_at"},
		"per_page": {strconv.Itoa(gitprovider.PageSize)}, "page": {strconv.Itoa(max(page, 1))},
	}
	if search != "" {
		q.Set("search", search)
		q.Set("search_namespaces", "true")
	}
	var projects []project
	h, err := c.http.Get(ctx, "projects", q, "the token's projects", &projects)
	if err != nil {
		return gitprovider.RepositoryPage{}, err
	}
	out := gitprovider.RepositoryPage{
		Repositories: make([]gitprovider.Repository, 0, len(projects)),
		HasMore:      gitprovider.NextPage(h, len(projects), gitprovider.PageSize),
	}
	for _, p := range projects {
		out.Repositories = append(out.Repositories, gitprovider.Repository{
			ID: strconv.FormatUint(p.ID, 10), FullName: p.PathWithNamespace, Private: p.Visibility != "public",
			DefaultBranch: p.DefaultBranch, Description: p.Description, WebURL: p.WebURL,
			UpdatedAt: gitprovider.Millis(p.LastActivityAt),
		})
	}
	return out, nil
}

// Branches reads up to 100 branches of repository.
func (c *Client) Branches(ctx context.Context, repository source.RepoName) ([]gitprovider.Branch, error) {
	var branches []branch
	q := url.Values{"per_page": {"100"}}
	if _, err := c.http.Get(ctx, projectPath(repository)+"/repository/branches", q, repository.String(), &branches); err != nil {
		return nil, err
	}
	out := make([]gitprovider.Branch, 0, len(branches))
	for _, b := range branches {
		out = append(out, gitprovider.Branch{
			Name: b.Name, Protected: b.Protected, Default: b.Default, Commit: gitprovider.NonEmpty(b.Commit.ID),
		})
	}
	return out, nil
}

// Head reads the project's id and the branch's commit.
func (c *Client) Head(ctx context.Context, repository source.RepoName, branchName source.BranchName) (build.Head, error) {
	var p project
	if _, err := c.http.Get(ctx, projectPath(repository), nil, repository.String(), &p); err != nil {
		return build.Head{}, err
	}
	var b branch
	what := repository.String() + "@" + branchName.String()
	path := projectPath(repository) + "/repository/branches/" + gitprovider.BranchPath(branchName)
	if _, err := c.http.Get(ctx, path, nil, what, &b); err != nil {
		return build.Head{}, err
	}
	return gitprovider.ParseHead(b.Commit.ID, p.ID)
}

// CloneURL is the project's HTTPS URL on this GitLab.
func (c *Client) CloneURL(repository source.RepoName) string {
	return c.base + "/" + repository.String() + ".git"
}

// commitStatus is the body of GitLab's `POST /projects/:id/statuses/:sha`.
type commitStatus struct {
	State       string  `json:"state"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	TargetURL   *string `json:"target_url,omitempty"`
}

// stateOf is GitLab's word for a GitHub status state: `failed` for a
// failure or an error, `pending` for anything it does not know.
func stateOf(state string) string {
	switch state {
	case "success":
		return "success"
	case "failure", "error":
		return "failed"
	}
	return "pending"
}

// CommitStatus posts `POST /projects/:id/statuses/:sha` (the token needs
// the `api` scope).
func (c *Client) CommitStatus(
	ctx context.Context, repository source.RepoName, commit string, status gitprovider.CommitStatus,
) error {
	body := commitStatus{
		State: stateOf(status.State), Name: status.Context, Description: status.ShortDescription(),
		TargetURL: status.TargetURL.Ptr(),
	}
	path := projectPath(repository) + "/statuses/" + gitprovider.PathEscape(commit)
	return c.http.Post(ctx, path, body, "the status of "+repository.String()+"@"+commit, nil) //nolint:wrapcheck // a build.ProviderError
}
