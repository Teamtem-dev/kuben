package source

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
)

// PushEvent is a push to a branch, as far as Kuben reads it.
type PushEvent struct {
	InstallationID uint64
	RepositoryID   uint64
	Repository     RepoName
	Branch         BranchName
	// After is the head the provider claims. A hint; the worker reads the
	// real head.
	After   CommitSha
	Forced  bool
	Deleted bool
}

// PullAction is what happened to a pull request, as far as previews care
// (M5.1).
type PullAction string

// The pull request actions.
const (
	// PullOpened: opened, reopened or marked ready: a preview should exist.
	PullOpened PullAction = "opened"
	// PullUpdated: new commits: the preview follows its head.
	PullUpdated PullAction = "updated"
	// PullClosed: closed or merged: the preview goes.
	PullClosed PullAction = "closed"
)

// ParsePullAction reads a pull request action name.
func ParsePullAction(s string) (PullAction, error) {
	switch a := PullAction(s); a {
	case PullOpened, PullUpdated, PullClosed:
		return a, nil
	}
	return "", kerr.New(kerr.Validation, "unknown pull request action `%s`", s)
}

// PullEvent is a pull request event.
type PullEvent struct {
	Action         PullAction
	InstallationID uint64
	// Repository is the base repository the pull request targets.
	Repository   RepoName
	RepositoryID uint64
	Number       uint64
	// HeadRepository is the repository the head lives in; another one for a
	// fork, `(deleted)` when the fork is gone.
	HeadRepository string
	HeadBranch     string
	Head           CommitSha
	// Open: the pull request is still open, as the event reports it.
	Open  bool
	Draft bool
	// UpdatedAt is the provider's `updated_at`, in unix milliseconds: events
	// older than what a preview has seen are ignored.
	UpdatedAt int64
}

// FromFork reports that the head is in another repository than the base:
// the change comes from outside the repository's writers.
func (e PullEvent) FromFork() bool {
	return asciiLower(e.HeadRepository) != asciiLower(e.Repository.String())
}

// asciiLower lowers A–Z only, for ASCII case-insensitive comparison.
func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, s)
}

// InstallationAction is an installation lifecycle event Kuben tracks.
type InstallationAction string

// The installation actions.
const (
	InstallationCreated     InstallationAction = "created"
	InstallationDeleted     InstallationAction = "deleted"
	InstallationSuspended   InstallationAction = "suspended"
	InstallationUnsuspended InstallationAction = "unsuspended"
)

// ParseInstallationAction reads an installation action name.
func ParseInstallationAction(s string) (InstallationAction, error) {
	switch a := InstallationAction(s); a {
	case InstallationCreated, InstallationDeleted, InstallationSuspended, InstallationUnsuspended:
		return a, nil
	}
	return "", kerr.New(kerr.Validation, "unknown installation action `%s`", s)
}

// InstallationEvent is a change to an installation of the GitHub App.
type InstallationEvent struct {
	Action         InstallationAction
	InstallationID uint64
	// Account is the login the app is installed on; empty when the payload
	// has none.
	Account string
}

// Ping is the delivery GitHub sends when a webhook is set up.
type Ping struct{}

// Ignored is a tag push, or another event or action Kuben does not act on.
type Ignored struct{}

// WebhookEvent is a parsed webhook delivery: [Ping], [PushEvent],
// [InstallationEvent], [PullEvent] or [Ignored].
//
//sumtype:decl
type WebhookEvent interface{ webhookEvent() }

func (Ping) webhookEvent()              {}
func (PushEvent) webhookEvent()         {}
func (InstallationEvent) webhookEvent() {}
func (PullEvent) webhookEvent()         {}
func (Ignored) webhookEvent()           {}

// required is a field serde refused to miss: absent and null are errors.
type required[T any] struct{ opt.Val[T] }

func (r required[T]) get(field string) (T, error) {
	v, ok := r.Get()
	if !ok {
		return v, &Invalid{Kind: InvalidPayload, Value: "missing field `" + field + "`"}
	}
	return v, nil
}

type rawRepository struct {
	ID       required[uint64] `json:"id"`
	FullName required[string] `json:"full_name"`
}

func (r rawRepository) read() (uint64, string, error) {
	id, err := r.ID.get("id")
	if err != nil {
		return 0, "", err
	}
	name, err := r.FullName.get("full_name")
	return id, name, err
}

type rawAccount struct {
	Login required[string] `json:"login"`
}

type rawInstallation struct {
	ID      required[uint64]    `json:"id"`
	Account opt.Val[rawAccount] `json:"account"`
}

// read is the installation's id and the account login, empty without one.
func (r rawInstallation) read() (uint64, string, error) {
	id, err := r.ID.get("id")
	if err != nil {
		return 0, "", err
	}
	account, ok := r.Account.Get()
	if !ok {
		return id, "", nil
	}
	login, err := account.Login.get("login")
	return id, login, err
}

func payload[T any](body []byte) (T, error) {
	var raw T
	if err := json.Unmarshal(body, &raw); err != nil {
		return raw, &Invalid{Kind: InvalidPayload, Value: err.Error()}
	}
	return raw, nil
}

// ParseGitHub parses a GitHub delivery of type event (the `X-GitHub-Event`
// header). Call only after the signature was verified. A refusal is an
// *Invalid.
func ParseGitHub(event string, body []byte) (WebhookEvent, error) {
	switch event {
	case "ping":
		return Ping{}, nil
	case "push":
		return parsePush(body)
	case "pull_request":
		return parsePull(body)
	case "installation":
		return parseInstallation(body)
	default:
		return Ignored{}, nil
	}
}

type rawPush struct {
	Ref          required[string]         `json:"ref"`
	After        required[string]         `json:"after"`
	Forced       bool                     `json:"forced"`
	Deleted      bool                     `json:"deleted"`
	Repository   required[rawRepository]  `json:"repository"`
	Installation opt.Val[rawInstallation] `json:"installation"`
}

func parsePush(body []byte) (WebhookEvent, error) {
	raw, err := payload[rawPush](body)
	if err != nil {
		return nil, err
	}
	gitRef, err := raw.Ref.get("ref")
	if err != nil {
		return nil, err
	}
	after, err := raw.After.get("after")
	if err != nil {
		return nil, err
	}
	repository, err := raw.Repository.get("repository")
	if err != nil {
		return nil, err
	}
	repositoryID, fullName, err := repository.read()
	if err != nil {
		return nil, err
	}
	installation, linked := raw.Installation.Get()
	installationID, _, err := installation.readIf(linked)
	if err != nil {
		return nil, err
	}
	branch, ok := BranchFromRef(gitRef)
	if !ok {
		return Ignored{}, nil
	}
	if !linked {
		return nil, &Invalid{Kind: InvalidPayload, Value: "a push without an installation"}
	}
	repo, err := ParseRepoName(fullName)
	if err != nil {
		return nil, err
	}
	head, err := ParseCommitSha(after)
	if err != nil {
		return nil, err
	}
	return PushEvent{
		InstallationID: installationID,
		RepositoryID:   repositoryID,
		Repository:     repo,
		Branch:         branch,
		After:          head,
		Forced:         raw.Forced,
		Deleted:        raw.Deleted,
	}, nil
}

// readIf is read for an installation that is there, and nothing otherwise.
func (r rawInstallation) readIf(present bool) (uint64, string, error) {
	if !present {
		return 0, "", nil
	}
	return r.read()
}

type rawInstallationEvent struct {
	Action       required[string]          `json:"action"`
	Installation required[rawInstallation] `json:"installation"`
}

func parseInstallation(body []byte) (WebhookEvent, error) {
	raw, err := payload[rawInstallationEvent](body)
	if err != nil {
		return nil, err
	}
	name, err := raw.Action.get("action")
	if err != nil {
		return nil, err
	}
	installation, err := raw.Installation.get("installation")
	if err != nil {
		return nil, err
	}
	id, account, err := installation.read()
	if err != nil {
		return nil, err
	}
	var action InstallationAction
	switch name {
	case "created":
		action = InstallationCreated
	case "deleted":
		action = InstallationDeleted
	case "suspend":
		action = InstallationSuspended
	case "unsuspend":
		action = InstallationUnsuspended
	default:
		return Ignored{}, nil
	}
	return InstallationEvent{Action: action, InstallationID: id, Account: account}, nil
}

type rawPullRepo struct {
	FullName required[string] `json:"full_name"`
}

type rawPullSide struct {
	Sha required[string] `json:"sha"`
	Ref required[string] `json:"ref"`
	// Repo is null when the fork was deleted.
	Repo opt.Val[rawPullRepo] `json:"repo"`
}

type rawPull struct {
	Number    required[uint64]      `json:"number"`
	State     required[string]      `json:"state"`
	Draft     bool                  `json:"draft"`
	UpdatedAt required[string]      `json:"updated_at"`
	Head      required[rawPullSide] `json:"head"`
}

type rawPullEvent struct {
	Action       required[string]         `json:"action"`
	PullRequest  required[rawPull]        `json:"pull_request"`
	Repository   required[rawRepository]  `json:"repository"`
	Installation opt.Val[rawInstallation] `json:"installation"`
}

// pullFields is a pull request payload with every required field read.
type pullFields struct {
	action, state, updatedAt string
	number                   uint64
	draft                    bool
	sha, headRef, headRepo   string
	repositoryID             uint64
	repository               string
	installationID           uint64
	hasInstallation          bool
}

func (raw rawPullEvent) read() (pullFields, error) {
	var f pullFields
	var err error
	fail := func(e error) (pullFields, error) { return pullFields{}, e }
	if f.action, err = raw.Action.get("action"); err != nil {
		return fail(err)
	}
	pull, err := raw.PullRequest.get("pull_request")
	if err != nil {
		return fail(err)
	}
	if f.number, err = pull.Number.get("number"); err != nil {
		return fail(err)
	}
	if f.state, err = pull.State.get("state"); err != nil {
		return fail(err)
	}
	if f.updatedAt, err = pull.UpdatedAt.get("updated_at"); err != nil {
		return fail(err)
	}
	f.draft = pull.Draft
	head, err := pull.Head.get("head")
	if err != nil {
		return fail(err)
	}
	if f.sha, err = head.Sha.get("sha"); err != nil {
		return fail(err)
	}
	if f.headRef, err = head.Ref.get("ref"); err != nil {
		return fail(err)
	}
	// A deleted fork is still a fork.
	f.headRepo = "(deleted)"
	if repo, ok := head.Repo.Get(); ok {
		if f.headRepo, err = repo.FullName.get("full_name"); err != nil {
			return fail(err)
		}
	}
	repository, err := raw.Repository.get("repository")
	if err != nil {
		return fail(err)
	}
	if f.repositoryID, f.repository, err = repository.read(); err != nil {
		return fail(err)
	}
	installation, linked := raw.Installation.Get()
	f.hasInstallation = linked
	if f.installationID, _, err = installation.readIf(linked); err != nil {
		return fail(err)
	}
	return f, nil
}

func parsePull(body []byte) (WebhookEvent, error) {
	raw, err := payload[rawPullEvent](body)
	if err != nil {
		return nil, err
	}
	f, err := raw.read()
	if err != nil {
		return nil, err
	}
	var action PullAction
	switch f.action {
	case "opened", "reopened", "ready_for_review":
		action = PullOpened
	case "synchronize":
		action = PullUpdated
	case "closed":
		action = PullClosed
	default:
		return Ignored{}, nil
	}
	if !f.hasInstallation {
		return nil, &Invalid{Kind: InvalidPayload, Value: "a pull request without an installation"}
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, f.updatedAt)
	if err != nil {
		return nil, &Invalid{Kind: InvalidPayload, Value: "updated_at: " + err.Error()}
	}
	repo, err := ParseRepoName(f.repository)
	if err != nil {
		return nil, err
	}
	head, err := ParseCommitSha(f.sha)
	if err != nil {
		return nil, err
	}
	return PullEvent{
		Action:         action,
		InstallationID: f.installationID,
		Repository:     repo,
		RepositoryID:   f.repositoryID,
		Number:         f.number,
		HeadRepository: f.headRepo,
		HeadBranch:     f.headRef,
		Head:           head,
		Open:           f.state == "open",
		Draft:          f.draft,
		UpdatedAt:      updatedAt.UnixMilli(),
	}, nil
}
