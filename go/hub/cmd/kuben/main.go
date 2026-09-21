// Command kuben is the Kuben server and its command line.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/config"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/serve"
	"github.com/Teamtem-dev/kuben/go/hub/internal/version"
)

func main() {
	if err := root().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func root() *cobra.Command {
	var configFile string
	cmd := &cobra.Command{
		Use:           "kuben",
		Short:         "A Kubernetes platform you can run yourself",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version.Version,
	}
	cmd.PersistentFlags().StringVar(&configFile, "config", "", "configuration file layered over the defaults")
	load := func() (config.Config, error) {
		src := config.DefaultSource()
		if configFile != "" {
			src.Files = append(src.Files, configFile)
		}
		return src.Load() //nolint:wrapcheck // config errors are for the operator as they are
	}
	cmd.AddCommand(serveCmd(load), setupTokenCmd(load), versionCmd())
	return cmd
}

func serveCmd(load func() (config.Config, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the server",
		RunE: func(*cobra.Command, []string) error {
			cfg, err := load()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return serve.Run(ctx, cfg, serve.Logger(cfg.Telemetry)) //nolint:wrapcheck // already explained
		},
	}
}

func setupTokenCmd(load func() (config.Config, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "setup-token",
		Short: "Print a fresh first-run setup link",
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := load()
			if err != nil {
				return err
			}
			token := opt.None[string]()
			if api.SetupTokenRequired(cfg) {
				t, err := api.CurrentOrNewSetupToken(cfg, time.Now())
				if err != nil {
					return err //nolint:wrapcheck // names the file
				}
				token = opt.Some(t)
			}
			host := serve.AdvertiseIP().Or("localhost")
			_, err = fmt.Fprintln(c.OutOrStdout(), api.SetupURLAt(cfg.ConsoleURLWithHost(host), token))
			return err //nolint:wrapcheck // stdout
		},
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		RunE: func(c *cobra.Command, _ []string) error {
			commit := version.Commit()
			if commit != "" {
				commit = " (" + commit[:min(len(commit), 12)] + ")"
			}
			_, err := fmt.Fprintf(c.OutOrStdout(), "kuben %s%s\n", version.Version, commit)
			return err //nolint:wrapcheck // stdout
		},
	}
}
