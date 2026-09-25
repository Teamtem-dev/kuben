package cli

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/Teamtem-dev/kuben/internal/core/config"
)

// parseServe parses `kuben serve` arguments the way Execute does, without
// running the server.
func parseServe(t *testing.T, args ...string) serveOpts {
	t.Helper()
	cmd := serveCmd(&globals{})
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	if err := applyEnv(cmd); err != nil {
		t.Fatal(err)
	}
	roles, err := cmd.Flags().GetStringSlice("roles")
	if err != nil {
		t.Fatal(err)
	}
	dev, err := cmd.Flags().GetBool("dev")
	if err != nil {
		t.Fatal(err)
	}
	return serveOpts{roles: roles, dev: dev}
}

func TestParsesServeRoles(t *testing.T) {
	opts := parseServe(t, "--roles=api,controller", "--dev")
	cfg, err := opts.apply(config.Default())
	if err != nil {
		t.Fatal(err)
	}
	if want := []config.Role{config.RoleAPI, config.RoleController}; !slices.Equal(cfg.Server.Roles, want) {
		t.Errorf("roles %v, want %v", cfg.Server.Roles, want)
	}
	if !opts.dev {
		t.Error("--dev not read")
	}
}

func TestDevFlagDisablesSecureCookie(t *testing.T) {
	cfg, err := parseServe(t, "--dev").apply(config.Default())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CookieSecure() {
		t.Error("cookie still secure")
	}
	if cfg.Telemetry.LogFormat != "pretty" {
		t.Errorf("log format %s", cfg.Telemetry.LogFormat)
	}
}

func TestRolesFallBackToTheEnvironmentButTheFlagWins(t *testing.T) {
	t.Setenv("KUBEN_ROLES", "controller")
	if got := parseServe(t).roles; !slices.Equal(got, []string{"controller"}) {
		t.Errorf("from env: %v", got)
	}
	if got := parseServe(t, "--roles=api").roles; !slices.Equal(got, []string{"api"}) {
		t.Errorf("flag over env: %v", got)
	}
}

func TestAnUnknownRoleIsRefused(t *testing.T) {
	_, err := parseServe(t, "--roles=api,web").apply(config.Default())
	if err == nil || !strings.Contains(err.Error(), "unknown variant `web`") {
		t.Fatalf("err = %v", err)
	}
}

func TestVersionNamesTheSystemAsRustDid(t *testing.T) {
	if got := rustOS("darwin") + " " + rustArch("amd64") + " " + rustArch("arm64"); got != "macos x86_64 aarch64" {
		t.Errorf("got %s", got)
	}
	var out bytes.Buffer
	if err := printVersion(&out, true, false); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out.String(), "\n")
	if !strings.HasPrefix(lines[0], "kuben ") || lines[1] != "bundle lock (schema 1)" {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestVersionJSONNeedsBundle(t *testing.T) {
	root := Root()
	root.SetArgs([]string{"version", "--json"})
	var out bytes.Buffer
	root.SetOut(&out)
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "requires '--bundle'") {
		t.Fatalf("err = %v", err)
	}
}

// pflag takes the first `quoted` word of a usage as the flag's value name
// (`--keep backup.keep` instead of `--keep uint32`), so no usage has one.
func TestNoFlagUsageHasABackquote(t *testing.T) {
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		c.LocalFlags().VisitAll(func(f *pflag.Flag) {
			if strings.Contains(f.Usage, "`") {
				t.Errorf("%s --%s: %s", c.CommandPath(), f.Name, f.Usage)
			}
		})
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(Root())
}

func TestVersionAndUsageErrorsBehaveAsClapDid(t *testing.T) {
	for _, args := range [][]string{{"--version"}, {"-V"}} {
		root := Root()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if got := out.String(); !strings.HasPrefix(got, "kuben ") || strings.Contains(got, "(") {
			t.Errorf("%v printed %q", args, got)
		}
	}
	for _, args := range [][]string{{"serve", "--no-such-flag"}, {"no-such-command"}} {
		root := Root()
		root.SetOut(&bytes.Buffer{})
		root.SetErr(&bytes.Buffer{})
		root.SetArgs(args)
		if code := ExitCode(root.Execute()); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	if ExitCode(errors.New("boom")) != 1 || ExitCode(nil) != 0 {
		t.Error("plain errors exit 1, success 0")
	}
}
