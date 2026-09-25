package cli

// `kuben reset-admin` (cli/admin.rs) and `kuben agent-token` (cli/agent.rs).

import "github.com/spf13/cobra"

func resetAdminCmd(*globals) *cobra.Command {
	return &cobra.Command{Use: "reset-admin", Short: "Reset (or create) the admin user's password", RunE: notPorted}
}

func agentTokenCmd(*globals) *cobra.Command {
	return &cobra.Command{Use: "agent-token", Short: "Issue a bootstrap token for a cluster's agent: printed once, only its hash is kept", RunE: notPorted}
}
