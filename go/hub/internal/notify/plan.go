package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/config"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/integrations/outbound"
)

// The notifier consumes the outbox: every accepted and settled deployment
// and build becomes an event. A failed deployment or build opens an
// incident (a repeat counts on the open one) and a success resolves it.
// Events go to the organization's subscribed webhook endpoints; incident
// events are held back while a silence covers them. Builds and deployments
// of Git sources report a commit status.
//
// Deliveries are signed: `Kuben-Signature: t=<unix seconds>,v1=<hex
// HMAC-SHA256 of "<t>.<body>">` with the endpoint's secret. A delivery is
// retried with backoff and given up after [MaxAttempts] or a day; an
// endpoint that keeps failing is disabled. Messages older than
// [StaleEvent] are not delivered, so a long outage never ends in a storm
// of stale events. Private, loopback and link-local targets are refused
// unless `notify.allow_private_targets` is set.

const (
	// MaxAttempts is the number of attempts of one delivery before it is
	// given up.
	MaxAttempts int32 = 12
	// MaxDeliveryAgeMs is the age after which a delivery is given up.
	MaxDeliveryAgeMs int64 = 24 * 3_600_000
	// StaleEvent is the age after which an outbox message is no longer
	// turned into deliveries.
	StaleEvent = time.Hour
	// Runbooks is where the runbooks live (M4.12).
	Runbooks = "https://kuben.teamtem.com/docs/operations/runbooks/"
)

// Runbook is the runbook section for incidents of kind, if there is one
// (routes/incidents.rs runbook).
func Runbook(kind string) (string, bool) {
	var anchor string
	switch kind {
	case "deployment.failed":
		anchor = "a-deployment-failed"
	case "build.failed":
		anchor = "a-build-failed"
	case "backup.stale":
		anchor = "backups-are-stale"
	default:
		return "", false
	}
	return Runbooks + "#" + anchor, true
}

// Plan is what an outbox message means.
type Plan struct {
	// Event is the webhook event type.
	Event string
	Build bool
	Phase opt.Val[string]
	Code  opt.Val[string]
	// Incident opens (true) or resolves (false) the incident.
	Incident opt.Val[bool]
	// Status is the commit status to report.
	Status opt.Val[string]
}

// stringMember is payload's member key when payload is an object and the
// member a string.
func stringMember(payload any, key string) opt.Val[string] {
	if o, ok := payload.(map[string]any); ok {
		if s, ok := o[key].(string); ok {
			return opt.Some(s)
		}
	}
	return opt.None[string]()
}

// PlanOf is the plan of an outbox topic with payload; false for topics
// nobody is told about.
func PlanOf(topic string, payload any) (Plan, bool) {
	phase, code := stringMember(payload, "phase"), stringMember(payload, "code")
	base := func(event string, build bool, incident opt.Val[bool], status string) (Plan, bool) {
		return Plan{Event: event, Build: build, Phase: phase, Code: code, Incident: incident, Status: opt.Some(status)}, true
	}
	open, resolve, none := opt.Some(true), opt.Some(false), opt.None[bool]()
	p, _ := phase.Get() // an absent phase matches none below
	switch {
	case topic == "deployment.accepted":
		return base("deployment.started", false, none, "pending")
	case topic == "deployment.settled" && p == "succeeded":
		return base("deployment.succeeded", false, resolve, "success")
	case topic == "deployment.settled" && (p == "failed" || p == "recoveryFailed" || p == "manualActionRequired"):
		return base("deployment.failed", false, open, "failure")
	case topic == "deployment.settled" && p == "cancelled":
		return base("deployment.cancelled", false, none, "error")
	case topic == "build.queued":
		return base("build.started", true, none, "pending")
	case topic == "build.settled" && p == "succeeded":
		return base("build.succeeded", true, resolve, "success")
	case topic == "build.settled" && p == "failed":
		return base("build.failed", true, open, "failure")
	case topic == "build.settled" && p == "cancelled":
		return base("build.cancelled", true, none, "error")
	}
	return Plan{}, false
}

// Signature is the `Kuben-Signature` header of body sent at unixSeconds.
func Signature(secret []byte, unixSeconds int64, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	t := strconv.FormatInt(unixSeconds, 10)
	mac.Write([]byte(t + ".")) //nolint:errcheck,gosec // a hash never fails to write
	mac.Write(body)            //nolint:errcheck,gosec // a hash never fails to write
	return "t=" + t + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// RetryAt is when a delivery that failed its attempts-th try is tried
// again; false to give it up.
func RetryAt(attempts int32, createdAt, now int64) (int64, bool) {
	if attempts >= MaxAttempts || now-createdAt > MaxDeliveryAgeMs {
		return 0, false
	}
	backoff := min(int64(30_000)<<min(max(attempts, 0), 10), 6*3_600_000)
	return now + backoff, true
}

// CheckTarget is why rawURL may not receive webhooks under cfg, if it may
// not: the error's text is the reason.
func CheckTarget(ctx context.Context, rawURL string, cfg config.NotifyCfg) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return errors.New("not a URL")
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && cfg.AllowHTTP:
	default:
		return errors.New("only https:// endpoints are allowed")
	}
	name := u.Hostname()
	if name == "" {
		return errors.New("no host")
	}
	if cfg.AllowPrivateTargets {
		return nil
	}
	host := name
	if strings.Contains(name, ":") {
		host = "[" + name + "]" // as http::Uri shows an IPv6 host
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", name)
	if err != nil {
		return fmt.Errorf("cannot resolve %s: %w", host, err)
	}
	for _, addr := range addrs {
		if outbound.IsPrivate(addr) {
			return fmt.Errorf("%s resolves to a private address (%s)", host, addr)
		}
	}
	return nil
}
