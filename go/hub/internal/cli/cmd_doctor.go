package cli

// `kuben doctor` (cli/doctor.rs).

import "github.com/spf13/cobra"

func doctorCmd(*globals) *cobra.Command {
	return &cobra.Command{Use: "doctor", Short: "Check the environment (database, cluster, gateway, cert-manager) and exit", RunE: notPorted}
}
