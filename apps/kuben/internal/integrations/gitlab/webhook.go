package gitlab

// GitLab's push webhooks: GitLab sends the secret it was given verbatim in
// `X-Gitlab-Token` (it does not sign), so the check is a constant-time
// comparison; the delivery id is `X-Gitlab-Event-UUID` (or the
// `Idempotency-Key` newer GitLabs send on retries, the same for every try).

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/source"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/gitprovider"
)

// The headers of a delivery.
const (
	TokenHeader       = "X-Gitlab-Token" //nolint:gosec // a header name, not a credential
	EventHeader       = "X-Gitlab-Event"
	IdempotencyHeader = "Idempotency-Key"
	EventUUIDHeader   = "X-Gitlab-Event-UUID"
	// PushEvent is the event of a branch push.
	PushEvent = "Push Hook"
)

// Verify reports whether given is secret, in constant time.
func Verify(secret []byte, given string) bool {
	if len(secret) == 0 {
		return false
	}
	return subtle.ConstantTimeCompare(secret, []byte(given)) == 1
}

type pushBody struct {
	ObjectKind string `json:"object_kind"`
	Ref        string `json:"ref"`
	After      string `json:"after"`
	Project    struct {
		PathWithNamespace string `json:"path_with_namespace"`
	} `json:"project"`
}

// zeroCommit is the `after` of a deleted branch.
const zeroCommit = "0000000000000000000000000000000000000000"

// ParsePush reads a delivery of event: none for an event other than a
// branch push, or a push to a repository Kuben cannot bind (a nested
// group); an error for a malformed push.
func ParsePush(event string, body []byte) (opt.Val[gitprovider.Push], error) {
	none := opt.None[gitprovider.Push]()
	if event != PushEvent {
		return none, nil
	}
	var b pushBody
	if err := json.Unmarshal(body, &b); err != nil {
		return none, fmt.Errorf("a push that is not JSON: %w", err)
	}
	if b.ObjectKind != "push" {
		return none, nil
	}
	name, isBranch := strings.CutPrefix(b.Ref, "refs/heads/")
	if !isBranch {
		return none, nil
	}
	branch, err := source.ParseBranchName(name)
	if err != nil {
		return none, fmt.Errorf("a push to %q: %w", b.Ref, err)
	}
	if b.Project.PathWithNamespace == "" {
		return none, errors.New("a push without project.path_with_namespace")
	}
	repo, err := source.ParseNestedRepoName(b.Project.PathWithNamespace)
	if err != nil {
		return none, nil //nolint:nilerr // a path no source can bind (too deep, or names Kuben refuses)
	}
	return opt.Some(gitprovider.Push{
		Repository: repo, Branch: branch, After: b.After, Deleted: b.After == zeroCommit,
	}), nil
}
