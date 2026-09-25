package api

// The image update watcher (M5.4, image_watch.rs): apps that follow a tag
// pattern of their repository get a new release when the pattern's tag
// moves.
//
// Every replica runs one. A pass:
//  1. Leases the due policies (their next check moves past the pass), so
//     replicas never check the same app twice.
//  2. For each app, lists the repository's tags (unless the policy follows
//     one fixed tag) with the environment's registry login, and resolves the
//     chosen tag to a digest.
//  3. When the digest is new, deploys it with the app's newest
//     configuration. A protected environment's run waits for approval, like
//     any other.
//  4. Records the outcome. A failure backs off exponentially, up to a day,
//     and a registry's `Retry-After` is honored.

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/artifact"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/imagepolicy"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/integrations/oci"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/health"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/secrets"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
)

// ImageWatchSubsystem is the watcher's health name.
const ImageWatchSubsystem = "image-policies"

const (
	imageWatchTick  = 30 * time.Second
	imageWatchBatch = 10
	// imageWatchLeaseMs is how long a leased policy is hidden from other
	// replicas.
	imageWatchLeaseMs = 10 * 60_000
	// defaultCheckInterval stands in for a stored interval that does not fit.
	defaultCheckInterval = 300
	rateLimitedWhy       = "the registry limits requests"
)

// Found is what checking one policy found.
//
//sumtype:decl
type Found interface{ found() }

type (
	// FoundCurrent means the app runs the pattern's digest already.
	FoundCurrent struct{ Tag, Digest string }
	// FoundDeployed means a run of the new digest was started.
	FoundDeployed struct {
		Tag, Digest string
		Run         ids.DeploymentRunID
	}
	// FoundRefused means the run was not started, for Why.
	FoundRefused struct{ Tag, Digest, Why string }
	// FoundFailed means the check failed; RetryAfter is the registry's wish,
	// in seconds.
	FoundFailed struct {
		Why        string
		RetryAfter opt.Val[uint64]
	}
)

func (FoundCurrent) found()  {}
func (FoundDeployed) found() {}
func (FoundRefused) found()  {}
func (FoundFailed) found()   {}

// ImageWatcher is the watcher of one process.
type ImageWatcher struct {
	store   *store.Store
	images  oci.Resolver
	keyring opt.Val[*secrets.Keyring]
	clock   clock.Clock
	logger  *slog.Logger
}

// NewImageWatcher is a watcher resolving images with images and opening
// registry logins with keyring, when there is one.
func NewImageWatcher(st *store.Store, images oci.Resolver, keyring opt.Val[*secrets.Keyring], c clock.Clock, logger *slog.Logger) *ImageWatcher {
	return &ImageWatcher{store: st, images: images, keyring: keyring, clock: c, logger: logger}
}

// Pass checks every due policy once; the number checked.
func (w *ImageWatcher) Pass(ctx context.Context) (int, error) {
	orgs, err := w.store.ImagePolicyOrgs(ctx)
	if err != nil {
		return 0, err //nolint:wrapcheck // a store error naming its operation
	}
	checked := 0
	for _, org := range orgs {
		leased, err := w.lease(ctx, org)
		if err != nil {
			return checked, err
		}
		for _, policy := range leased {
			if err := w.record(ctx, org, policy, w.check(ctx, org, policy)); err != nil {
				return checked, err
			}
			checked++
		}
	}
	return checked, nil
}

// lease takes org's due policies and moves their next check past the pass.
func (w *ImageWatcher) lease(ctx context.Context, org ids.OrgID) ([]store.ImagePolicy, error) {
	now := w.clock.NowMs()
	t, err := w.store.Tenant(ctx, org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error naming its operation
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	due, err := t.DueImagePolicies(ctx, now, imageWatchBatch)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error naming its operation
	}
	for _, p := range due {
		lease := store.PolicyCheck{NextCheckAt: now + imageWatchLeaseMs, Failures: nonNegative(p.Failures, 0), Error: p.LastError}
		if err := t.ImagePolicyChecked(ctx, p.Target, lease); err != nil {
			return nil, err //nolint:wrapcheck // a store error naming its operation
		}
	}
	return due, t.Commit(ctx) //nolint:wrapcheck // a store error naming its operation
}

// nonNegative is v as unsigned, or fallback when it is negative.
func nonNegative(v int32, fallback uint32) uint32 {
	if v < 0 {
		return fallback
	}
	return uint32(v)
}

func failed(err error) Found { return FoundFailed{Why: err.Error()} }

// rateLimited is the failure of a registry that asked to slow down.
func rateLimited(err error) (Found, bool) {
	var limited oci.RateLimited
	if errors.As(err, &limited) {
		return FoundFailed{Why: rateLimitedWhy, RetryAfter: opt.Some(limited.RetryAfter)}, true
	}
	return nil, false
}

func (w *ImageWatcher) check(ctx context.Context, org ids.OrgID, policy store.ImagePolicy) Found {
	pattern, err := imagepolicy.Parse(policy.Pattern)
	if err != nil {
		return failed(err)
	}
	reference, err := oci.Parse(policy.Repository)
	if err != nil {
		return failed(err)
	}
	login, err := w.login(ctx, org, policy.Environment, reference.Registry)
	if err != nil {
		return failed(err)
	}
	tags := []string{}
	if pattern.NeedsListing() {
		if tags, err = w.images.ListTags(ctx, policy.Repository, login); err != nil {
			if f, ok := rateLimited(err); ok {
				return f
			}
			return failed(err)
		}
	}
	tag, ok := pattern.Select(tags).Get()
	if !ok {
		return FoundFailed{Why: "no tag of " + policy.Repository + " matches " + policy.Pattern}
	}
	image := policy.Repository + ":" + tag
	resolved, err := w.images.ResolveAs(ctx, image, login)
	if err != nil {
		if f, ok := rateLimited(err); ok {
			return f
		}
		return failed(err)
	}
	return w.deploy(ctx, org, policy, tag, resolved.Digest, image)
}

// login is the environment's login for registry, opened; none without a
// keyring or a login.
func (w *ImageWatcher) login(ctx context.Context, org ids.OrgID, env ids.EnvironmentID, registry string) (opt.Val[oci.Login], error) {
	t, err := w.store.Tenant(ctx, org)
	if err != nil {
		return opt.None[oci.Login](), err //nolint:wrapcheck // the store's text, as Rust's to_string
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	keyring, ok := w.keyring.Get()
	if !ok {
		return opt.None[oci.Login](), nil
	}
	login, err := openRegistryLogin(ctx, keyring, t, org, env, registry)
	if err != nil {
		return opt.None[oci.Login](), err
	}
	if l, ok := login.Get(); ok {
		return opt.Some(oci.Login{Username: l.Username, Password: l.Password}), nil
	}
	return opt.None[oci.Login](), nil
}

func (w *ImageWatcher) deploy(ctx context.Context, org ids.OrgID, policy store.ImagePolicy, tag string, digest artifact.Digest, image string) Found {
	text := digest.String()
	started, current, err := w.startFollowed(ctx, org, policy, digest, image)
	switch {
	case err != nil:
		return failed(err)
	case current:
		return FoundCurrent{Tag: tag, Digest: text}
	}
	if accepted, ok := started.(store.StartedAccepted); ok {
		return FoundDeployed{Tag: tag, Digest: text, Run: accepted.Run}
	}
	return FoundRefused{Tag: tag, Digest: text, Why: refusal(started)}
}

// startFollowed deploys digest unless the app runs it already (current).
func (w *ImageWatcher) startFollowed(ctx context.Context, org ids.OrgID, policy store.ImagePolicy, digest artifact.Digest, image string) (store.Started, bool, error) {
	t, err := w.store.Tenant(ctx, org)
	if err != nil {
		return nil, false, err //nolint:wrapcheck // the store's text, as Rust's to_string
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	running, found, err := t.CurrentDigest(ctx, policy.Target)
	if err != nil {
		return nil, false, err //nolint:wrapcheck // the store's text, as Rust's to_string
	}
	if found && running == digest.String() {
		return nil, true, nil
	}
	started, err := t.DeployFollowedImage(ctx, policy, digest, image)
	if err != nil {
		return nil, false, err //nolint:wrapcheck // the store's text, as Rust's to_string
	}
	return started, false, t.Commit(ctx) //nolint:wrapcheck // the store's text, as Rust's to_string
}

func (w *ImageWatcher) record(ctx context.Context, org ids.OrgID, policy store.ImagePolicy, found Found) error {
	now := w.clock.NowMs()
	interval := nonNegative(policy.IntervalSecs, defaultCheckInterval)
	failures := nonNegative(policy.Failures, 0)
	var c store.PolicyCheck
	switch f := found.(type) {
	case FoundCurrent:
		c = store.PolicyCheck{
			NextCheckAt: imagepolicy.NextCheck(now, interval, 0, opt.None[uint64]()),
			Tag:         opt.Some(f.Tag), Digest: opt.Some(f.Digest),
		}
	case FoundDeployed:
		w.logger.Info("a followed image was deployed",
			"target", policy.Target.String(), "tag", f.Tag, "digest", f.Digest, "run", f.Run.String())
		c = store.PolicyCheck{
			NextCheckAt: imagepolicy.NextCheck(now, interval, 0, opt.None[uint64]()),
			Tag:         opt.Some(f.Tag), Digest: opt.Some(f.Digest), Run: opt.Some(f.Run),
		}
	case FoundRefused:
		c = store.PolicyCheck{
			NextCheckAt: imagepolicy.NextCheck(now, interval, failures+1, opt.None[uint64]()),
			Failures:    failures + 1, Error: opt.Some(f.Why), Tag: opt.Some(f.Tag), Digest: opt.Some(f.Digest),
		}
	case FoundFailed:
		c = store.PolicyCheck{
			NextCheckAt: imagepolicy.NextCheck(now, interval, failures+1, f.RetryAfter),
			Failures:    failures + 1, Error: opt.Some(f.Why),
		}
	}
	t, err := w.store.Tenant(ctx, org)
	if err != nil {
		return err //nolint:wrapcheck // a store error naming its operation
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	if err := t.ImagePolicyChecked(ctx, policy.Target, c); err != nil {
		return err //nolint:wrapcheck // a store error naming its operation
	}
	return t.Commit(ctx) //nolint:wrapcheck // a store error naming its operation
}

// refusal is why started is not a run of the followed image.
func refusal(started store.Started) string {
	switch s := started.(type) {
	case store.StartedRejected:
		return s.Reject.Error()
	case store.StartedSecretRevoked:
		return "a secret the app uses is revoked"
	case store.StartedVulnerabilityBlocked:
		return "the environment's vulnerability gate refuses the image"
	case store.StartedFrozen:
		return "the environment is frozen"
	case store.StartedUntrusted:
		return "an untrusted preview cannot use secrets"
	case store.StartedNotFound:
		return "the app has no configuration yet"
	case store.StartedAccepted, store.StartedReplayed, store.StartedKeyReused:
		return "the run was accepted before"
	}
	return "the run was accepted before"
}

// RunImageWatch runs w until ctx ends: a pass every 30 seconds.
func RunImageWatch(ctx context.Context, w *ImageWatcher, h *health.Health) error {
	h.OK(ImageWatchSubsystem)
	tick := time.NewTicker(imageWatchTick)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		if _, err := w.Pass(ctx); err != nil {
			return err
		}
	}
}
