package api

// Git providers (M3): the GitHub App's installations and its webhook
// (routes/git.rs).
//
// An organization admin links an installation once; GitHub cannot say which
// Kuben organization an installation belongs to. The webhook is served
// outside the session and CSRF layers (GitHub sends neither) and trusts only
// the HMAC signature. A push is a hint: it is recorded once per delivery id
// in the inbox and turned into `source.sync` operations, which read the real
// branch head from GitHub before anything is built.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/gen"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/github"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/problem"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/perm"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/source"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/build"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
)

const (
	// githubWebhookPath is where GitHub delivers webhooks.
	githubWebhookPath = "/api/v1/webhooks/github"
	// githubWebhookBodyLimit is the largest delivery accepted; GitHub caps
	// payloads at 25 MiB, pushes are far smaller.
	githubWebhookBodyLimit = 5 << 20
	// maxDeliveryID is the longest delivery id accepted.
	maxDeliveryID = 128
)

// github is the configured GitHub App, or 503.
func (s *Server) github() (*github.App, error) {
	app, ok := s.deps.GitHub.Get()
	if !ok || app == nil {
		return nil, kerr.New(kerr.Unavailable, "Git sources are not configured on this server (git.github_app_id)")
	}
	return app, nil
}

// providerError is what an answer from GitHub means for the caller. An
// error that is not a [build.ProviderError] is returned as it is.
func providerError(err error) error {
	var notFound build.NotFound
	var refused build.Refused
	var unavailable build.Unavailable
	switch {
	case errors.As(err, &notFound):
		return kerr.New(kerr.Validation, "GitHub does not know %s", notFound.What)
	case errors.As(err, &refused):
		return kerr.New(kerr.Validation, "GitHub refused access: %s", refused.Reason)
	case errors.As(err, &unavailable):
		return kerr.New(kerr.Unavailable, "GitHub: %s", unavailable.Reason)
	}
	return err
}

// ListGitInstallations is the GitHub App installations linked to the
// caller's organization.
func (s *Server) ListGitInstallations(ctx context.Context) (gen.ListGitInstallationsRes, error) {
	c, err := s.callerOrg(ctx, perm.OrgRead, false)
	if err != nil {
		return nil, err
	}
	t, err := s.deps.Store.Tenant(ctx, c.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	rows, err := t.Installations(ctx)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	out := make(gen.ListGitInstallationsOKApplicationJSON, 0, len(rows))
	for _, i := range rows {
		out = append(out, gen.InstallationDto{
			InstallationId: int64(i.ID), //nolint:gosec // stored as BIGINT
			Account:        i.Account,
			Suspended:      i.Suspended,
		})
	}
	return &out, nil
}

// LinkGitInstallation links a GitHub App installation to the caller's
// organization. GitHub is asked first; an installation stays with the
// organization that linked it.
func (s *Server) LinkGitInstallation(ctx context.Context, req *gen.LinkInstallation) (gen.LinkGitInstallationRes, error) {
	c, err := s.callerOrg(ctx, perm.OrgAdmin, true)
	if err != nil {
		return nil, err
	}
	app, err := s.github()
	if err != nil {
		return nil, err
	}
	id := uint64(req.InstallationId) //nolint:gosec // the contract's minimum is 0
	installation, err := app.Installation(ctx, id)
	if err != nil {
		return nil, providerError(err)
	}
	if installation.Suspended {
		return nil, kerr.New(kerr.Validation, "the installation is suspended on GitHub")
	}
	t, err := s.deps.Store.Tenant(ctx, c.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	linked, err := t.LinkInstallation(ctx, id, installation.Account)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !linked {
		return nil, kerr.New(kerr.Conflict, "the installation is linked to another organization")
	}
	if err := t.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	return &gen.InstallationDto{InstallationId: req.InstallationId, Account: installation.Account}, nil
}

// headerValue is the first value of header name as axum's to_str read it:
// false when absent or when it holds anything but visible ASCII and tabs.
func headerValue(h http.Header, name string) (string, bool) {
	values := h.Values(name)
	if len(values) == 0 {
		return "", false
	}
	v := values[0]
	for i := range len(v) {
		if b := v[i]; b != '\t' && (b < 0x20 || b > 0x7e) {
			return "", false
		}
	}
	return v, true
}

// outcome is the webhook's answer body.
type outcome struct {
	Outcome string `json:"outcome"`
}

// answerWebhook writes {"outcome": what} with status.
func answerWebhook(w http.ResponseWriter, status int, what string) {
	writeCIJSON(w, status, outcome{Outcome: what})
}

// githubWebhook is `POST /api/v1/webhooks/github`, with its own body
// limit; only POST is routed to it, as axum's post() did.
func (s *Server) githubWebhook() http.Handler {
	deliver := limitBody(githubWebhookBodyLimit, http.HandlerFunc(s.serveGithubWebhook))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		deliver.ServeHTTP(w, r)
	})
}

// serveGithubWebhook answers a signed delivery from the GitHub App.
func (s *Server) serveGithubWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	app, ok := s.deps.GitHub.Get()
	if !ok || app == nil {
		answerWebhook(w, http.StatusNotFound, "notConfigured")
		return
	}
	signature, signed := headerValue(r.Header, github.SignatureHeader)
	if !signed || !app.Verify(body, signature) {
		s.deps.Logger.Warn("a GitHub webhook delivery with a missing or wrong signature was refused")
		answerWebhook(w, http.StatusUnauthorized, "badSignature")
		return
	}
	event, hasEvent := headerValue(r.Header, "x-github-event")
	delivery, hasDelivery := headerValue(r.Header, "x-github-delivery")
	if !hasEvent || !hasDelivery {
		answerWebhook(w, http.StatusBadRequest, "missingHeaders")
		return
	}
	if delivery == "" || len(delivery) > maxDeliveryID {
		answerWebhook(w, http.StatusBadRequest, "badDeliveryId")
		return
	}
	parsed, err := source.ParseGitHub(event, body)
	if err != nil {
		s.deps.Logger.Warn("a malformed GitHub delivery was refused", "delivery", delivery, "error", err)
		answerWebhook(w, http.StatusBadRequest, "malformed")
		return
	}
	status, what, err := s.githubEvent(r.Context(), parsed, delivery, body)
	if err != nil {
		s.deps.Logger.Error("a GitHub delivery could not be recorded", "delivery", delivery, "error", err)
		problem.Write(w, s.deps.Logger, err)
		return
	}
	answerWebhook(w, status, what)
}

// githubEvent acts on one verified delivery: the status and outcome to
// answer.
func (s *Server) githubEvent(ctx context.Context, parsed source.WebhookEvent, delivery string, body []byte) (int, string, error) {
	switch e := parsed.(type) {
	case source.Ping:
		return http.StatusOK, "pong", nil
	case source.Ignored:
		return http.StatusAccepted, "ignored", nil
	case source.InstallationEvent:
		return s.installationEvent(ctx, e.Action, e.InstallationID)
	case source.PushEvent:
		return s.pushEvent(ctx, delivery, body, e)
	case source.PullEvent:
		return s.pullEvent(ctx, delivery, body, e)
	}
	return 0, "", kerr.New(kerr.Internal, "unknown webhook event %T", parsed)
}

func (s *Server) installationEvent(ctx context.Context, action source.InstallationAction, id uint64) (int, string, error) {
	var suspended bool
	switch action {
	case source.InstallationCreated:
		// Linking needs an organization admin; GitHub cannot name the org.
		return http.StatusAccepted, "linkInKuben", nil
	case source.InstallationDeleted, source.InstallationSuspended:
		suspended = true
	case source.InstallationUnsuspended:
		suspended = false
	}
	known, err := s.deps.Store.SetInstallationSuspended(ctx, id, suspended)
	if err != nil {
		return 0, "", err //nolint:wrapcheck // a store error, answered as internal
	}
	if known {
		return http.StatusAccepted, "recorded", nil
	}
	return http.StatusAccepted, "unknown", nil
}

// receiveDelivery finds the organization of installation and records the
// delivery in its inbox. It answers instead when the installation is
// unknown or suspended, or the delivery was seen (or changed).
func (s *Server) receiveDelivery(
	ctx context.Context, installation uint64, delivery string, body []byte,
) (ids.OrgID, opt.Val[webhookAnswer], error) {
	link, known, err := s.deps.Store.GitInstallationOrg(ctx, installation)
	if err != nil {
		return ids.OrgID{}, opt.None[webhookAnswer](), err //nolint:wrapcheck // a store error, answered as internal
	}
	if !known {
		return ids.OrgID{}, opt.Some(webhookAnswer{http.StatusAccepted, "unknownInstallation"}), nil
	}
	if link.Suspended {
		return ids.OrgID{}, opt.Some(webhookAnswer{http.StatusAccepted, "suspended"}), nil
	}
	received, err := s.deps.Store.Receive(ctx, link.Org, store.GitHub, delivery, body)
	if err != nil {
		return ids.OrgID{}, opt.None[webhookAnswer](), err //nolint:wrapcheck // a store error, answered as internal
	}
	switch received.(type) {
	case store.ReceivedNew:
		return link.Org, opt.None[webhookAnswer](), nil
	case store.ReceivedDuplicate:
		return ids.OrgID{}, opt.Some(webhookAnswer{http.StatusOK, "duplicate"}), nil
	case store.ReceivedChanged:
		return ids.OrgID{}, opt.Some(webhookAnswer{http.StatusConflict, "deliveryChanged"}), nil
	}
	return ids.OrgID{}, opt.None[webhookAnswer](), kerr.New(kerr.Internal, "unknown receipt %T", received)
}

// webhookAnswer is a status and an outcome.
type webhookAnswer struct {
	status  int
	outcome string
}

// pullEvent records a pull request delivery and hands it to the previews.
func (s *Server) pullEvent(ctx context.Context, delivery string, body []byte, pull source.PullEvent) (int, string, error) {
	org, answer, err := s.receiveDelivery(ctx, pull.InstallationID, delivery, body)
	if err != nil {
		return 0, "", err
	}
	if a, ok := answer.Get(); ok {
		return a.status, a.outcome, nil
	}
	outcomes, err := s.onPull(ctx, org, pull)
	if err != nil {
		return 0, "", err
	}
	for _, o := range outcomes {
		s.deps.Logger.Info("pull request event", "delivery", delivery, "project", o.project,
			"result", o.result, "number", pull.Number)
	}
	if len(outcomes) == 0 {
		return http.StatusAccepted, "noPreview", nil
	}
	return http.StatusAccepted, "previews", nil
}

// pullOutcome is what one project's previews did with a pull request event
// (previews.rs PullOutcome).
type pullOutcome struct {
	project string
	result  string
}

// onPull is previews::on_pull: previews (M5.1, previews.rs) are not ported
// yet, so no project follows pull requests and a pull request event changes
// nothing, as in a Rust installation without preview settings.
func (s *Server) onPull(context.Context, ids.OrgID, source.PullEvent) ([]pullOutcome, error) {
	return nil, nil
}

// pushEvent records a push and asks every binding it concerns for a sync.
func (s *Server) pushEvent(ctx context.Context, delivery string, body []byte, push source.PushEvent) (int, string, error) {
	org, answer, err := s.receiveDelivery(ctx, push.InstallationID, delivery, body)
	if err != nil {
		return 0, "", err
	}
	if a, ok := answer.Get(); ok {
		return a.status, a.outcome, nil
	}
	if push.Deleted {
		return http.StatusAccepted, "branchDeleted", nil
	}
	t, err := s.deps.Store.Tenant(ctx, org)
	if err != nil {
		return 0, "", err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	bindings, err := t.BindingsForPush(ctx, push.InstallationID, push.Repository, push.Branch)
	if err != nil {
		return 0, "", err //nolint:wrapcheck // a store error, answered as internal
	}
	for _, binding := range bindings {
		audit := store.NewAudit{
			ActorKind:  "webhook",
			ActorID:    opt.Some("github:" + strconv.FormatUint(push.InstallationID, 10)),
			Action:     "syncSource",
			TargetKind: opt.Some("app"),
			TargetRef:  opt.Some(binding.Target.String()),
			Outcome:    "accepted",
			Data: opt.Some[any](map[string]any{
				"delivery":   delivery,
				"repository": push.Repository.String(),
				"branch":     push.Branch.String(),
				"after":      push.After.String(),
				"forced":     push.Forced,
			}),
		}
		if _, err := t.RequestSync(ctx, binding, "github:"+delivery, audit); err != nil {
			return 0, "", err //nolint:wrapcheck // a store error, answered as internal
		}
	}
	if err := t.Commit(ctx); err != nil {
		return 0, "", err //nolint:wrapcheck // a store error, answered as internal
	}
	if len(bindings) == 0 {
		return http.StatusAccepted, "noBinding", nil
	}
	return http.StatusAccepted, "syncing", nil
}
