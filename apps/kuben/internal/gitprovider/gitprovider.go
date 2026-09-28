// Package gitprovider is what Kuben asks of a Git hosting service through
// an organization's token connection (2.1): who the token belongs to and
// what it may do, the repositories and branches it can read, a branch's
// head for a sync, and where a build clones from. GitHub by personal or
// fine-grained token lives here; GitLab (REST v4) and Gitea/Forgejo (REST
// v1) live in internal/integrations/gitlab and internal/integrations/gitea,
// and internal/gitprovider/connect opens a stored connection as one of
// them.
//
// Every error a [Client] returns is a [build.ProviderError]: NotFound for a
// repository or branch the provider does not know (or hides), Refused for a
// token it does not accept, Unavailable for anything that may pass. Tokens
// are sent as headers only and never appear in an error.
package gitprovider

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/build"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/source"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

// The public services a connection reaches when it names no URL.
const (
	GitHubAPI = "https://api.github.com"
	GitLabURL = "https://gitlab.com"
)

// maxBaseURL is the longest base URL stored.
const maxBaseURL = 2048

// Account is what a provider says about a token.
type Account struct {
	// Username is the account the token acts as.
	Username string
	// Scopes are the token's scopes; empty when the provider does not tell.
	Scopes []string
	// Missing are the scopes Kuben needs that the token lacks; empty when
	// the provider does not tell the scopes.
	Missing []string
}

// Repository is one repository a token can read.
type Repository struct {
	// ID is the provider's id.
	ID string
	// FullName is `owner/name` (GitLab: `group/subgroup/name`).
	FullName      string
	Private       bool
	DefaultBranch opt.Val[string]
	Description   opt.Val[string]
	WebURL        opt.Val[string]
	// UpdatedAt is unix milliseconds.
	UpdatedAt opt.Val[int64]
}

// RepositoryPage is one page of repositories.
type RepositoryPage struct {
	Repositories []Repository
	HasMore      bool
}

// Branch is one branch of a repository.
type Branch struct {
	Name      string
	Protected bool
	Default   bool
	// Commit is the branch's head commit.
	Commit opt.Val[string]
}

// PageSize is how many repositories a page holds.
const PageSize = 30

// Client is one connection's token at one provider.
type Client interface {
	// Whoami reads the account and scopes of the token.
	Whoami(ctx context.Context) (Account, error)
	// Repositories is page (from 1) of the repositories the token can read
	// whose name contains search (all when empty).
	Repositories(ctx context.Context, search string, page int) (RepositoryPage, error)
	// Branches are the branches of repository.
	Branches(ctx context.Context, repository source.RepoName) ([]Branch, error)
	// Head is the head of branch of repository.
	Head(ctx context.Context, repository source.RepoName, branch source.BranchName) (build.Head, error)
	// CloneURL is the HTTPS URL a build fetches repository from, with the
	// token as the password of Basic authentication.
	CloneURL(repository source.RepoName) string
	// CommitStatus sets status on commit of repository (the token must be
	// allowed to: GitLab's `api` scope, GitHub's `repo:status`).
	CommitStatus(ctx context.Context, repository source.RepoName, commit string, status CommitStatus) error
}

// CommitStatus is a build's or deployment's outcome shown on its commit.
type CommitStatus struct {
	// State is `pending`, `success`, `failure` or `error`, as GitHub names
	// them; each provider says them in its own words.
	State string
	// Context names the check, e.g. `kuben/build`.
	Context string
	// Description is cut to [MaxStatusDescription] characters.
	Description string
	// TargetURL is where the status links to, if anywhere.
	TargetURL opt.Val[string]
}

// MaxStatusDescription is the most characters of a status's description
// sent (GitHub's limit).
const MaxStatusDescription = 140

// ShortDescription is the description of s, cut to MaxStatusDescription
// characters.
func (s CommitStatus) ShortDescription() string {
	count := 0
	for i := range s.Description {
		if count == MaxStatusDescription {
			return s.Description[:i]
		}
		count++
	}
	return s.Description
}

// statusBody is the body GitHub, Gitea and Forgejo take for a status.
type statusBody struct {
	State       string  `json:"state"`
	Context     string  `json:"context"`
	Description string  `json:"description"`
	TargetURL   *string `json:"target_url,omitempty"`
}

// PostStatus posts status to path in the body GitHub, Gitea and Forgejo
// take.
func PostStatus(ctx context.Context, h *HTTP, path, what string, status CommitStatus) error {
	body := statusBody{
		State: status.State, Context: status.Context, Description: status.ShortDescription(),
		TargetURL: status.TargetURL.Ptr(),
	}
	return h.Post(ctx, path, body, what, nil)
}

// Push is a push a provider reported to a connection's webhook.
type Push struct {
	Repository source.RepoName
	Branch     source.BranchName
	// After is the commit the branch points to now; empty when unknown.
	After string
	// Deleted says the push deleted the branch.
	Deleted bool
}

// DefaultBaseURL is the URL of the public service of provider; empty for
// Gitea, which has none.
func DefaultBaseURL(provider store.GitProvider) string {
	switch provider {
	case store.GitProviderGitHub:
		return GitHubAPI
	case store.GitProviderGitLab:
		return GitLabURL
	case store.GitProviderGitea:
	}
	return ""
}

// BaseURL is given as a connection of provider stores it: its public
// service when given is empty, without a trailing slash or the API suffix
// (`/api/v4`, `/api/v1`), and `https://github.com` as its API. It refuses
// anything but an http(s) URL without credentials, query or fragment.
func BaseURL(provider store.GitProvider, given string) (string, error) {
	given = strings.TrimSpace(given)
	if given == "" {
		if def := DefaultBaseURL(provider); def != "" {
			return def, nil
		}
		return "", fmt.Errorf("a %s connection needs the URL of its server", provider)
	}
	u, err := url.Parse(given)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || strings.ContainsAny(u.Host, "@ \t") {
		return "", fmt.Errorf("%q is not an http(s) URL without credentials", given)
	}
	path := strings.TrimRight(u.EscapedPath(), "/")
	switch provider {
	case store.GitProviderGitLab:
		path = strings.TrimSuffix(path, "/api/v4")
	case store.GitProviderGitea:
		path = strings.TrimSuffix(path, "/api/v1")
	case store.GitProviderGitHub:
		if strings.EqualFold(u.Host, "github.com") && path == "" {
			return GitHubAPI, nil
		}
	}
	out := strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + path
	if len(out) > maxBaseURL || strings.ContainsAny(out, " \t\n@") {
		return "", fmt.Errorf("%q is not a usable URL", given)
	}
	return out, nil
}

// Missing are the scopes of need that have no grant in scopes: need maps a
// needed scope to the scopes that grant it (itself included).
func Missing(scopes []string, need map[string][]string, order []string) []string {
	have := make(map[string]bool, len(scopes))
	for _, s := range scopes {
		have[strings.TrimSpace(s)] = true
	}
	missing := []string{}
	for _, want := range order {
		granted := false
		for _, g := range need[want] {
			if have[g] {
				granted = true
				break
			}
		}
		if !granted {
			missing = append(missing, want)
		}
	}
	return missing
}

// PathEscape escapes value as one path segment, `/` included.
func PathEscape(value string) string { return url.PathEscape(value) }

// RepoPath is `owner/name` of repository, each part escaped.
func RepoPath(repository source.RepoName) string {
	return url.PathEscape(repository.Owner()) + "/" + url.PathEscape(repository.Name())
}

// BranchPath is branch escaped as one path segment (its `/` too).
func BranchPath(branch source.BranchName) string {
	return url.PathEscape(branch.String())
}

// ParseHead is the head of a provider's answer: a commit id and the
// repository's numeric id.
func ParseHead(commit string, repositoryID uint64) (build.Head, error) {
	sha, err := source.ParseCommitSha(commit)
	if err != nil {
		return build.Head{}, build.Unavailable{Reason: err.Error()}
	}
	return build.Head{Commit: sha, RepositoryID: repositoryID}, nil
}
