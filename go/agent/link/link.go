// Package link is the agent's end of AgentLink (ADR-027; crates/
// kuben-agent/src/link.rs): dial the hub, say hello, keep the link alive
// with heartbeats, and dial again with backoff when it drops.
//
//   - The agent always dials; the hub never connects to a cluster.
//   - A link that answers no heartbeat for three intervals is dead: the
//     agent drops it and dials again.
//   - The pause before the next dial doubles from MinBackoff up to
//     MaxBackoff, with up to half taken off at random so a hub restart is
//     not met by every cluster at once. A link that lived longer than
//     MaxBackoff starts the count again.
//   - A refusal the agent cannot fix by itself (revoked, unknown cluster, no
//     common protocol version) is retried at the longest pause, never given
//     up: an operator may fix the hub's side.
//   - Certificates are short-lived. Two thirds into its certificate's life
//     (up to a tenth earlier at random) the agent asks for a fresh one over
//     the link, stores it, and dials with it from then on ([Credentials]).
//   - Execution envelopes ([protocol.Apply]) go to the [Executor], one task
//     per target: a newer envelope for a target replaces the one still
//     running. Its observations go back over the link. Without the
//     negotiated feature, or without an executor, an envelope is rejected.
package link

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
	"sync"
	"time"

	"github.com/Teamtem-dev/kuben/go/agent/internal/clock"
	"github.com/Teamtem-dev/kuben/go/kubenapi/protocol"
)

// minHeartbeat is the shortest heartbeat interval the agent accepts from a
// hub.
const minHeartbeat = 10 * time.Millisecond

// Connector is how the agent reaches the hub: TCP in production, a stream
// of the test's own in tests.
type Connector interface {
	Connect(ctx context.Context) (net.Conn, error)
}

// TCPConnector dials Address (`host:port`) over TCP.
type TCPConnector struct {
	Address string
}

// Connect dials the hub.
func (c TCPConnector) Connect(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", c.Address) //nolint:wrapcheck // net says what failed
}

// Lifetime is when a certificate is valid.
type Lifetime struct {
	NotBefore, NotAfter time.Time
}

// Known says whether the lifetime was given (the zero Lifetime is not).
func (l Lifetime) Known() bool { return !l.NotAfter.IsZero() }

// Credentials are what the agent authenticates with. A renewal replaces
// them; the next dial uses the new certificate.
type Credentials struct {
	pinned  []*x509.Certificate
	hubName string

	mu sync.Mutex
	// tls and lifetime are guarded by mu.
	tls      *tls.Config
	lifetime Lifetime
}

// NewCredentials is identity against the pinned hub CA, for the hub named
// hubName; the lifetime of its certificate, when known (not zero), lets
// the link renew it.
func NewCredentials(pinned []*x509.Certificate, hubName string, identity tls.Certificate, lifetime Lifetime) (*Credentials, error) {
	cfg, err := protocol.AgentConfig(pinned, hubName, &identity)
	if err != nil {
		return nil, err //nolint:wrapcheck // says what failed
	}
	return &Credentials{pinned: pinned, hubName: hubName, tls: cfg, lifetime: lifetime}, nil
}

// TLS is the TLS configuration of the next dial.
func (c *Credentials) TLS() *tls.Config {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tls
}

// Lifetime is the current certificate's lifetime; false when unknown.
func (c *Credentials) Lifetime() (Lifetime, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lifetime, c.lifetime.Known()
}

// Replace uses identity from the next dial on.
func (c *Credentials) Replace(identity tls.Certificate, lifetime Lifetime) error {
	cfg, err := protocol.AgentConfig(c.pinned, c.hubName, &identity)
	if err != nil {
		return err //nolint:wrapcheck // says what failed
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tls, c.lifetime = cfg, lifetime
	return nil
}

// Renewal is renewing the certificate over the link.
type Renewal struct {
	// Key is the device key the fresh certificate is for.
	Key protocol.DeviceKey
	// Store keeps a renewed certificate (PEM), e.g. on disk, before the
	// link uses it.
	Store func(pem string) error
}

// RenewAt is when to renew: two thirds into the certificate's life, up to
// a tenth of the life earlier with jitter (0 to 1).
func RenewAt(l Lifetime, jitter float64) time.Time {
	life := l.NotAfter.Sub(l.NotBefore)
	factor := 2.0/3.0 - 0.1*min(max(jitter, 0), 1)
	return l.NotBefore.Add(time.Duration(float64(life) * factor))
}

// Executor carries out the envelopes the hub hands the agent: the cluster
// side.
type Executor interface {
	// Apply acts on apply until it is done or ctx ends, sending every
	// observation along the way to reports (with [Report]).
	Apply(ctx context.Context, apply protocol.Apply, reports chan<- protocol.Observation)
}

// Report sends o to reports unless ctx ends first; false then.
func Report(ctx context.Context, reports chan<- protocol.Observation, o protocol.Observation) bool {
	select {
	case reports <- o:
		return true
	case <-ctx.Done():
		return false
	}
}

// Config is who the agent is and how it talks to the hub.
type Config struct {
	ClusterID    string
	AgentVersion string
	Capabilities protocol.Features
	// KubernetesVersion is the cluster's version; empty when unknown.
	KubernetesVersion string
	// Credentials are the pinned hub CA, the hub's name and the agent's
	// identity.
	Credentials *Credentials
	// Renewal renews the certificate over the link; nil never renews.
	Renewal *Renewal
	// Executor carries out envelopes; nil rejects them.
	Executor               Executor
	MinBackoff, MaxBackoff time.Duration
	Logger                 *slog.Logger
	// Now is the wall clock, for when to renew.
	Now func() time.Time
}

// NewConfig is this build's version (agentVersion), no capabilities, no
// renewal, backoff from 1 second to 5 minutes.
func NewConfig(clusterID, agentVersion string, credentials *Credentials, logger *slog.Logger) Config {
	return Config{
		ClusterID: clusterID, AgentVersion: agentVersion, Capabilities: protocol.Features{}, Credentials: credentials,
		MinBackoff: time.Second, MaxBackoff: 5 * time.Minute, Logger: logger, Now: clock.Now,
	}
}

// RefusedError is the hub refusing.
type RefusedError struct {
	Reason  protocol.Refusal
	Message string
}

func (e RefusedError) Error() string {
	return fmt.Sprintf("the hub refused (%s): %s", e.Reason.Error(), e.Message)
}

// ConnectError is the hub out of reach.
type ConnectError struct{ Err error }

func (e ConnectError) Error() string { return fmt.Sprintf("cannot reach the hub: %v", e.Err) }

// Unwrap is the network's error.
func (e ConnectError) Unwrap() error { return e.Err }

// TLSError is TLS with the hub failing.
type TLSError struct{ Err error }

func (e TLSError) Error() string { return fmt.Sprintf("TLS with the hub failed: %v", e.Err) }

// Unwrap is crypto/tls' error.
func (e TLSError) Unwrap() error { return e.Err }

// UnexpectedError is the hub sending what the agent did not expect.
type UnexpectedError struct{ What string }

func (e UnexpectedError) Error() string { return "the hub sent " + e.What }

// HeartbeatTimeoutError is no heartbeat answer for After.
type HeartbeatTimeoutError struct{ After time.Duration }

func (e HeartbeatTimeoutError) Error() string {
	return fmt.Sprintf("no heartbeat answer for %s", e.After)
}

// ErrClosed is the hub closing the link.
var ErrClosed = errors.New("the hub closed the link") //nolint:gochecknoglobals // a sentinel error

var errNoRenewal = errors.New("no renewal is set up") //nolint:gochecknoglobals // a sentinel error

// IsLasting says whether err is a refusal only the hub's side can lift.
func IsLasting(err error) bool {
	refused, ok := errors.AsType[RefusedError](err)
	if !ok {
		return false
	}
	switch refused.Reason {
	case protocol.RefusalRevoked, protocol.RefusalUnknownCluster, protocol.RefusalUnsupportedProtocol:
		return true
	case protocol.RefusalUnsupportedCapability, protocol.RefusalBadRequest, protocol.RefusalInvalidToken, protocol.RefusalOther:
	}
	return false
}

// Session is what the handshake agreed.
type Session struct {
	Negotiated protocol.Negotiated
	HubVersion string
	Heartbeat  time.Duration
}

// Handshake says hello on a fresh link and reads the hub's answer.
func Handshake(conn net.Conn, cfg Config) (Session, error) {
	hello := protocol.Hello{
		ProtocolVersions: protocol.SupportedVersions.Descending(),
		AgentVersion:     cfg.AgentVersion,
		ClusterID:        cfg.ClusterID,
		Capabilities:     cfg.Capabilities,
	}
	if cfg.KubernetesVersion != "" {
		v := cfg.KubernetesVersion
		hello.KubernetesVersion = &v
	}
	if err := protocol.WriteFrame(conn, hello); err != nil {
		return Session{}, err //nolint:wrapcheck // a frame error says what failed
	}
	m, open, err := protocol.ReadFrame(conn)
	switch {
	case err != nil:
		return Session{}, err //nolint:wrapcheck // a frame error says what failed
	case !open:
		return Session{}, ErrClosed
	}
	switch m := m.(type) {
	case protocol.Welcome:
		if !protocol.SupportedVersions.Contains(m.ProtocolVersion) {
			return Session{}, UnexpectedError{What: "a protocol version the agent did not offer"}
		}
		heartbeat := time.Duration(min(m.HeartbeatMs, uint64(math.MaxInt64/int64(time.Millisecond)))) * time.Millisecond //nolint:gosec // bounded
		return Session{
			Negotiated: protocol.Negotiated{Version: m.ProtocolVersion, Features: m.Features},
			HubVersion: m.HubVersion,
			Heartbeat:  max(heartbeat, minHeartbeat),
		}, nil
	case protocol.Refused:
		return Session{}, RefusedError(m)
	case protocol.Hello, protocol.Heartbeat, protocol.HeartbeatAck, protocol.Enroll, protocol.Renew,
		protocol.Enrolled, protocol.Apply, protocol.Observation, protocol.Unknown:
	}
	return Session{}, UnexpectedError{What: "another message than Welcome"}
}

// Backoff is the pause before the next dial after failures failed ones:
// doubling from minimum, capped at maximum, with jitter (0 to 1) of up to
// half taken off.
func Backoff(failures uint32, minimum, maximum time.Duration, jitter float64) time.Duration {
	full := maximum
	if shift := min(failures, 20); minimum <= maximum>>shift {
		full = min(minimum<<shift, maximum)
	}
	return time.Duration(float64(full) * (1 - 0.5*min(max(jitter, 0), 1)))
}

// Run keeps the agent linked to the hub until ctx ends.
func Run(ctx context.Context, connector Connector, cfg Config) {
	var failures uint32
	for ctx.Err() == nil {
		dialed := time.Now() //nolint:forbidigo // a duration measurement, as tokio's Instant
		err := dialOnce(ctx, connector, cfg)
		if err == nil || ctx.Err() != nil {
			return
		}
		if time.Since(dialed) > cfg.MaxBackoff {
			failures = 0
		}
		pause := cfg.MaxBackoff
		if !IsLasting(err) {
			pause = Backoff(failures, cfg.MinBackoff, cfg.MaxBackoff, rand.Float64()) //nolint:gosec // jitter, not a secret
		}
		failures = min(failures, math.MaxUint32-1) + 1
		cfg.Logger.Warn("AgentLink down", "cluster", cfg.ClusterID, "error", err.Error(), "retry_in_ms", pause.Milliseconds())
		wait := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			wait.Stop()
			return
		case <-wait.C:
		}
	}
}

// dialOnce dials, says hello and keeps the link until it fails (an error)
// or ctx ends (nil).
func dialOnce(ctx context.Context, connector Connector, cfg Config) error {
	raw, err := connector.Connect(ctx)
	if err != nil {
		return ConnectError{Err: err}
	}
	conn := tls.Client(raw, cfg.Credentials.TLS())
	defer conn.Close() //nolint:errcheck // the link is over either way
	if err := conn.HandshakeContext(ctx); err != nil {
		return TLSError{Err: err}
	}
	session, err := Handshake(conn, cfg)
	if err != nil {
		return err
	}
	cfg.Logger.Info("AgentLink up", "cluster", cfg.ClusterID, "protocol", session.Negotiated.Version,
		"hub", session.HubVersion, "features", session.Negotiated.Features)
	return KeepAlive(ctx, conn, session, cfg)
}
