package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/Teamtem-dev/kuben/internal/agent/state"
	"github.com/Teamtem-dev/kuben/internal/agentlink/protocol"
)

// args are the command line and environment, as clap read them in
// main.rs: every flag but --token-stdin may come from its KUBEN_AGENT_*
// variable, and a value from the environment counts as given.
type args struct {
	hub            string
	hubName        string
	hubCA          string
	cluster        string
	enrollmentDir  string
	identitySecret string
	stateDir       string
	tokenFile      string
	tokenStdin     bool
	logFormat      string
	// given are the flags given on the command line or in the environment.
	given map[string]bool
}

// flagSpec is one flag: its name, its variable and its default.
type flagSpec struct {
	name, env, def, usage string
	to                    *string
}

func (a *args) specs() []flagSpec {
	return []flagSpec{
		{"hub", "KUBEN_AGENT_HUB", "", "The hub's AgentLink address, `host:port`", &a.hub},
		{"hub-name", "KUBEN_AGENT_HUB_NAME", protocol.HubName, "The name the hub's certificate carries", &a.hubName},
		{"hub-ca", "KUBEN_AGENT_HUB_CA", "", "The hub CA to pin, in PEM", &a.hubCA},
		{"cluster", "KUBEN_AGENT_CLUSTER", "", "This cluster's id", &a.cluster},
		{
			"enrollment-dir", "KUBEN_AGENT_ENROLLMENT_DIR", "",
			"Inside Kuben's own cluster: the directory where the hub publishes its address, CA, this cluster's id " +
				"and a bootstrap token (a mounted Secret). Replaces --hub, --hub-ca, --cluster and --token-file",
			&a.enrollmentDir,
		},
		{
			"identity-secret", "KUBEN_AGENT_IDENTITY_SECRET", "",
			"Keep the device key and certificate in this Secret of the pod's own namespace too, so a new pod keeps the identity",
			&a.identitySecret,
		},
		{"state-dir", "KUBEN_AGENT_STATE_DIR", "/var/lib/kuben-agent", "Where the device key and the certificate live", &a.stateDir},
		{"token-file", "KUBEN_AGENT_TOKEN_FILE", "", "A file holding the bootstrap token, read only to enroll", &a.tokenFile},
		{"log-format", "KUBEN_AGENT_LOG_FORMAT", "json", "`json` or `pretty`", &a.logFormat},
	}
}

// usageError is a command line clap refused (exit code 2).
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

// command is the CLI; run is called with the parsed arguments.
func command(env func(string) (string, bool), run func(args) error) *cobra.Command {
	a := args{given: map[string]bool{}}
	cmd := &cobra.Command{
		Use:     "kuben-agent",
		Short:   "The Kuben cluster agent: links this cluster to its hub over outbound mTLS",
		Version: version,
		Args: func(_ *cobra.Command, positional []string) error {
			if len(positional) > 0 {
				return usageError{msg: fmt.Sprintf("unexpected argument '%s' found", positional[0])}
			}
			return nil
		},
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			for _, s := range a.specs() {
				if cmd.Flags().Changed(s.name) {
					a.given[s.name] = true
				} else if v, ok := env(s.env); ok && v != "" {
					*s.to, a.given[s.name] = v, true
				}
			}
			a.given["token-stdin"] = a.tokenStdin
			if err := a.validate(); err != nil {
				return err
			}
			return run(a)
		},
	}
	for _, s := range a.specs() {
		cmd.Flags().StringVar(s.to, s.name, s.def, s.usage)
	}
	cmd.Flags().BoolVar(&a.tokenStdin, "token-stdin", false, "Read the bootstrap token from stdin, only to enroll")
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{msg: err.Error()} })
	// clap's `--version`: the name and the version.
	cmd.SetVersionTemplate("{{.Name}} {{.Version}}\n")
	return cmd
}

// validate applies clap's conflicts and requirements.
func (a *args) validate() error {
	if a.given["enrollment-dir"] {
		for _, other := range []string{"hub", "hub-ca", "cluster", "token-file", "token-stdin"} {
			if a.given[other] {
				return usageError{msg: fmt.Sprintf("the argument '--enrollment-dir' cannot be used with '--%s'", other)}
			}
		}
		return nil
	}
	if a.given["token-file"] && a.tokenStdin {
		return usageError{msg: "the argument '--token-file' cannot be used with '--token-stdin'"}
	}
	for _, required := range []string{"hub", "hub-ca", "cluster"} {
		if !a.given[required] {
			return usageError{msg: fmt.Sprintf("the following required arguments were not provided: --%s", required)}
		}
	}
	return nil
}

// tokenSource is where the bootstrap token comes from.
func (a *args) tokenSource() state.TokenSource {
	switch {
	case a.given["token-file"]:
		return state.TokenFile{Path: a.tokenFile}
	case a.tokenStdin:
		return state.TokenStdin{}
	}
	return state.NoToken{}
}

// execute runs the CLI on argv; the process's exit code.
func execute(argv []string, env func(string) (string, bool), stderr io.Writer, run func(args) error) int {
	cmd := command(env, run)
	cmd.SetArgs(argv)
	cmd.SetOut(os.Stdout)
	cmd.SetErr(stderr)
	err := cmd.Execute()
	if err == nil {
		return 0
	}
	var usage usageError
	if errors.As(err, &usage) {
		fmt.Fprintf(stderr, "error: %v\n", err) //nolint:errcheck // the exit code says it
		return 2
	}
	fmt.Fprintf(stderr, "Error: %v\n", err) //nolint:errcheck // the exit code says it
	return 1
}
