package notify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/source"
	"github.com/Teamtem-dev/kuben/internal/health"
	"github.com/Teamtem-dev/kuben/internal/integrations/github"
	"github.com/Teamtem-dev/kuben/internal/integrations/outbound"
	wire "github.com/Teamtem-dev/kuben/internal/jsonx"
	secrets "github.com/Teamtem-dev/kuben/internal/keyring"
	"github.com/Teamtem-dev/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/internal/version"
)

const (
	// Subsystem names the notifier in health and logs.
	Subsystem = "notifications"
	// batch is the number of outbox messages or deliveries handled per round.
	batch = 50
	idle  = 2 * time.Second
	// timeout bounds the wait for a receiver's answer.
	timeout = 10 * time.Second
	// leaseMs hides a delivery from other workers while it is in flight.
	leaseMs = 60_000
	// system is who resolves the incidents the notifier resolves.
	system = "system:notifier"
)

// Deps are what a Notifier works with.
type Deps struct {
	Store   *store.Store
	Keyring *secrets.Keyring
	// GitHub reports commit statuses, when Git sources are configured.
	GitHub opt.Val[*github.App]
	Config config.NotifyCfg
	// PublicURL is the console's address, for links in payloads.
	PublicURL opt.Val[string]
	Clock     clock.Clock
	Logger    *slog.Logger
}

// Notifier turns the outbox into incidents, webhook deliveries and commit
// statuses, and sends the deliveries. Every replica runs one; the store's
// claims keep them apart.
type Notifier struct {
	deps   Deps
	client *outbound.Client
}

// New is the notifier of deps.
func New(deps Deps) *Notifier {
	if deps.Clock == nil {
		deps.Clock = clock.System{}
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if u, ok := deps.PublicURL.Get(); ok {
		deps.PublicURL = opt.Some(strings.TrimRight(u, "/"))
	}
	return &Notifier{deps: deps, client: outbound.New(deps.Config.AllowHTTP, timeout)}
}

// Run runs n until ctx ends; an error of the store ends it (the supervisor
// starts it again).
func Run(ctx context.Context, n *Notifier, h *health.Health) error {
	h.OK(Subsystem)
	for ctx.Err() == nil {
		consumed, err := n.Consume(ctx)
		if err != nil {
			return err
		}
		delivered, err := n.Deliver(ctx)
		if err != nil {
			return err
		}
		if consumed+delivered == 0 {
			wait := time.NewTimer(idle)
			select {
			case <-ctx.Done():
			case <-wait.C:
			}
			wait.Stop()
		}
	}
	return nil
}

// Consume turns pending outbox messages into incidents, deliveries and
// commit statuses. It returns the number handled.
func (n *Notifier) Consume(ctx context.Context) (int, error) {
	messages, err := n.deps.Store.TakeOutbox(ctx, batch, time.Minute)
	if err != nil {
		return 0, fmt.Errorf("notifications: %w", err)
	}
	staleBefore := n.deps.Clock.NowMs() - StaleEvent.Milliseconds()
	for _, m := range messages {
		if plan, ok := PlanOf(m.Topic, m.Payload); ok && m.CreatedAt >= staleBefore {
			if err := n.handle(ctx, m, plan); err != nil {
				n.deps.Logger.Warn("an event is retried", "message", m.ID, "topic", m.Topic, "error", err)
				continue
			}
		}
		if _, err := n.deps.Store.OutboxDelivered(ctx, m.ID); err != nil {
			return 0, fmt.Errorf("notifications: %w", err)
		}
	}
	return len(messages), nil
}

func (n *Notifier) handle(ctx context.Context, m store.OutboxMessage, plan Plan) error {
	operation, ok := m.Operation.Get()
	if !ok {
		return nil
	}
	t, err := n.deps.Store.Tenant(ctx, m.Org)
	if err != nil {
		return err //nolint:wrapcheck // the store's error, logged
	}
	defer t.Rollback(ctx) //nolint:errcheck // committed on success
	c, found, err := t.OperationContext(ctx, operation, plan.Build)
	if err != nil || !found {
		return err //nolint:wrapcheck // the store's error, logged
	}
	environment, target := ids.From[ids.Environment](c.EnvironmentID), ids.From[ids.Target](c.TargetID)
	silenced, err := t.Silenced(ctx, environment, opt.Some(target), n.deps.Clock.NowMs())
	if err != nil {
		return err //nolint:wrapcheck // the store's error, logged
	}
	payload := n.payload(m, plan, c)
	if _, err := t.EnqueueEvent(ctx, m.ID, plan.Event, payload); err != nil {
		return err //nolint:wrapcheck // the store's error, logged
	}
	if open, ok := plan.Incident.Get(); ok {
		if err := n.incident(ctx, t, plan, c, open, silenced, payload["subject"]); err != nil {
			return err
		}
	}
	if err := t.Commit(ctx); err != nil {
		return err //nolint:wrapcheck // the store's error, logged
	}
	if state, ok := plan.Status.Get(); ok {
		n.reportStatus(ctx, c, plan, state)
	}
	return nil
}

// incident opens (open) or resolves the incident of the operation's
// target, and unless silenced tells the endpoints when that changed it.
func (n *Notifier) incident(
	ctx context.Context, t *store.Tenant, plan Plan, c store.OperationContext, open, silenced bool, subject any,
) error {
	what, title := "deployment", "The deployment"
	severity := "critical"
	if plan.Build {
		what, title, severity = "build", "The build", "warning"
	}
	key := what + ":" + c.TargetID.String()
	var (
		id      uuid.UUID
		event   string
		changed bool
		err     error
	)
	if open {
		id, changed, err = t.OpenIncident(ctx, store.NewIncident{
			Project:     opt.Some(ids.From[ids.Project](c.ProjectID)),
			Environment: opt.Some(ids.From[ids.Environment](c.EnvironmentID)),
			Target:      opt.Some(ids.From[ids.Target](c.TargetID)),
			Kind:        plan.Event,
			Severity:    severity,
			DedupeKey:   key,
			Title:       fmt.Sprintf("%s of %s/%s/%s failed", title, c.Project, c.Environment, c.App),
			Detail:      plan.Code,
		})
		event = "incident.opened"
	} else {
		id, changed, err = t.ResolveIncidentKey(ctx, key, system)
		event = "incident.resolved"
	}
	if err != nil || !changed || silenced {
		return err //nolint:wrapcheck // the store's error, logged
	}
	runbook := opt.None[string]()
	if r, found := Runbook(plan.Event); found {
		runbook = opt.Some(r)
	}
	body := map[string]any{
		"incident": id,
		"event":    plan.Event,
		"subject":  subject,
		"runbook":  runbook,
	}
	_, err = t.EnqueueEvent(ctx, uuid.Must(uuid.NewV7()), event, body)
	return err //nolint:wrapcheck // the store's error, logged
}

func (n *Notifier) appURL(c store.OperationContext) opt.Val[string] {
	if base, ok := n.deps.PublicURL.Get(); ok {
		return opt.Some(fmt.Sprintf("%s/projects/%s/%s/%s", base, c.Project, c.Environment, c.App))
	}
	return opt.None[string]()
}

// sourceOf is the operation's source document, null when it has none or
// it is not JSON.
func sourceOf(c store.OperationContext) map[string]any {
	text, ok := c.Source.Get()
	if !ok {
		return nil
	}
	v, err := wire.DecodeAny([]byte(text))
	if err != nil {
		return nil
	}
	if o, ok := v.(map[string]any); ok {
		return o
	}
	return nil
}

func (n *Notifier) payload(m store.OutboxMessage, plan Plan, c store.OperationContext) map[string]any {
	src := sourceOf(c)
	return map[string]any{
		"id":         m.ID,
		"type":       plan.Event,
		"created_at": n.deps.Clock.NowMs(),
		"subject": map[string]any{
			"project":     c.Project,
			"environment": c.Environment,
			"app":         c.App,
			"url":         n.appURL(c),
		},
		"operation":  m.Operation,
		"phase":      plan.Phase,
		"code":       plan.Code,
		"reason":     c.Reason,
		"generation": c.Generation,
		"repository": src["repository"],
		"commit":     src["commit"],
	}
}

// reportStatus reports a commit status for a Git-sourced build or
// deployment; a failure is logged, never retried (the next event reports
// again).
func (n *Notifier) reportStatus(ctx context.Context, c store.OperationContext, plan Plan, state string) {
	app, okApp := n.deps.GitHub.Get()
	installation, okInstallation := c.InstallationID.Get()
	src := sourceOf(c)
	if !okApp || !okInstallation || c.Source.IsNone() || installation < 0 {
		return
	}
	name, okName := src["repository"].(string)
	commit, okCommit := src["commit"].(string)
	if !okName || !okCommit {
		return
	}
	repository, err := source.ParseRepoName(name)
	if err != nil {
		return
	}
	check := "kuben/" + c.Environment
	if plan.Build {
		check = "kuben/build"
	}
	status := github.CommitStatus{
		State:       state,
		Context:     check,
		Description: strings.TrimSpace(plan.Event + " " + plan.Code.Or("")),
		TargetURL:   n.appURL(c),
	}
	if err := app.CommitStatus(ctx, uint64(installation), repository, commit, status); err != nil {
		n.deps.Logger.Warn("a commit status was not reported", "repository", repository.String(), "commit", commit, "error", err)
	}
}

// Deliver sends the due deliveries and returns the number sent.
func (n *Notifier) Deliver(ctx context.Context) (int, error) {
	due, err := n.deps.Store.TakeDeliveries(ctx, batch, leaseMs)
	if err != nil {
		return 0, fmt.Errorf("notifications: %w", err)
	}
	for _, d := range due {
		code, err := n.send(ctx, d)
		if err == nil {
			err = n.deps.Store.DeliverySucceeded(ctx, d.ID, code)
		} else {
			var answered statusError
			status := opt.None[int32]()
			if errors.As(err, &answered) {
				status = opt.Some(answered.code)
			}
			again := opt.None[int64]()
			if at, ok := RetryAt(d.Attempts, d.CreatedAt, n.deps.Clock.NowMs()); ok {
				again = opt.Some(at)
			}
			err = n.deps.Store.DeliveryFailed(ctx, d.ID, status, err.Error(), again)
		}
		if err != nil {
			return 0, fmt.Errorf("notifications: %w", err)
		}
	}
	return len(due), nil
}

// statusError is a receiver's answer other than 2xx.
type statusError struct{ code int32 }

// Error is Rust's `HTTP {status}`: the code and its canonical reason, as
// the http crate's StatusCode displays it.
func (e statusError) Error() string {
	return fmt.Sprintf("HTTP %d %s", e.code, CanonicalReason(int(e.code)))
}

// CanonicalReason is the reason phrase the Rust http crate (1.x,
// status.rs) gives code, `<unknown status code>` for codes it does not
// name. Go's http.StatusText words some differently (203, 413, 414, 416),
// and the text is stored as a delivery's last error.
func CanonicalReason(code int) string {
	if reason, ok := rustReasons()[code]; ok {
		return reason
	}
	return "<unknown status code>"
}

// rustReasons are the http crate's reason phrases by status code.
func rustReasons() map[int]string {
	return map[int]string{
		100: "Continue", 101: "Switching Protocols", 102: "Processing", 103: "Early Hints",
		200: "OK", 201: "Created", 202: "Accepted", 203: "Non Authoritative Information", 204: "No Content",
		205: "Reset Content", 206: "Partial Content", 207: "Multi-Status", 208: "Already Reported", 226: "IM Used",
		300: "Multiple Choices", 301: "Moved Permanently", 302: "Found", 303: "See Other", 304: "Not Modified",
		305: "Use Proxy", 307: "Temporary Redirect", 308: "Permanent Redirect",
		400: "Bad Request", 401: "Unauthorized", 402: "Payment Required", 403: "Forbidden", 404: "Not Found",
		405: "Method Not Allowed", 406: "Not Acceptable", 407: "Proxy Authentication Required",
		408: "Request Timeout", 409: "Conflict", 410: "Gone", 411: "Length Required", 412: "Precondition Failed",
		413: "Payload Too Large", 414: "URI Too Long", 415: "Unsupported Media Type", 416: "Range Not Satisfiable",
		417: "Expectation Failed", 418: "I'm a teapot",
		421: "Misdirected Request", 422: "Unprocessable Entity", 423: "Locked", 424: "Failed Dependency",
		425: "Too Early", 426: "Upgrade Required", 428: "Precondition Required", 429: "Too Many Requests",
		431: "Request Header Fields Too Large", 451: "Unavailable For Legal Reasons",
		500: "Internal Server Error", 501: "Not Implemented", 502: "Bad Gateway", 503: "Service Unavailable",
		504: "Gateway Timeout", 505: "HTTP Version Not Supported", 506: "Variant Also Negotiates",
		507: "Insufficient Storage", 508: "Loop Detected", 510: "Not Extended",
		511: "Network Authentication Required",
	}
}

// endpointOf is the URL and opened secret of d's endpoint, if it may still
// be delivered to.
func (n *Notifier) endpointOf(ctx context.Context, d store.WebhookDelivery) (string, []byte, error) {
	t, err := n.deps.Store.Tenant(ctx, d.Org)
	if err != nil {
		return "", nil, err //nolint:wrapcheck // its text is the delivery's error
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	url, sealed, found, err := t.EndpointSecret(ctx, d.Endpoint)
	if err != nil {
		return "", nil, err //nolint:wrapcheck // its text is the delivery's error
	}
	if !found {
		return "", nil, errors.New("the endpoint is disabled")
	}
	if err := CheckTarget(ctx, url, n.deps.Config); err != nil {
		return "", nil, err
	}
	secret, err := OpenSecret(n.deps.Keyring, d.Org, d.Endpoint, sealed)
	return url, secret, err
}

func (n *Notifier) send(ctx context.Context, d store.WebhookDelivery) (int32, error) {
	url, secret, err := n.endpointOf(ctx, d)
	if err != nil {
		return 0, err
	}
	text, err := wire.CanonicalValue(d.Payload)
	if err != nil {
		return 0, err //nolint:wrapcheck // its text is the delivery's error
	}
	body := []byte(text)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err //nolint:wrapcheck // its text is the delivery's error
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "kuben/"+version.Version)
	// Kuben's own headers go out in lower case, as hyper sent them.
	req.Header["kuben-event"] = []string{d.Event}
	req.Header["kuben-delivery"] = []string{d.EventID.String()}
	req.Header["kuben-signature"] = []string{Signature(secret, n.deps.Clock.NowMs()/1000, body)}
	code, err := n.client.Send(ctx, req)
	if err != nil {
		return 0, err //nolint:wrapcheck // its text is the delivery's error
	}
	if code < 200 || code > 299 {
		return 0, statusError{code: int32(code)} //nolint:gosec // an HTTP status fits
	}
	return int32(code), nil //nolint:gosec // an HTTP status fits
}
