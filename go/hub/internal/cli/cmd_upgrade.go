package cli

// `kuben migrate` and `kuben upgrade-check` (cli/upgrade.rs; main.rs's
// Migrate arm).

import (
	"github.com/spf13/cobra"

	"github.com/Teamtem-dev/kuben/go/hub/internal/cli/upgrade"
	"github.com/Teamtem-dev/kuben/go/hub/internal/serve"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
)

func migrateCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "Apply database migrations and exit",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := g.load()
			if err != nil {
				return err
			}
			logger := serve.Logger(cfg.Telemetry)
			ctx, stop := runCtx(c)
			defer stop()
			st, err := store.ConnectUnmigrated(ctx, cfg.Database)
			if err != nil {
				return err //nolint:wrapcheck // explains itself
			}
			defer st.Close()
			if _, err := upgrade.Migrate(ctx, cfg, st, logger); err != nil {
				return err //nolint:wrapcheck // explains itself
			}
			logger.Info("migrations applied", "backend", st.Backend())
			return nil
		},
	}
}

func upgradeCheckCmd(g *globals) *cobra.Command {
	var major bool
	cmd := &cobra.Command{
		Use:   "upgrade-check",
		Short: "Check that this binary may upgrade the installation (run it before the new version replaces the old one); nothing is changed",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := g.load()
			if err != nil {
				return err
			}
			ctx, stop := runCtx(c)
			defer stop()
			return upgrade.Check(ctx, cfg, major, g.stdout) //nolint:wrapcheck // explains itself
		},
	}
	cmd.Flags().BoolVar(&major, "major", false, "Allow a major version step (after reading its release notes)")
	return cmd
}
