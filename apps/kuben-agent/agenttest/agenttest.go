// Package agenttest opens the agent to tests outside this module. The
// agent's packages are internal to it, which keeps the hub from building on
// them; but the hub's tests run the real agent against the real hub (the
// AgentLink end-to-end tests of apps/kuben/internal/agentlink) and hold the
// hub's envelopes to the agent's own check (apps/kuben/internal/kube/
// materializer). This package is the one door for them: aliases and thin
// wrappers over the agent's link, state and runtime, and nothing that is
// not already the agent's behaviour. Only tests import it.
package agenttest

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"time"

	"github.com/Teamtem-dev/kuben/apps/kuben-agent/internal/link"
	"github.com/Teamtem-dev/kuben/apps/kuben-agent/internal/runtime"
	"github.com/Teamtem-dev/kuben/apps/kuben-agent/internal/state"
	"github.com/Teamtem-dev/kuben/packages/api/agentlink"
)

// Config is who the agent is and how it talks to the hub (link.Config).
type Config = link.Config

// Connector opens the stream to the hub (link.Connector).
type Connector = link.Connector

// Credentials are the pinned hub CA, the hub's name and the agent's
// identity (link.Credentials).
type Credentials = link.Credentials

// Lifetime is the validity of the agent's certificate (link.Lifetime).
type Lifetime = link.Lifetime

// Renewal renews the certificate over the link (link.Renewal).
type Renewal = link.Renewal

// State is the agent's state directory (state.State).
type State = state.State

// HubAddress is how the agent reaches the hub to enroll (state.HubAddress).
type HubAddress = state.HubAddress

// Checked is an envelope that passed the agent's check (runtime.Checked).
type Checked = runtime.Checked

// ErrNeedsEnrollment is state.ErrNeedsEnrollment.
var ErrNeedsEnrollment = state.ErrNeedsEnrollment //nolint:gochecknoglobals // the agent's sentinel error

// NewCredentials is link.NewCredentials.
func NewCredentials(pinned []*x509.Certificate, hubName string, identity tls.Certificate, lifetime Lifetime) (*Credentials, error) {
	return link.NewCredentials(pinned, hubName, identity, lifetime)
}

// NewConfig is link.NewConfig.
func NewConfig(clusterID, agentVersion string, credentials *Credentials, logger *slog.Logger) Config {
	return link.NewConfig(clusterID, agentVersion, credentials, logger)
}

// Run is link.Run: the agent's link until ctx ends.
func Run(ctx context.Context, connector Connector, cfg Config) {
	link.Run(ctx, connector, cfg)
}

// Report is link.Report.
func Report(ctx context.Context, reports chan<- agentlink.Observation, o agentlink.Observation) bool {
	return link.Report(ctx, reports, o)
}

// OpenState is state.Open.
func OpenState(dir string) (State, error) {
	return state.Open(dir)
}

// EnsureIdentity is state.EnsureIdentity.
func EnsureIdentity(ctx context.Context, s State, key agentlink.DeviceKey, hub HubAddress, clusterID string,
	token func() (string, bool, error), now time.Time,
) (tls.Certificate, error) {
	return state.EnsureIdentity(ctx, s, key, hub, clusterID, token, now)
}

// Check is runtime.Check: the agent's check of an envelope before anything
// is written.
func Check(apply agentlink.Apply) (Checked, error) {
	return runtime.Check(apply)
}
