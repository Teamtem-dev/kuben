// Command kuben-agent is the Kuben cluster agent (ADR-027; crates/
// kuben-agent/src/main.rs). It runs inside the cluster, dials the hub over
// outbound mTLS and keeps that link up.
//
// On the first start it makes its device key, reads a bootstrap token
// (--token-file or --token-stdin; never an argument, so it stays out of
// process lists and shell history) and enrolls. Later starts use the
// stored certificate, which the agent renews over the link before it
// expires.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/Teamtem-dev/kuben/go/agent/bootstrap"
	"github.com/Teamtem-dev/kuben/go/agent/state"
	"github.com/Teamtem-dev/kuben/go/kubenapi/protocol"
)

// version is the release version, set by the linker; "dev" for local
// builds.
var version = "dev" //nolint:gochecknoglobals // set by the linker, never written at run time

// enrollRetry is how long to wait before enrolling again with a published
// token.
const enrollRetry = 10 * time.Second

func main() {
	// The agent stays light (plan §21): collect at half the default growth
	// unless the operator chose otherwise.
	if _, set := os.LookupEnv("GOGC"); !set {
		debug.SetGCPercent(50)
	}
	os.Exit(execute(os.Args[1:], os.LookupEnv, os.Stderr, func(a args) error {
		logger := newLogger(a.logFormat)
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return agent(ctx, a, logger)
	}))
}

// newLogger logs JSON to stdout, or text for `pretty`, at the level
// RUST_LOG names (info when it names none).
func newLogger(format string) *slog.Logger {
	var level slog.Level
	if err := level.UnmarshalText([]byte(strings.TrimSpace(os.Getenv("RUST_LOG")))); err != nil {
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	if format == "pretty" {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}

// kubeConfig is the cluster's apiserver with the agent's credentials: the
// kubeconfig, else in-cluster, as kube-rs's Client::try_default.
func kubeConfig() (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kubernetes: %w", err)
	}
	return cfg, nil
}

// target is where the agent links to, and how it enrolls.
type target struct {
	hub, hubCA, cluster string
	token               state.TokenSource
	// published by the hub: wait for a token instead of failing without one.
	published bool
}

// resolve is the target; false when ctx ended while waiting for a
// published enrollment.
func resolve(ctx context.Context, a args, logger *slog.Logger) (target, bool) {
	if a.given["enrollment-dir"] {
		e, ok := bootstrap.WaitEnrollment(ctx, a.enrollmentDir, bootstrap.Poll, logger)
		if !ok {
			return target{}, false
		}
		return target{hub: e.Hub, hubCA: e.HubCA, cluster: e.Cluster, token: state.TokenFile{Path: e.TokenFile}, published: true}, true
	}
	return target{hub: a.hub, hubCA: a.hubCA, cluster: a.cluster, token: a.tokenSource()}, true
}

// readToken is the token; a published one that is not there yet counts as
// none.
func (t target) readToken() (string, bool, error) {
	if file, ok := t.token.(state.TokenFile); ok && t.published {
		if _, err := os.Stat(file.Path); err != nil {
			return "", false, nil //nolint:nilerr // not published yet
		}
	}
	return state.ReadToken(t.token, os.Stdin) //nolint:wrapcheck // says which file
}

func agent(ctx context.Context, a args, logger *slog.Logger) error {
	restCfg, err := kubeConfig()
	if err != nil {
		return err
	}
	client, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("kubernetes: %w", err)
	}
	secret, hasSecret, err := identitySecret(a, client)
	if err != nil {
		return err
	}
	st, err := state.Open(a.stateDir)
	if err != nil {
		return err //nolint:wrapcheck // says which path
	}
	if hasSecret {
		restored, err := secret.Restore(ctx, a.stateDir)
		if err != nil {
			return err //nolint:wrapcheck // says what failed
		}
		if restored {
			logger.Info("identity restored from its Secret")
		}
	}
	key, err := st.DeviceKey()
	if err != nil {
		return err //nolint:wrapcheck // says which path
	}
	if hasSecret {
		// Kept before enrolling: a pod that dies after redeeming the token
		// resumes the enrollment with the same key.
		if err := secret.Save(ctx, a.stateDir); err != nil {
			return err //nolint:wrapcheck // says what failed
		}
	}
	t, ok := resolve(ctx, a, logger)
	if !ok {
		return nil
	}
	hubName, err := protocol.ServerName(a.hubName)
	if err != nil {
		return err //nolint:wrapcheck // says what failed
	}
	identity, t, ok, err := enroll(ctx, a, st, key, t, hubName, logger)
	if err != nil || !ok {
		return err
	}
	return linked(ctx, a, linkedDeps{
		restCfg: restCfg, client: client, secret: secret, hasSecret: hasSecret, state: st, key: key,
		target: t, hubName: hubName, identity: identity, logger: logger,
	})
}

// identitySecret is the Secret the identity is kept in, when asked for.
func identitySecret(a args, client dynamic.Interface) (bootstrap.IdentitySecret, bool, error) {
	if !a.given["identity-secret"] {
		return bootstrap.IdentitySecret{}, false, nil
	}
	namespace, ok := bootstrap.OwnNamespace()
	if !ok {
		return bootstrap.IdentitySecret{}, false, errors.New("--identity-secret needs to run in a pod")
	}
	return bootstrap.NewIdentitySecret(client, namespace, a.identitySecret), true, nil
}
