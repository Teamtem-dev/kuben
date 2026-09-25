package cli

// `kuben support-bundle` (cli/support.rs).

import "github.com/spf13/cobra"

func supportBundleCmd(*globals) *cobra.Command {
	return &cobra.Command{Use: "support-bundle", Short: "Write a local support bundle: versions, allowlisted configuration, doctor, database counts, cluster state and (with --logs) Kuben's logs. Nothing is uploaded; `--preview` writes nothing", RunE: notPorted}
}
