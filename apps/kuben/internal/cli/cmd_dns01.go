package cli

// `kuben dns01-issuer` (Dns01Opts; the work is package cli/dns01).

import (
	"github.com/spf13/cobra"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/cli/dns01"
)

func dns01IssuerCmd(g *globals) *cobra.Command {
	var opts dns01.Options
	cmd := &cobra.Command{
		Use:   "dns01-issuer",
		Short: "Set up a cert-manager ClusterIssuer that answers ACME DNS-01 challenges through Cloudflare (wildcard and apex certificates)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := g.load()
			if err != nil {
				return err
			}
			return dns01.Run(c.Context(), cfg, opts, g.stdout) //nolint:wrapcheck // explains itself
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&opts.Name, "name", "letsencrypt-dns", "The ClusterIssuer to create or update")
	flags.StringVar(&opts.Email, "email", "", "The ACME account's email address")
	flags.StringVar(&opts.TokenFile, "token-file", "", "A file holding a Cloudflare API token with Zone:DNS:Edit on the zones")
	flags.StringVar(&opts.Namespace, "namespace", "cert-manager", "The namespace cert-manager runs in (the token Secret goes there)")
	flags.BoolVar(&opts.Staging, "staging", false, "Use Let's Encrypt's staging server (untrusted certificates)")
	flags.BoolVar(&opts.DryRun, "dry-run", false, "Print the objects instead of applying them (the token is redacted)")
	_ = cmd.MarkFlagRequired("email")      //nolint:errcheck // the flag is defined above
	_ = cmd.MarkFlagRequired("token-file") //nolint:errcheck // the flag is defined above
	return cmd
}
