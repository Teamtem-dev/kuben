package cli

// The client commands (cli/client.rs): status, login, apps, deploy, logs,
// rollback, against a Kuben server with an API token. The server and token
// come from the context file `kuben login` writes (owner-only), or from
// KUBEN_URL and KUBEN_TOKEN (CI).

import (
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
)

// targetFlags adds --context, --project and --environment to cmd; the
// returned function reads them once the command runs (after applyEnv).
func targetFlags(cmd *cobra.Command) func() clientTarget {
	var context, project, environment string
	flags := cmd.Flags()
	flags.StringVar(&context, "context", "", "A context written by `kuben login` (default: the current one)")
	bindEnv(flags, "context", "KUBEN_CONTEXT")
	flags.StringVar(&project, "project", "", "Project of apps named without one")
	bindEnv(flags, "project", "KUBEN_PROJECT")
	flags.StringVar(&environment, "environment", "", "Environment of apps named without one")
	bindEnv(flags, "environment", "KUBEN_ENVIRONMENT")
	return func() clientTarget {
		return clientTarget{
			context:     givenFlag(flags, "context", context),
			project:     givenFlag(flags, "project", project),
			environment: givenFlag(flags, "environment", environment),
		}
	}
}

// givenFlag is value when the flag was set on the command line or from its
// environment variable (clap's Option<T>), None otherwise.
func givenFlag[T any](flags *pflag.FlagSet, name string, value T) opt.Val[T] {
	if f := flags.Lookup(name); f != nil && f.Changed {
		return opt.Some(value)
	}
	return opt.None[T]()
}

func statusCmd(g *globals) *cobra.Command {
	var opts statusOpts
	cmd := &cobra.Command{
		Use:   "status [APP]",
		Short: "An app's state and Doctor (`kuben status shop/prod/web`); without an app, the service, admin account, cluster and console address of this server",
		Long: "An app's state and Doctor (`kuben status shop/prod/web`); without an app, the service, admin account, cluster and console address of this server.\n\n" +
			"APP is an app (project/environment/app): its state and Doctor. Without one, the state of this server",
		Args: cobra.MaximumNArgs(1),
	}
	cmd.Flags().BoolVar(&opts.json, "json", false, "Print JSON")
	readTarget := targetFlags(cmd)
	cmd.RunE = func(c *cobra.Command, args []string) error {
		if len(args) == 0 {
			// Without an app, the state of this server (cli/setup status),
			// ported with `kuben setup` in cmd_setup.go.
			return setupStatus(g)
		}
		opts.target = readTarget()
		return runAppStatus(c.Context(), g, opts, args[0], os.LookupEnv)
	}
	return cmd
}

func loginCmd(g *globals) *cobra.Command {
	var token, name string
	cmd := &cobra.Command{
		Use:   "login URL",
		Short: "Sign in to a Kuben server with an API token; later commands use it",
		Long: "Sign in to a Kuben server with an API token; later commands use it.\n\n" +
			"URL is the server, e.g. https://kuben.example.com",
		Args: cobra.ExactArgs(1),
	}
	flags := cmd.Flags()
	flags.StringVar(&token, "token", "", "An API token (Account → API tokens); without it, it is read from standard input")
	bindEnv(flags, "token", "KUBEN_TOKEN")
	flags.StringVar(&name, "name", "", "Name of the context (default: the server's host)")
	readTarget := targetFlags(cmd)
	cmd.RunE = func(c *cobra.Command, args []string) error {
		opts := loginOpts{
			url:    args[0],
			token:  givenFlag(flags, "token", token),
			name:   givenFlag(flags, "name", name),
			target: readTarget(),
		}
		return runLogin(c.Context(), g, opts, os.LookupEnv)
	}
	return cmd
}

func appsCmd(g *globals) *cobra.Command {
	var opts appsOpts
	cmd := &cobra.Command{
		Use:   "apps",
		Short: "The apps you can see, with their state",
		Args:  cobra.NoArgs,
	}
	readTarget := targetFlags(cmd)
	cmd.Flags().BoolVar(&opts.json, "json", false, "Print JSON")
	cmd.RunE = func(c *cobra.Command, _ []string) error {
		opts.target = readTarget()
		return runApps(c.Context(), g, opts, os.LookupEnv)
	}
	return cmd
}

func deployCmd(g *globals) *cobra.Command {
	var opts deployOpts
	var key string
	cmd := &cobra.Command{
		Use:   "deploy APP",
		Short: "Deploy an image to an app and wait until it runs",
		Long: "Deploy an image to an app and wait until it runs.\n\n" +
			"APP is the app: project/environment/app, or app with --project and --environment",
		Args: cobra.ExactArgs(1),
	}
	flags := cmd.Flags()
	flags.StringVar(&opts.image, "image", "", "The image; a tag is resolved to its digest first")
	_ = cmd.MarkFlagRequired("image") //nolint:errcheck // the flag was just added
	flags.StringVar(&key, "idempotency-key", "", "Retrying with the same key returns the same deployment (default: a new key)")
	flags.BoolVar(&opts.noWait, "no-wait", false, "Return once the deployment is accepted")
	flags.Uint64Var(&opts.timeout, "timeout", 600, "How long to wait for the deployment, seconds")
	readTarget := targetFlags(cmd)
	cmd.RunE = func(c *cobra.Command, args []string) error {
		opts.app, opts.target = args[0], readTarget()
		opts.idempotencyKey = givenFlag(flags, "idempotency-key", key)
		return runDeploy(c.Context(), g, opts, os.LookupEnv)
	}
	return cmd
}

func logsCmd(g *globals) *cobra.Command {
	var opts logsOpts
	var tail int64
	var process string
	cmd := &cobra.Command{
		Use:   "logs APP",
		Short: "An app's log lines; `-f` keeps following them",
		Args:  cobra.ExactArgs(1),
	}
	flags := cmd.Flags()
	flags.BoolVarP(&opts.follow, "follow", "f", false, "Keep printing new lines")
	flags.BoolVar(&opts.previous, "previous", false, "Lines of the container before its last restart")
	cmd.MarkFlagsMutuallyExclusive("follow", "previous")
	flags.Int64Var(&tail, "tail", 0, "Lines per pod to start with")
	flags.StringVar(&process, "process", "", "Only pods of this process")
	readTarget := targetFlags(cmd)
	cmd.RunE = func(c *cobra.Command, args []string) error {
		opts.app, opts.target = args[0], readTarget()
		opts.tail, opts.process = givenFlag(flags, "tail", tail), givenFlag(flags, "process", process)
		return runLogs(c.Context(), g, opts, os.LookupEnv)
	}
	return cmd
}

func rollbackCmd(g *globals) *cobra.Command {
	var opts rollbackOpts
	var to int64
	cmd := &cobra.Command{
		Use:   "rollback APP",
		Short: "Return an app to an earlier revision",
		Args:  cobra.ExactArgs(1),
	}
	flags := cmd.Flags()
	flags.Int64Var(&to, "to", 0, "The revision to return to (default: the one before the current)")
	readTarget := targetFlags(cmd)
	cmd.RunE = func(c *cobra.Command, args []string) error {
		opts.app, opts.target, opts.to = args[0], readTarget(), givenFlag(flags, "to", to)
		return runRollback(c.Context(), g, opts, os.LookupEnv)
	}
	return cmd
}
