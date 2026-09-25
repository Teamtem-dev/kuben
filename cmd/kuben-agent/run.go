package main

import (
	"context"
	"crypto/tls"
	"log/slog"
	"time"

	"golang.org/x/sync/errgroup"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/Teamtem-dev/kuben/internal/agent/bootstrap"
	"github.com/Teamtem-dev/kuben/internal/agent/clock"
	"github.com/Teamtem-dev/kuben/internal/agent/link"
	"github.com/Teamtem-dev/kuben/internal/agent/runtime"
	"github.com/Teamtem-dev/kuben/internal/agent/state"
	"github.com/Teamtem-dev/kuben/internal/agentlink/protocol"
)

// enroll is the agent's identity: the stored one, or a fresh enrollment.
// With a published enrollment it tries again until the hub issued a token
// that works; false when ctx ended first.
func enroll(ctx context.Context, a args, st state.State, key protocol.DeviceKey, t target, hubName string,
	logger *slog.Logger,
) (tls.Certificate, target, bool, error) {
	for {
		pinned, err := state.PinnedCA(t.hubCA)
		if err != nil {
			return tls.Certificate{}, t, false, err //nolint:wrapcheck // says which path
		}
		hub := state.HubAddress{Connector: link.TCPConnector{Address: t.hub}, Pinned: pinned, Name: hubName, Logger: logger}
		identity, err := state.EnsureIdentity(ctx, st, key, hub, t.cluster, t.readToken, clock.Now())
		switch {
		case err == nil:
			return identity, t, true, nil
		case !t.published:
			return tls.Certificate{}, t, false, err //nolint:wrapcheck // says what failed
		}
		// Published enrollment: the hub issues a fresh token when this one
		// is missing, spent or expired.
		logger.Warn("not enrolled yet", "error", err.Error(), "retry_in_s", int(enrollRetry/time.Second))
		wait := time.NewTimer(enrollRetry)
		select {
		case <-ctx.Done():
			wait.Stop()
			return tls.Certificate{}, t, false, nil
		case <-wait.C:
		}
		if again, ok := resolve(ctx, a, logger); ok {
			t = again
		}
	}
}

// linkedDeps is what the linked agent works with.
type linkedDeps struct {
	restCfg   *rest.Config
	client    dynamic.Interface
	secret    bootstrap.IdentitySecret
	hasSecret bool
	state     state.State
	key       protocol.DeviceKey
	target    target
	hubName   string
	identity  tls.Certificate
	logger    *slog.Logger
}

// linked keeps the enrolled agent linked until ctx ends.
func linked(ctx context.Context, a args, d linkedDeps) error {
	if d.hasSecret {
		if err := d.secret.Save(ctx, a.stateDir); err != nil {
			return err //nolint:wrapcheck // says what failed
		}
	}
	d.logger.Info("kuben-agent starting", "device", d.key.DeviceID(), "cluster", d.target.cluster, "hub", d.target.hub)
	pinned, err := state.PinnedCA(d.target.hubCA)
	if err != nil {
		return err //nolint:wrapcheck // says which path
	}
	var lifetime link.Lifetime
	stored, found, err := d.state.Certificate()
	if err != nil {
		return err //nolint:wrapcheck // says which path
	}
	if found {
		lifetime = link.Lifetime{NotBefore: stored.NotBefore, NotAfter: stored.NotAfter}
	}
	credentials, err := link.NewCredentials(pinned, d.hubName, d.identity, lifetime)
	if err != nil {
		return err //nolint:wrapcheck // says what failed
	}
	cfg := link.NewConfig(d.target.cluster, version, credentials, d.logger)
	if disco, err := discovery.NewDiscoveryClientForConfig(d.restCfg); err == nil {
		if v, err := disco.ServerVersion(); err == nil {
			cfg.KubernetesVersion = v.GitVersion
		}
	}
	cfg.Capabilities = protocol.NewFeatures(protocol.ApplicationRuntime)
	cfg.Executor = runtime.NewKubeExecutor(d.client, d.logger)
	g, ctx := errgroup.WithContext(ctx)
	// A renewed certificate goes into the Secret off the link's loop.
	saveSecret := make(chan struct{}, 1)
	if d.hasSecret {
		g.Go(func() error {
			for {
				select {
				case <-ctx.Done():
					return nil
				case <-saveSecret:
					if err := d.secret.Save(ctx, a.stateDir); err != nil {
						d.logger.Warn("the renewed certificate is not in its Secret yet", "error", err)
					}
				}
			}
		})
	}
	cfg.Renewal = &link.Renewal{Key: d.key, Store: func(pem string) error {
		if err := d.state.SaveCertificate(pem); err != nil {
			return err //nolint:wrapcheck // says which path
		}
		select {
		case saveSecret <- struct{}{}:
		default:
		}
		return nil
	}}
	g.Go(func() error {
		link.Run(ctx, link.TCPConnector{Address: d.target.hub}, cfg)
		return context.Canceled // stops the Secret saver
	})
	_ = g.Wait() //nolint:errcheck // context.Canceled once the link loop ended
	d.logger.Info("kuben-agent stopped")
	return nil
}
