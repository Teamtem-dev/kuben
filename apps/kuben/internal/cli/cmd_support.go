package cli

// `kuben support-bundle` (SupportOpts; the work is package cli/supportbundle).

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/cli/supportbundle"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
)

func supportBundleCmd(g *globals) *cobra.Command {
	var (
		out  string
		opts supportbundle.Options
	)
	cmd := &cobra.Command{
		Use: "support-bundle",
		Short: "Write a local support bundle: versions, allowlisted configuration, doctor, database counts, " +
			"cluster state and (with --logs) Kuben's logs. Nothing is uploaded; `--preview` writes nothing",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if opts.LogLines < 1 || opts.LogLines > supportbundle.MaxLogLines {
				return fmt.Errorf("invalid value '%d' for '--log-lines <LOG_LINES>': %d is not in 1..=%d",
					opts.LogLines, opts.LogLines, supportbundle.MaxLogLines)
			}
			cfg, err := g.load()
			if err != nil {
				return err
			}
			if c.Flags().Changed("out") {
				opts.Out = opt.Some(out)
			}
			env := supportbundle.Env{Build: versionString(), Stdout: g.stdout, Stderr: g.stderr}
			if g.configFile != "" {
				env.ConfigPath = opt.Some(g.configFile)
			}
			return supportbundle.Run(c.Context(), cfg, opts, env) //nolint:wrapcheck // explains itself
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&out, "out", "", "Directory the bundle goes to [default: 'support' in the state dir]")
	flags.BoolVar(&opts.Preview, "preview", false, "Show what the bundle would hold, and write nothing")
	flags.BoolVar(&opts.Logs, "logs", false,
		"Also the newest lines of Kuben's own logs (they may name apps and people: read them before sharing)")
	flags.Uint32Var(&opts.LogLines, "log-lines", 500, "Log lines per container with --logs")
	flags.Uint32Var(&opts.Keep, "keep", 3, "Bundles kept in the directory; older ones are removed")
	return cmd
}
