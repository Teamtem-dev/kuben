// Package cli is the `kuben` command line (crates/kuben/src/cli/mod.rs and
// main.rs): the server's commands (`serve`, `migrate`, `doctor`, `setup`,
// `backup`, …) and a client of a server (`login`, `apps`, `deploy`,
// `status`, `logs`, `rollback`).
//
// Each command lives in its own cmd_*.go file; the command list and the
// options every command shares are here.
package cli

import (
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/config"
	"github.com/Teamtem-dev/kuben/go/hub/internal/version"
)

// envAnnotation names the environment variable a flag falls back to, as
// clap's `#[arg(env = …)]` did: the flag wins, then the variable, then the
// default.
const envAnnotation = "kuben_env"

// globals are the options every command shares.
type globals struct {
	// configFile is `--config` / KUBEN_CONFIG.
	configFile string
	stdin      io.Reader
	stdout     io.Writer
	stderr     io.Writer
}

// load reads the configuration: defaults, /etc/kuben/config.toml,
// ./kuben.toml searched upward, the `--config` file, then `KUBEN_*`
// variables (Cli::load_config).
func (g *globals) load() (config.Config, error) {
	src := config.DefaultSource()
	if g.configFile != "" {
		src.Files = append(src.Files, g.configFile)
	}
	cfg, err := src.Load()
	if err != nil {
		return config.Config{}, fmt.Errorf("configuration: %w", err)
	}
	return cfg, nil
}

// Root is the `kuben` command. The streams are the process's unless a test
// sets them on the returned command.
func Root() *cobra.Command {
	g := &globals{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr}
	cmd := &cobra.Command{
		Use:           "kuben",
		Short:         "Kuben — Kubernetes-native PaaS in a single binary",
		Version:       version.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(c *cobra.Command, _ []string) error {
			g.stdin, g.stdout, g.stderr = c.InOrStdin(), c.OutOrStdout(), c.ErrOrStderr()
			return applyEnv(c)
		},
	}
	cmd.SetVersionTemplate(versionString() + "\n")
	flags := cmd.PersistentFlags()
	flags.StringVar(&g.configFile, "config", "", "Path to a TOML config file (merged over defaults, under env vars)")
	bindEnv(flags, "config", "KUBEN_CONFIG")
	cmd.AddCommand(
		serveCmd(g),
		migrateCmd(g),
		doctorCmd(g),
		resetAdminCmd(g),
		setupTokenCmd(g),
		agentTokenCmd(g),
		setupCmd(g),
		statusCmd(g),
		loginCmd(g),
		appsCmd(g),
		deployCmd(g),
		logsCmd(g),
		rollbackCmd(g),
		uninstallCmd(g),
		upgradeCheckCmd(g),
		backupCmd(g),
		restoreCmd(g),
		supportBundleCmd(g),
		dns01IssuerCmd(g),
		versionCmd(g),
		copySelfCmd(g),
	)
	return cmd
}

// bindEnv makes the flag fall back to the environment variable.
func bindEnv(flags *pflag.FlagSet, name, env string) {
	f := flags.Lookup(name)
	if f == nil {
		return
	}
	if f.Annotations == nil {
		f.Annotations = map[string][]string{}
	}
	f.Annotations[envAnnotation] = []string{env}
}

// applyEnv sets every flag of the command (and its parents) that was not
// given on the command line from its environment variable, if set.
func applyEnv(c *cobra.Command) error {
	var failed error
	visit := func(f *pflag.Flag) {
		env, ok := f.Annotations[envAnnotation]
		if !ok || len(env) == 0 || f.Changed || failed != nil {
			return
		}
		value, set := os.LookupEnv(env[0])
		if !set {
			return
		}
		if err := f.Value.Set(value); err != nil {
			failed = fmt.Errorf("invalid value %q for %s (from %s): %w", value, "--"+f.Name, env[0], err)
			return
		}
		f.Changed = true
	}
	c.Flags().VisitAll(visit)
	c.InheritedFlags().VisitAll(visit)
	return failed
}

// versionString is `kuben <version> (<os> <arch>)`, with Rust's names for
// the operating system and architecture (std::env::consts).
func versionString() string {
	return fmt.Sprintf("kuben %s (%s %s)", version.Version, rustOS(runtime.GOOS), rustArch(runtime.GOARCH))
}

func rustOS(goos string) string {
	if goos == "darwin" {
		return "macos"
	}
	return goos
}

func rustArch(goarch string) string {
	switch goarch {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	case "386":
		return "x86"
	default:
		return goarch
	}
}
