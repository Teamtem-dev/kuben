package httpapi

// Push webhooks of Git connections (2.1):
//
//	POST /api/v1/webhooks/gitlab/{connection}   X-Gitlab-Token = the connection's secret
//	POST /api/v1/webhooks/gitea/{connection}    X-Gitea-Signature / X-Forgejo-Signature =
//	                                            hex HMAC-SHA256 of the body with the secret
//	POST /api/v1/webhooks/github/{connection}   X-Hub-Signature-256 = sha256=<hex HMAC-SHA256>
//	                                            (a token connection; the GitHub App's is
//	                                            /api/v1/webhooks/github)
//
// Like GitHub's, they are served outside the session and CSRF layers and
// trust only the connection's secret, compared in constant time. A push is
// a hint: it is recorded once per delivery id in the inbox and turned into
// `source.sync` operations for the bindings that read the pushed branch
// through the connection; the sync reads the real head from the provider
// before anything is built.

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/gitprovider"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/gitprovider/connect"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/problem"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/integrations/gitea"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/integrations/github"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/integrations/gitlab"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

const (
	// gitlabWebhookPrefix and giteaWebhookPrefix are followed by the
	// connection's id.
	gitlabWebhookPrefix = "/api/v1/webhooks/gitlab/"
	giteaWebhookPrefix  = "/api/v1/webhooks/gitea/"
	// githubWebhookPrefix is a GitHub token connection's; githubWebhookPath
	// (no trailing slash) stays the GitHub App's.
	githubWebhookPrefix = githubWebhookPath + "/"
	// connectionWebhookBodyLimit is the largest delivery accepted.
	connectionWebhookBodyLimit = githubWebhookBodyLimit
)

// hookedConnection is the connection a delivery names, with its opened
// webhook secret.
type hookedConnection struct {
	org    ids.OrgID
	conn   store.GitConnection
	secret []byte
}

// delivery is a verified push delivery of a connection.
type delivery struct {
	id   string
	push opt.Val[gitprovider.Push]
}

// hookReader verifies and reads one provider's delivery; it answers itself
// (and returns false) when the delivery is refused, or needs no more than
// an answer (GitHub's ping).
type hookReader func(w http.ResponseWriter, r *http.Request, body []byte, hook hookedConnection) (delivery, bool)

// gitlabWebhook is `POST /api/v1/webhooks/gitlab/{connection}`.
func (s *Server) gitlabWebhook() http.Handler {
	return s.connectionWebhook(store.GitProviderGitLab, s.readGitlab)
}

// giteaWebhook is `POST /api/v1/webhooks/gitea/{connection}`.
func (s *Server) giteaWebhook() http.Handler {
	return s.connectionWebhook(store.GitProviderGitea, s.readGitea)
}

// githubConnectionWebhook is `POST /api/v1/webhooks/github/{connection}`.
func (s *Server) githubConnectionWebhook() http.Handler {
	return s.connectionWebhook(store.GitProviderGitHub, s.readGithub)
}

// connectionWebhook serves the push webhook of provider's connections,
// with its own body limit; only POST is routed to it.
func (s *Server) connectionWebhook(provider store.GitProvider, read hookReader) http.Handler {
	deliver := limitBody(connectionWebhookBodyLimit, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.serveConnectionWebhook(w, r, provider, read)
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		deliver.ServeHTTP(w, r)
	})
}

func (s *Server) serveConnectionWebhook(w http.ResponseWriter, r *http.Request, provider store.GitProvider, read hookReader) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	hook, answer, err := s.hookedConnection(r.Context(), provider, r.PathValue("connection"))
	if err != nil {
		s.deps.Logger.Error("a Git connection webhook could not be read", "provider", string(provider), "error", err)
		problem.Write(w, s.deps.Logger, err)
		return
	}
	if a, ok := answer.Get(); ok {
		answerWebhook(w, a.status, a.outcome)
		return
	}
	d, ok := read(w, r, body, hook)
	if !ok {
		return
	}
	push, isPush := d.push.Get()
	if !isPush {
		answerWebhook(w, http.StatusAccepted, "ignored")
		return
	}
	status, what, err := s.connectionPush(r.Context(), hook, d.id, body, push)
	if err != nil {
		s.deps.Logger.Error("a Git connection delivery could not be recorded", "provider", string(provider),
			"delivery", d.id, "error", err)
		problem.Write(w, s.deps.Logger, err)
		return
	}
	answerWebhook(w, status, what)
}

// hookedConnection is the connection text of provider with its webhook
// secret, or the answer for a delivery to no such connection.
func (s *Server) hookedConnection(
	ctx context.Context, provider store.GitProvider, text string,
) (hookedConnection, opt.Val[webhookAnswer], error) {
	unknown := opt.Some(webhookAnswer{http.StatusNotFound, "unknownConnection"})
	id, err := ids.Parse[ids.GitConnection](text)
	if err != nil {
		return hookedConnection{}, unknown, nil
	}
	ring, ok := s.deps.Keyring.Get()
	if !ok || ring == nil {
		return hookedConnection{}, opt.Some(webhookAnswer{http.StatusServiceUnavailable, "notConfigured"}), nil
	}
	org, found, err := s.deps.Store.GitConnectionOrg(ctx, id)
	if err != nil || !found {
		return hookedConnection{}, unknown, err //nolint:wrapcheck // a store error, answered as internal
	}
	t, err := s.deps.Store.Tenant(ctx, org)
	if err != nil {
		return hookedConnection{}, opt.None[webhookAnswer](), err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	conn, found, err := t.GitConnection(ctx, id)
	if err != nil || !found || conn.Provider != provider {
		return hookedConnection{}, unknown, err //nolint:wrapcheck // a store error, answered as internal
	}
	secret, err := connect.WebhookSecret(ring, conn)
	if err != nil {
		return hookedConnection{}, opt.None[webhookAnswer](), kerrors.New(kerrors.Internal, "%s", err.Error())
	}
	return hookedConnection{org: org, conn: conn, secret: secret}, opt.None[webhookAnswer](), nil
}

// deliveryID is the first of names present, or answers for a delivery
// without one.
func deliveryID(w http.ResponseWriter, h http.Header, names ...string) (string, bool) {
	for _, name := range names {
		if id, ok := headerValue(h, name); ok && id != "" {
			if len(id) > maxDeliveryID {
				answerWebhook(w, http.StatusBadRequest, "badDeliveryId")
				return "", false
			}
			return id, true
		}
	}
	answerWebhook(w, http.StatusBadRequest, "missingHeaders")
	return "", false
}

// firstHeader is the first of names present.
func firstHeader(h http.Header, names ...string) (string, bool) {
	for _, name := range names {
		if v, ok := headerValue(h, name); ok {
			return v, true
		}
	}
	return "", false
}

// readGitlab verifies X-Gitlab-Token and reads a GitLab delivery.
func (s *Server) readGitlab(w http.ResponseWriter, r *http.Request, body []byte, hook hookedConnection) (delivery, bool) {
	token, given := headerValue(r.Header, gitlab.TokenHeader)
	if !given || !gitlab.Verify(hook.secret, token) {
		s.deps.Logger.Warn("a GitLab webhook delivery with a missing or wrong token was refused",
			"connection", hook.conn.ID.String())
		answerWebhook(w, http.StatusUnauthorized, "badSignature")
		return delivery{}, false
	}
	event, hasEvent := headerValue(r.Header, gitlab.EventHeader)
	if !hasEvent {
		answerWebhook(w, http.StatusBadRequest, "missingHeaders")
		return delivery{}, false
	}
	id, ok := deliveryID(w, r.Header, gitlab.IdempotencyHeader, gitlab.EventUUIDHeader)
	if !ok {
		return delivery{}, false
	}
	push, err := gitlab.ParsePush(event, body)
	if err != nil {
		s.deps.Logger.Warn("a malformed GitLab delivery was refused", "delivery", id, "error", err)
		answerWebhook(w, http.StatusBadRequest, "malformed")
		return delivery{}, false
	}
	return delivery{id: id, push: push}, true
}

// readGitea verifies the HMAC signature and reads a Gitea or Forgejo
// delivery.
func (s *Server) readGitea(w http.ResponseWriter, r *http.Request, body []byte, hook hookedConnection) (delivery, bool) {
	signature, signed := firstHeader(r.Header, gitea.SignatureHeaders...)
	if !signed || !gitea.Verify(hook.secret, body, signature) {
		s.deps.Logger.Warn("a Gitea webhook delivery with a missing or wrong signature was refused",
			"connection", hook.conn.ID.String())
		answerWebhook(w, http.StatusUnauthorized, "badSignature")
		return delivery{}, false
	}
	event, hasEvent := firstHeader(r.Header, gitea.EventHeaders...)
	if !hasEvent {
		answerWebhook(w, http.StatusBadRequest, "missingHeaders")
		return delivery{}, false
	}
	id, ok := deliveryID(w, r.Header, gitea.DeliveryHeaders...)
	if !ok {
		return delivery{}, false
	}
	push, err := gitea.ParsePush(event, body)
	if err != nil {
		s.deps.Logger.Warn("a malformed Gitea delivery was refused", "delivery", id, "error", err)
		answerWebhook(w, http.StatusBadRequest, "malformed")
		return delivery{}, false
	}
	return delivery{id: id, push: push}, true
}

// readGithub verifies X-Hub-Signature-256 and reads a delivery of a GitHub
// repository or organization webhook; a ping is answered `pong`.
func (s *Server) readGithub(w http.ResponseWriter, r *http.Request, body []byte, hook hookedConnection) (delivery, bool) {
	signature, signed := headerValue(r.Header, github.SignatureHeader)
	if !signed || len(hook.secret) == 0 || !github.VerifySignature(hook.secret, body, signature) {
		s.deps.Logger.Warn("a GitHub webhook delivery with a missing or wrong signature was refused",
			"connection", hook.conn.ID.String())
		answerWebhook(w, http.StatusUnauthorized, "badSignature")
		return delivery{}, false
	}
	event, hasEvent := headerValue(r.Header, gitprovider.GitHubEventHeader)
	if !hasEvent {
		answerWebhook(w, http.StatusBadRequest, "missingHeaders")
		return delivery{}, false
	}
	id, ok := deliveryID(w, r.Header, gitprovider.GitHubDeliveryHeader)
	if !ok {
		return delivery{}, false
	}
	if event == gitprovider.GitHubPingEvent {
		answerWebhook(w, http.StatusOK, "pong")
		return delivery{}, false
	}
	push, err := gitprovider.ParseGitHubPush(event, body)
	if err != nil {
		s.deps.Logger.Warn("a malformed GitHub delivery was refused", "delivery", id, "error", err)
		answerWebhook(w, http.StatusBadRequest, "malformed")
		return delivery{}, false
	}
	return delivery{id: id, push: push}, true
}

// connectionPush records a push delivered to hook's connection and asks
// every binding it concerns for a sync.
func (s *Server) connectionPush(
	ctx context.Context, hook hookedConnection, id string, body []byte, push gitprovider.Push,
) (int, string, error) {
	provider := string(hook.conn.Provider)
	// Deliveries are recorded per connection: one event may reach several.
	received, err := s.deps.Store.Receive(ctx, hook.org, provider+":"+hook.conn.ID.String(), id, body)
	if err != nil {
		return 0, "", err //nolint:wrapcheck // a store error, answered as internal
	}
	switch received.(type) {
	case store.ReceivedNew:
	case store.ReceivedDuplicate:
		return http.StatusOK, "duplicate", nil
	case store.ReceivedChanged:
		return http.StatusConflict, "deliveryChanged", nil
	}
	if push.Deleted {
		return http.StatusAccepted, "branchDeleted", nil
	}
	t, err := s.deps.Store.Tenant(ctx, hook.org)
	if err != nil {
		return 0, "", err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	bindings, err := t.BindingsForConnectionPush(ctx, hook.conn.ID, push.Repository, push.Branch)
	if err != nil {
		return 0, "", err //nolint:wrapcheck // a store error, answered as internal
	}
	for _, binding := range bindings {
		audit := store.NewAudit{
			ActorKind:  "webhook",
			ActorID:    opt.Some(provider + ":" + hook.conn.ID.String()),
			Action:     "syncSource",
			TargetKind: opt.Some("app"),
			TargetRef:  opt.Some(binding.Target.String()),
			Outcome:    "accepted",
			Data: opt.Some[any](map[string]any{
				"delivery":   id,
				"connection": hook.conn.Name,
				"repository": push.Repository.String(),
				"branch":     push.Branch.String(),
				"after":      push.After,
			}),
		}
		if _, err := t.RequestSync(ctx, binding, provider+":"+id, audit); err != nil {
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
