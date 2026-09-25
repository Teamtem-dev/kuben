package cli

// `kuben dns01-issuer` (cli/dns01.rs).

import "github.com/spf13/cobra"

func dns01IssuerCmd(*globals) *cobra.Command {
	return &cobra.Command{Use: "dns01-issuer", Short: "Set up a cert-manager ClusterIssuer that answers ACME DNS-01 challenges through Cloudflare (wildcard and apex certificates)", RunE: notPorted}
}
