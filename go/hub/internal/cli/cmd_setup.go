package cli

// `kuben setup` and `kuben uninstall` (cli/setup).

import "github.com/spf13/cobra"

func setupCmd(*globals) *cobra.Command {
	return &cobra.Command{Use: "setup", Short: "Make this Linux server a Kuben server: k3s if needed, a system user, the config, a systemd service, the firewall. Re-run to upgrade or repair", RunE: notPorted}
}

func uninstallCmd(*globals) *cobra.Command {
	return &cobra.Command{Use: "uninstall", Short: "Remove the service installed by `kuben setup` (with --purge: everything)", RunE: notPorted}
}
