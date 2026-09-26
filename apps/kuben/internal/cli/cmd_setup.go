package cli

// `kuben setup`, `kuben uninstall` and `kuben status` without an app
// (cli/setup, SetupOpts and UninstallOpts).

import (
	"context"
	"errors"

	"github.com/spf13/cobra"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/cli/setup"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/version"
)

// setupFlags are the flags of `kuben setup` as parsed; opts turns them
// into setup.Opts.
type setupFlags struct {
	port           uint16
	kubeconfig     string
	noK3s          bool
	bindLocal      bool
	yes            bool
	plan           bool
	domain         string
	acmeEmail      string
	acmeStaging    bool
	datastore      setup.Datastore
	allowHTTPSetup bool
}

func setupCmd(g *globals) *cobra.Command {
	var f setupFlags
	cmd := &cobra.Command{
		Use: "setup",
		Short: "Make this Linux server a Kuben server: k3s if needed, a system user, the config, a systemd service, " +
			"the firewall. Re-run to upgrade or repair",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			// main.rs loaded the configuration before every command.
			if _, err := g.load(); err != nil {
				return err
			}
			return setup.Setup(c.Context(), f.opts(c), g.stdout, g.stderr, version.Version) //nolint:wrapcheck // explains itself
		},
	}
	flags := cmd.Flags()
	flags.Uint16Var(&f.port, "port", 0, "Port of the console and API [default: 3000, or the next free port when 3000 is "+
		"taken; an existing install keeps its port]")
	bindEnv(flags, "port", "KUBEN_PORT")
	flags.StringVar(&f.kubeconfig, "kubeconfig", "", "Manage this cluster instead of installing k3s")
	bindEnv(flags, "kubeconfig", "KUBEN_KUBECONFIG")
	flags.BoolVar(&f.noK3s, "no-k3s", false, "Never install k3s; fail when no cluster is found")
	flags.BoolVar(&f.bindLocal, "bind-local", false, "Listen on 127.0.0.1 only (reach the console through an SSH tunnel)")
	flags.BoolVarP(&f.yes, "yes", "y", false, "Go ahead on warnings without asking")
	flags.BoolVar(&f.plan, "plan", false, "Show what setup would do, and change nothing")
	flags.StringVar(&f.domain, "domain", "", "Base domain for app hostnames (<app>-<environment>.<domain>); point a "+
		"wildcard DNS record at this server")
	bindEnv(flags, "domain", "KUBEN_DOMAIN")
	flags.StringVar(&f.acmeEmail, "acme-email", "", "Email for Let's Encrypt: apps get HTTPS certificates. Without it they "+
		"are served over plain HTTP")
	bindEnv(flags, "acme-email", "KUBEN_ACME_EMAIL")
	flags.BoolVar(&f.acmeStaging, "acme-staging", false, "Use Let's Encrypt's staging server (untrusted certificates, for tests)")
	f.datastore = setup.DatastoreSqlite
	flags.Var(&f.datastore, "datastore", "How an installed k3s keeps its state: sqlite (one server) or etcd (a server "+
		"that will grow)")
	flags.BoolVar(&f.allowHTTPSetup, "allow-http-setup", false, "Let the first admin be created over plain HTTP from "+
		"another machine, on a network you trust. Without it: HTTPS or an SSH tunnel")
	return cmd
}

// opts are the parsed flags. A string option given neither on the command
// line nor (non-empty) in its variable is absent, as clap treated an empty
// variable.
func (f setupFlags) opts(c *cobra.Command) setup.Opts {
	given := func(name, value string) opt.Val[string] {
		if !c.Flags().Changed(name) || value == "" {
			return opt.None[string]()
		}
		return opt.Some(value)
	}
	port := opt.None[uint16]()
	if c.Flags().Changed("port") {
		port = opt.Some(f.port)
	}
	return setup.Opts{
		Port:           port,
		Kubeconfig:     given("kubeconfig", f.kubeconfig),
		NoK3s:          f.noK3s,
		BindLocal:      f.bindLocal,
		Yes:            f.yes,
		Plan:           f.plan,
		Domain:         given("domain", f.domain),
		AcmeEmail:      given("acme-email", f.acmeEmail),
		AcmeStaging:    f.acmeStaging,
		Datastore:      f.datastore,
		AllowHTTPSetup: f.allowHTTPSetup,
	}
}

func uninstallCmd(g *globals) *cobra.Command {
	var opts setup.UninstallOpts
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the service installed by `kuben setup` (with --purge: everything)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if opts.KeepApps && !opts.Purge {
				return errors.New("the following required arguments were not provided:\n  --purge")
			}
			if _, err := g.load(); err != nil {
				return err
			}
			return setup.Uninstall(c.Context(), opts, g.stdout, g.stderr, version.Version) //nolint:wrapcheck // explains itself
		},
	}
	flags := cmd.Flags()
	flags.BoolVar(&opts.Purge, "purge", false, "Also delete the data, the configuration, the binary, and k3s when "+
		"'kuben setup' installed it")
	flags.BoolVar(&opts.KeepApps, "keep-apps", false, "With --purge: keep the apps running without Kuben. The cluster "+
		"(k3s too), the Gateway, the ClusterIssuer and the apps' namespaces, Secrets and volumes stay; what is left is "+
		"listed. Detach the apps first for a clean handover")
	flags.BoolVarP(&opts.Yes, "yes", "y", false, "Do not ask for confirmation")
	return cmd
}

// setupStatus is `kuben status` without an app: the service, admin
// account, cluster and console address of this server.
func setupStatus(g *globals) error {
	return setup.Status(context.Background(), g.stdout, g.stderr, version.Version) //nolint:wrapcheck // explains itself
}
