package cli

// `kuben backup` and `kuben restore` (BackupOpts, RestoreOpts; the work is
// package cli/backup).

import (
	"github.com/spf13/cobra"

	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/maintenance/backup"
	"github.com/Teamtem-dev/kuben/internal/server"
)

func backupCmd(g *globals) *cobra.Command {
	var (
		out  string
		keep uint32
		opts backup.Options
	)
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Back up the database (and, if asked, the secret keyring) into a new directory under `backup.dir`",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := g.load()
			if err != nil {
				return err
			}
			if c.Flags().Changed("out") {
				opts.Out = opt.Some(out)
			}
			if c.Flags().Changed("keep") {
				opts.Keep = opt.Some(keep)
			}
			return backup.RunTo(c.Context(), cfg, opts, g.stdout, g.stderr) //nolint:wrapcheck // explains itself
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&out, "out", "", "Directory the backup is written under [default: backup.dir]")
	flags.BoolVar(&opts.IncludeKeyring, "include-keyring", false,
		"Also copy the secret keyring into the backup. Whoever holds such a backup can read every secret: keep it encrypted")
	flags.Uint32Var(&keep, "keep", 0, "Backups kept under the directory [default: backup.keep]")
	flags.BoolVar(&opts.Scheduled, "scheduled", false, "Record the backup as scheduled (the timer and the CronJob set it)")
	_ = flags.MarkHidden("scheduled") //nolint:errcheck // the flag is defined above
	return cmd
}

func restoreCmd(g *globals) *cobra.Command {
	var opts backup.RestoreOptions
	cmd := &cobra.Command{
		Use:   "restore",
		Short: "Restore a backup made by `kuben backup` into an empty database",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := g.load()
			if err != nil {
				return err
			}
			return backup.Restore(c.Context(), cfg, opts, g.stdout, server.Logger(cfg.Telemetry)) //nolint:wrapcheck // explains itself
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&opts.From, "from", "", "A backup directory ('kuben-<time>') made by 'kuben backup'")
	flags.BoolVar(&opts.Check, "check", false, "Only check that the backup is intact and restorable")
	_ = cmd.MarkFlagRequired("from") //nolint:errcheck // the flag is defined above
	return cmd
}
