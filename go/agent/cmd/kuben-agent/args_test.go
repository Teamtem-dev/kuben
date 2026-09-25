package main

import (
	"io"
	"slices"
	"testing"

	"github.com/Teamtem-dev/kuben/go/agent/state"
	"github.com/Teamtem-dev/kuben/go/kubenapi/protocol"
)

// Ported from crates/kuben-agent/src/main.rs.

var base = []string{"--hub", "hub.example.com:7443", "--hub-ca", "ca.pem", "--cluster", "primary"}

func noEnv(string) (string, bool) { return "", false }

// parse runs the CLI on argv; the parsed arguments, or the exit code.
func parse(argv []string, env func(string) (string, bool)) (args, int) {
	var parsed args
	code := execute(argv, env, io.Discard, func(a args) error {
		parsed = a
		return nil
	})
	return parsed, code
}

func with(extra ...string) []string { return append(slices.Clone(base), extra...) }

func TestTheTokenIsNeverAnArgument(t *testing.T) {
	if _, code := parse(with("--token", "kbt_secret"), noEnv); code != 2 {
		t.Fatalf("exit %d", code)
	}
}

func TestAnEnrollmentDirectoryReplacesTheHubFlags(t *testing.T) {
	a, code := parse([]string{"--enrollment-dir", "/etc/kuben-agent/enrollment"}, noEnv)
	if code != 0 || a.enrollmentDir != "/etc/kuben-agent/enrollment" {
		t.Fatal(code, a.enrollmentDir)
	}
	if _, code := parse(nil, noEnv); code != 2 {
		t.Fatal("a hub is needed")
	}
	if _, code := parse(with("--enrollment-dir", "/x"), noEnv); code != 2 {
		t.Fatal("one or the other")
	}
	// A value from the environment counts as given, as with clap.
	env := func(name string) (string, bool) {
		return map[string]string{"KUBEN_AGENT_ENROLLMENT_DIR": "/e", "KUBEN_AGENT_HUB": "h:1"}[name], name == "KUBEN_AGENT_ENROLLMENT_DIR" || name == "KUBEN_AGENT_HUB"
	}
	if _, code := parse(nil, env); code != 2 {
		t.Fatal("an enrollment directory and a hub from the environment")
	}
}

func TestTheTokenComesFromOneFileOrStdin(t *testing.T) {
	a, code := parse(base, noEnv)
	if code != 0 || a.tokenSource() != (state.NoToken{}) || a.hubName != protocol.HubName ||
		a.stateDir != "/var/lib/kuben-agent" || a.logFormat != "json" {
		t.Fatalf("%d %+v", code, a)
	}
	a, code = parse(with("--token-file", "/run/secrets/token"), noEnv)
	if code != 0 || a.tokenSource() != (state.TokenFile{Path: "/run/secrets/token"}) {
		t.Fatalf("%d %+v", code, a.tokenSource())
	}
	if _, code := parse(with("--token-file", "t", "--token-stdin"), noEnv); code != 2 {
		t.Fatal("one source only")
	}
	a, code = parse(with("--token-stdin"), noEnv)
	if code != 0 || a.tokenSource() != (state.TokenStdin{}) {
		t.Fatalf("%d %+v", code, a.tokenSource())
	}
}
