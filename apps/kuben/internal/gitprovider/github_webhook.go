package gitprovider

// The push webhook of a GitHub token connection: a repository or
// organization webhook the user adds on GitHub (the GitHub App has its
// own, source.ParseGitHub). GitHub signs the body with the webhook's
// secret in `X-Hub-Signature-256` (integrations/github.VerifySignature),
// names the event in `X-GitHub-Event` and the delivery in
// `X-GitHub-Delivery`.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/source"
)

// The headers and events of a GitHub delivery.
const (
	GitHubEventHeader    = "X-GitHub-Event"
	GitHubDeliveryHeader = "X-GitHub-Delivery"
	GitHubPushEvent      = "push"
	// GitHubPingEvent is sent when the webhook is added.
	GitHubPingEvent = "ping"
)

type githubPush struct {
	Ref        string `json:"ref"`
	After      string `json:"after"`
	Deleted    bool   `json:"deleted"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// ParseGitHubPush reads a delivery of event: none for an event other than
// a branch push; an error for a malformed push.
func ParseGitHubPush(event string, body []byte) (opt.Val[Push], error) {
	none := opt.None[Push]()
	if event != GitHubPushEvent {
		return none, nil
	}
	var b githubPush
	if err := json.Unmarshal(body, &b); err != nil {
		return none, fmt.Errorf("a push that is not JSON: %w", err)
	}
	name, isBranch := strings.CutPrefix(b.Ref, "refs/heads/")
	if !isBranch {
		return none, nil
	}
	branch, err := source.ParseBranchName(name)
	if err != nil {
		return none, fmt.Errorf("a push to %q: %w", b.Ref, err)
	}
	if b.Repository.FullName == "" {
		return none, errors.New("a push without repository.full_name")
	}
	repo, err := source.ParseRepoName(b.Repository.FullName)
	if err != nil {
		return none, fmt.Errorf("a push to %q: %w", b.Repository.FullName, err)
	}
	return opt.Some(Push{
		Repository: repo, Branch: branch, After: b.After,
		Deleted: b.Deleted || b.After == strings.Repeat("0", 40),
	}), nil
}
