package cli

// `kuben setup-token` (bootstrap::print_setup_token).

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/host"
)

func setupTokenCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "setup-token",
		Short: "Print a fresh link to the first-run setup page (`/setup`), with its token",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := g.load()
			if err != nil {
				return err
			}
			token := opt.None[string]()
			if api.SetupTokenRequired(cfg) {
				t, err := api.CurrentOrNewSetupToken(cfg, time.UnixMilli(clock.System{}.NowMs()))
				if err != nil {
					return err //nolint:wrapcheck // names the file
				}
				token = opt.Some(t)
			}
			_, err = fmt.Fprintln(g.stdout, api.SetupURLAt(host.ConsoleURL(c.Context(), cfg), token))
			return err //nolint:wrapcheck // stdout
		},
	}
}
