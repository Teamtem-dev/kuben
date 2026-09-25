package cli

// `kuben doctor` (DoctorOpts; the checks are package cli/doctor).

import (
	"github.com/spf13/cobra"

	"github.com/Teamtem-dev/kuben/internal/cli/doctor"
	serve "github.com/Teamtem-dev/kuben/internal/server"
)

func doctorCmd(g *globals) *cobra.Command {
	var opts doctor.Options
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the environment (database, cluster, gateway, cert-manager) and exit",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := g.load()
			if err != nil {
				return err
			}
			return doctor.Run(c.Context(), cfg, opts, versionString(), g.stdout, serve.Logger(cfg.Telemetry)) //nolint:wrapcheck // explains itself
		},
	}
	cmd.Flags().BoolVar(&opts.Cluster, "cluster", false,
		"Check only the cluster (before installing Kuben into it, BYOK): what each feature needs, what is there, "+
			"and the permissions Kuben needs. No database is needed")
	return cmd
}
