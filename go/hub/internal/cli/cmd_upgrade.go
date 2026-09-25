package cli

// `kuben migrate` and `kuben upgrade-check` (cli/upgrade.rs).

import "github.com/spf13/cobra"

func migrateCmd(*globals) *cobra.Command {
	return &cobra.Command{Use: "migrate", Short: "Apply database migrations and exit", RunE: notPorted}
}

func upgradeCheckCmd(*globals) *cobra.Command {
	return &cobra.Command{Use: "upgrade-check", Short: "Check that this binary may upgrade the installation (run it before the new version replaces the old one); nothing is changed", RunE: notPorted}
}
