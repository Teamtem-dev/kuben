package cli

// `kuben setup-token` (bootstrap::print_setup_token).

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/Teamtem-dev/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/host"
	"github.com/Teamtem-dev/kuben/internal/httpapi"
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
			if httpapi.SetupTokenRequired(cfg) {
				t, err := httpapi.CurrentOrNewSetupToken(cfg, time.UnixMilli(clock.System{}.NowMs()))
				if err != nil {
					return err //nolint:wrapcheck // names the file
				}
				token = opt.Some(t)
			}
			_, err = fmt.Fprintln(g.stdout, httpapi.SetupURLAt(host.ConsoleURL(c.Context(), cfg), token))
			return err //nolint:wrapcheck // stdout
		},
	}
}
