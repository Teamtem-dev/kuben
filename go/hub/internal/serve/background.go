package serve

import (
	"context"
	"log/slog"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api"
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/oci"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/health"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/secrets"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/supervise"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
)

// startBackground starts the work every replica shares through SQL claims
// (serve.rs spawn_background): the image update watcher (M5.4). The
// channels are closed when each has ended.
func startBackground(ctx context.Context, st *store.Store, keyring *secrets.Keyring, h *health.Health, logger *slog.Logger) []<-chan struct{} {
	watcher := api.NewImageWatcher(st, oci.Registry{}, opt.Some(keyring), clock.System{}, logger)
	images := supervise.Go(ctx, api.ImageWatchSubsystem, h, logger, func(ctx context.Context) error {
		return api.RunImageWatch(ctx, watcher, h)
	})
	return []<-chan struct{}{images}
}
