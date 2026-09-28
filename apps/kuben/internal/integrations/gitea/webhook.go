package gitea

// Gitea's and Forgejo's push webhooks: the body is signed with HMAC-SHA256
// of the webhook's secret, hex-encoded in `X-Gitea-Signature` (Forgejo
// also sends `X-Forgejo-Signature`); the delivery id is `X-Gitea-Delivery`
// (`X-Forgejo-Delivery`) and the event `X-Gitea-Event` (`X-Forgejo-Event`).

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/source"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/gitprovider"
)

// The headers of a delivery, Gitea's first.
var (
	SignatureHeaders = []string{"X-Gitea-Signature", "X-Forgejo-Signature"} //nolint:gochecknoglobals // constant lists
	EventHeaders     = []string{"X-Gitea-Event", "X-Forgejo-Event"}         //nolint:gochecknoglobals // constant lists
	DeliveryHeaders  = []string{"X-Gitea-Delivery", "X-Forgejo-Delivery"}   //nolint:gochecknoglobals // constant lists
)

// PushEvent is the event of a push.
const PushEvent = "push"

// Sign is the hex HMAC-SHA256 of body with secret.
func Sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body) //nolint:errcheck // a hash never fails to write
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify reports whether signature (hex, optionally `sha256=`-prefixed) is
// body's with secret, in constant time.
func Verify(secret, body []byte, signature string) bool {
	if len(secret) == 0 {
		return false
	}
	given, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(signature), "sha256="))
	if err != nil || len(given) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body) //nolint:errcheck // a hash never fails to write
	return hmac.Equal(given, mac.Sum(nil))
}

type pushBody struct {
	Ref        string `json:"ref"`
	After      string `json:"after"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

const zeroCommit = "0000000000000000000000000000000000000000"

// ParsePush reads a delivery of event: none for an event other than a
// branch push; an error for a malformed push.
func ParsePush(event string, body []byte) (opt.Val[gitprovider.Push], error) {
	none := opt.None[gitprovider.Push]()
	if event != PushEvent {
		return none, nil
	}
	var b pushBody
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
	return opt.Some(gitprovider.Push{
		Repository: repo, Branch: branch, After: b.After, Deleted: b.After == zeroCommit,
	}), nil
}
