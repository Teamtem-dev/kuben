package cli

// `kuben serve` (ServeOpts and the serve arm of Cli::load_config).

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/server"
)

// serveOpts are the options of `kuben serve`.
type serveOpts struct {
	// roles to run in this process (comma separated); empty keeps the
	// configured ones.
	roles []string
	// dev is development mode: pretty logs and an insecure cookie.
	dev bool
}

func serveCmd(g *globals) *cobra.Command {
	var opts serveOpts
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the server (API, controllers, activator) according to --roles",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := g.load()
			if err != nil {
				return err
			}
			if cfg, err = opts.apply(cfg); err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(c.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return server.Run(ctx, cfg, server.Logger(cfg.Telemetry)) //nolint:wrapcheck // already explained
		},
	}
	flags := cmd.Flags()
	flags.StringSliceVar(&opts.roles, "roles", nil, "Roles to run in this process (comma separated). Defaults to config")
	bindEnv(flags, "roles", "KUBEN_ROLES")
	flags.BoolVar(&opts.dev, "dev", false, "Development mode: pretty logs and an insecure cookie. The database is still PostgreSQL (database.url)")
	return cmd
}

// apply lays the command-line options over the configuration, as
// Cli::load_config did for `serve`.
func (o serveOpts) apply(cfg config.Config) (config.Config, error) {
	if len(o.roles) > 0 {
		roles := make([]config.Role, 0, len(o.roles))
		for _, name := range o.roles {
			role, err := config.ParseRole(strings.TrimSpace(name))
			if err != nil {
				return cfg, fmt.Errorf("invalid value '%s' for '--roles <ROLES>': %w", name, err)
			}
			roles = append(roles, role)
		}
		cfg.Server.Roles = roles
	}
	if o.dev {
		cfg.Security.CookieSecure = config.CookieFixed(false)
		cfg.Telemetry.LogFormat = "pretty"
		if cfg.Telemetry.LogLevel == "info" {
			cfg.Telemetry.LogLevel = "debug,hyper=info,h2=info,tower=info"
		}
	}
	return cfg, nil
}

// runCtx is the context of a one-shot command: cancelled by an interrupt.
func runCtx(c *cobra.Command) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(c.Context(), os.Interrupt, syscall.SIGTERM)
}
