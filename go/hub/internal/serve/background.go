package serve

import (
	"context"
	"log/slog"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/github"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/notify"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/oci"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/config"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/health"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/secrets"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/supervise"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
)

// startBackground starts the work every replica shares through SQL claims
// (serve.rs spawn_background): the notifier (M4.10: incidents, webhooks
// and commit statuses from the outbox; app reports the commit statuses)
// and the image update watcher (M5.4). The channels are closed when each
// has ended.
func startBackground(
	ctx context.Context, cfg config.Config, st *store.Store, keyring *secrets.Keyring, app opt.Val[*github.App],
	h *health.Health, logger *slog.Logger,
) []<-chan struct{} {
	notifier := notify.New(notify.Deps{
		Store: st, Keyring: keyring, GitHub: app, Config: cfg.Notify, PublicURL: cfg.Server.PublicURL,
		Clock: clock.System{}, Logger: logger,
	})
	notifications := supervise.Go(ctx, notify.Subsystem, h, logger, func(ctx context.Context) error {
		return notify.Run(ctx, notifier, h)
	})
	watcher := api.NewImageWatcher(st, oci.Registry{}, opt.Some(keyring), clock.System{}, logger)
	images := supervise.Go(ctx, api.ImageWatchSubsystem, h, logger, func(ctx context.Context) error {
		return api.RunImageWatch(ctx, watcher, h)
	})
	return []<-chan struct{}{notifications, images}
}
