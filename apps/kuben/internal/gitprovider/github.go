package gitprovider

// GitHub by personal (classic) or fine-grained token: the REST API at
// https://api.github.com, or GitHub Enterprise Server's `…/api/v3`.

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/build"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/source"
)

// githubNeeds: a classic token must read private repositories. A
// fine-grained token has no scopes header; GitHub then refuses what it may
// not read.
var githubNeeds = map[string][]string{"repo": {"repo"}} //nolint:gochecknoglobals // a constant table

// GitHub is a GitHub token.
type GitHub struct {
	http *HTTP
	base string
}

// NewGitHub is the token at the GitHub API base (see [BaseURL]).
func NewGitHub(o Options, base, token string) *GitHub {
	return &GitHub{
		http: NewHTTP(o, base, func(h http.Header) {
			h.Set("Authorization", "Bearer "+token)
			h.Set("Accept", "application/vnd.github+json")
			h.Set("X-GitHub-Api-Version", "2022-11-28")
		}),
		base: base,
	}
}

type githubUser struct {
	Login string `json:"login"`
}

type githubRepo struct {
	ID            uint64          `json:"id"`
	FullName      string          `json:"full_name"`
	Private       bool            `json:"private"`
	DefaultBranch opt.Val[string] `json:"default_branch"`
	Description   opt.Val[string] `json:"description"`
	HTMLURL       opt.Val[string] `json:"html_url"`
	UpdatedAt     opt.Val[string] `json:"updated_at"`
}

type githubBranch struct {
	Name      string `json:"name"`
	Protected bool   `json:"protected"`
	Commit    struct {
		Sha string `json:"sha"`
	} `json:"commit"`
}

// Whoami reads `GET /user` and the classic scopes it answers with.
func (g *GitHub) Whoami(ctx context.Context) (Account, error) {
	var user githubUser
	h, err := g.http.Get(ctx, "user", nil, "the token's user", &user)
	if err != nil {
		return Account{}, err
	}
	account := Account{Username: user.Login, Scopes: []string{}, Missing: []string{}}
	if header, ok := h["X-Oauth-Scopes"]; ok && len(header) > 0 {
		for s := range strings.SplitSeq(header[0], ",") {
			if s = strings.TrimSpace(s); s != "" {
				account.Scopes = append(account.Scopes, s)
			}
		}
		account.Missing = Missing(account.Scopes, githubNeeds, []string{"repo"})
	}
	return account, nil
}

// Repositories reads `GET /user/repos`; GitHub cannot search it, so a
// search reads bigger pages and keeps the matches.
func (g *GitHub) Repositories(ctx context.Context, search string, page int) (RepositoryPage, error) {
	size := PageSize
	if search != "" {
		size = 100
	}
	q := url.Values{
		"per_page": {strconv.Itoa(size)}, "page": {strconv.Itoa(max(page, 1))}, "sort": {"updated"},
		"affiliation": {"owner,collaborator,organization_member"},
	}
	var repos []githubRepo
	h, err := g.http.Get(ctx, "user/repos", q, "the token's repositories", &repos)
	if err != nil {
		return RepositoryPage{}, err
	}
	out := RepositoryPage{Repositories: []Repository{}, HasMore: NextPage(h, len(repos), size)}
	needle := strings.ToLower(search)
	for _, r := range repos {
		if needle != "" && !strings.Contains(strings.ToLower(r.FullName), needle) {
			continue
		}
		out.Repositories = append(out.Repositories, Repository{
			ID: strconv.FormatUint(r.ID, 10), FullName: r.FullName, Private: r.Private,
			DefaultBranch: r.DefaultBranch, Description: r.Description, WebURL: r.HTMLURL,
			UpdatedAt: Millis(r.UpdatedAt),
		})
	}
	return out, nil
}

// Branches reads the repository (for its default branch) and its
// branches, up to 100.
func (g *GitHub) Branches(ctx context.Context, repository source.RepoName) ([]Branch, error) {
	var repo githubRepo
	if _, err := g.http.Get(ctx, "repos/"+RepoPath(repository), nil, repository.String(), &repo); err != nil {
		return nil, err
	}
	var branches []githubBranch
	q := url.Values{"per_page": {"100"}}
	if _, err := g.http.Get(ctx, "repos/"+RepoPath(repository)+"/branches", q, repository.String(), &branches); err != nil {
		return nil, err
	}
	out := make([]Branch, 0, len(branches))
	for _, b := range branches {
		out = append(out, Branch{
			Name: b.Name, Protected: b.Protected, Default: b.Name == repo.DefaultBranch.Or(""),
			Commit: NonEmpty(b.Commit.Sha),
		})
	}
	return out, nil
}

// Head reads the repository's id and the branch's commit.
func (g *GitHub) Head(ctx context.Context, repository source.RepoName, branch source.BranchName) (build.Head, error) {
	var repo githubRepo
	if _, err := g.http.Get(ctx, "repos/"+RepoPath(repository), nil, repository.String(), &repo); err != nil {
		return build.Head{}, err
	}
	var b githubBranch
	what := repository.String() + "@" + branch.String()
	if _, err := g.http.Get(ctx, "repos/"+RepoPath(repository)+"/branches/"+BranchPath(branch), nil, what, &b); err != nil {
		return build.Head{}, err
	}
	return ParseHead(b.Commit.Sha, repo.ID)
}

// CloneURL is on github.com for the public API, on the Enterprise host
// (its API is `…/api/v3`) otherwise.
func (g *GitHub) CloneURL(repository source.RepoName) string {
	web := "https://github.com"
	if g.base != GitHubAPI {
		web = strings.TrimSuffix(g.base, "/api/v3")
	}
	return web + "/" + repository.String() + ".git"
}

// Millis is an RFC 3339 time as unix milliseconds; none when it is not
// one.
func Millis(text opt.Val[string]) opt.Val[int64] {
	s, ok := text.Get()
	if !ok {
		return opt.None[int64]()
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return opt.None[int64]()
	}
	return opt.Some(t.UnixMilli())
}

// NonEmpty is s, none when empty.
func NonEmpty(s string) opt.Val[string] {
	if s == "" {
		return opt.None[string]()
	}
	return opt.Some(s)
}

// CommitStatus posts `POST /repos/{owner}/{repo}/statuses/{sha}`.
func (g *GitHub) CommitStatus(ctx context.Context, repository source.RepoName, commit string, status CommitStatus) error {
	path := "repos/" + RepoPath(repository) + "/statuses/" + PathEscape(commit)
	return PostStatus(ctx, g.http, path, "the status of "+repository.String()+"@"+commit, status)
}
