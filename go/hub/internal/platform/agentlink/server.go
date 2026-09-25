package agentlink

// AgentLink in `kuben serve` (crates/kuben-platform/src/agentlink.rs): the
// hub's endpoint for cluster agents, on the records of store/agents.go.
//
//   - [SQLTokens]: bootstrap tokens redeemed in SQL, by the database clock.
//   - [SQLRegistry]: who may link, and what is recorded of each agent; a
//     database that cannot answer refuses the agent.
//   - [ClusterCAIn]: the cluster CA kept under `<state dir>/agentlink`. Its
//     certificate is what agents pin; its private key is readable by the
//     hub's user alone and never lives in the database (ADR-030).
//   - [AgentLink]: the hub, built once per process; the listener, where
//     enrollments and linked agents share one port; and the hub the
//     materializer hands envelopes to.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"net"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/config"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
	"github.com/Teamtem-dev/kuben/go/kubenapi/protocol"
)

const (
	// CACertificate is the cluster CA's certificate, the file agents pin
	// (`--hub-ca`).
	CACertificate = "ca.crt"
	// CAKey is the cluster CA's private key.
	CAKey = "ca.key"
	// handshakeTimeout is how long a TLS handshake may take before the
	// connection is dropped.
	handshakeTimeout = 10 * time.Second
	// hubCertificate is the lifetime of the hub's own server certificate,
	// made at each start.
	hubCertificate = 30 * 24 * time.Hour
)

// Directory is the directory of the hub's AgentLink files.
func Directory(stateDir string) string { return filepath.Join(stateDir, "agentlink") }

// writeOwnerOnly writes content to path as a new file readable by its
// owner only; an existing file is replaced, so a leftover never keeps a
// wider mode.
func writeOwnerOnly(path string, content []byte) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // a path of Kuben's own
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	_, werr := f.Write(content)
	if err := errors.Join(werr, f.Close()); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ClusterCAIn is the cluster CA kept in dir, made on first use.
func ClusterCAIn(dir string, logger *slog.Logger) (ClusterCA, error) {
	certificate, key := filepath.Join(dir, CACertificate), filepath.Join(dir, CAKey)
	if exists(certificate) && exists(key) {
		certPEM, err := os.ReadFile(certificate) //nolint:gosec // a path of Kuben's own
		if err != nil {
			return ClusterCA{}, fmt.Errorf("%s: %w", certificate, err)
		}
		keyPEM, err := os.ReadFile(key) //nolint:gosec // a path of Kuben's own
		if err != nil {
			return ClusterCA{}, fmt.Errorf("%s: %w", key, err)
		}
		return ClusterCAFromPEM(string(certPEM), string(keyPEM))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ClusterCA{}, fmt.Errorf("%s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // a directory only its owner enters
		return ClusterCA{}, fmt.Errorf("%s: %w", dir, err)
	}
	ca, err := GenerateClusterCA("Kuben AgentLink CA")
	if err != nil {
		return ClusterCA{}, err
	}
	if err := writeOwnerOnly(key, []byte(ca.KeyPEM())); err != nil {
		return ClusterCA{}, err
	}
	if err := os.WriteFile(certificate, []byte(ca.CertificatePEM()), 0o644); err != nil { //nolint:gosec // the certificate agents pin is public
		return ClusterCA{}, fmt.Errorf("%s: %w", certificate, err)
	}
	logger.Info("AgentLink CA made", "dir", dir)
	return ca, nil
}

// clusterID is a cluster id as the agent protocol carries it.
func clusterID(text string) (ids.ClusterID, bool) {
	id, err := ids.Parse[ids.Cluster](text)
	return id, err == nil
}

// SQLTokens are bootstrap tokens in SQL.
type SQLTokens struct {
	Store  *store.Store
	Logger *slog.Logger
}

// Redeem redeems a token by the database clock (now is not used).
func (s SQLTokens) Redeem(ctx context.Context, hash [32]byte, cluster, device string, _ time.Time) (Redeemed, error) {
	id, ok := clusterID(cluster)
	if !ok {
		return 0, TokenUnknown
	}
	redeemed, err := s.Store.RedeemAgentToken(ctx, hash, id, device, ResumeGrace)
	if refusal, isRefusal := errors.AsType[store.TokenRefusal](err); isRefusal {
		switch refusal {
		case store.TokenUnknown:
			return 0, TokenUnknown
		case store.TokenExpired:
			return 0, TokenExpired
		case store.TokenOtherCluster:
			return 0, TokenOtherCluster
		case store.TokenOtherDevice:
			return 0, TokenOtherDevice
		}
		return 0, TokenUnknown
	}
	if err != nil {
		s.Logger.Warn("cannot redeem an agent token", "error", err)
		return 0, TokenUnknown
	}
	switch redeemed.Kind {
	case store.TokenFirst:
		return RedeemedFirst, nil
	case store.TokenResumed:
		return RedeemedResumed, nil
	}
	return RedeemedFirst, nil
}

// SQLRegistry is what the hub knows of each cluster's agent, in SQL.
type SQLRegistry struct {
	Store  *store.Store
	Logger *slog.Logger
}

// Admits reads the cluster's agent; a database that cannot answer refuses.
func (r SQLRegistry) Admits(ctx context.Context, cluster, device string) bool {
	id, ok := clusterID(cluster)
	if !ok {
		return false
	}
	agent, found, err := r.Store.ClusterAgent(ctx, id)
	if err != nil {
		r.Logger.Warn("cannot read the cluster's agent; refusing", "error", err, "cluster", cluster)
		return false
	}
	return found && agent.Accepts(device)
}

// Certified records the certificate in the organization of the cluster's
// agent, or of the token the device redeemed.
func (r SQLRegistry) Certified(ctx context.Context, cluster, device string, notAfter time.Time) {
	id, ok := clusterID(cluster)
	if !ok {
		return
	}
	org := opt.None[ids.OrgID]()
	agent, found, err := r.Store.ClusterAgent(ctx, id)
	switch {
	case err != nil:
		r.Logger.Warn("cannot read the cluster's agent", "error", err, "cluster", cluster)
	case found:
		org = opt.Some(agent.Org)
	default:
		if o, named, err := r.Store.AgentTokenOrg(ctx, id, device); err == nil && named {
			org = opt.Some(o)
		}
	}
	o, ok := org.Get()
	if !ok {
		r.Logger.Warn("a certificate for a cluster no redeemed token names", "cluster", cluster)
		return
	}
	if err := r.Store.RecordAgentCertificate(ctx, o, id, device, notAfter.UnixMilli()); err != nil {
		r.Logger.Warn("cannot record the agent's certificate", "error", err, "cluster", cluster)
	}
}

// Linked records the link.
func (r SQLRegistry) Linked(ctx context.Context, cluster, device string, session SessionInfo) {
	id, ok := clusterID(cluster)
	if !ok {
		return
	}
	recorded, err := r.Store.RecordAgentLink(ctx, id, device, session.Version, session.Features, session.AgentVersion)
	switch {
	case err != nil:
		r.Logger.Warn("cannot record the agent's link", "error", err, "cluster", cluster)
	case recorded:
		r.Logger.Info("agent linked", "cluster", cluster, "protocol", session.Version, "agent", session.AgentVersion)
	default:
		r.Logger.Warn("a link of a device that is not the cluster's agent", "cluster", cluster)
	}
}

// Observed records the observation under the cluster whose agent sent it.
func (r SQLRegistry) Observed(ctx context.Context, cluster, _ string, o protocol.Observation) {
	id, ok := clusterID(cluster)
	target, err := ids.Parse[ids.Target](o.Target)
	if !ok || err != nil {
		return
	}
	agent, found, err := r.Store.ClusterAgent(ctx, id)
	switch {
	case err != nil:
		r.Logger.Warn("cannot read the cluster's agent", "error", err, "cluster", cluster)
		return
	case !found:
		return
	}
	phase := string(o.Phase)
	recorded, err := r.record(ctx, agent.Org, id, target, o)
	switch {
	case err != nil:
		r.Logger.Warn("cannot record the agent's observation", "error", err, "cluster", cluster)
	case recorded:
		r.Logger.Info("agent observation", "cluster", cluster, "target", o.Target, "generation", o.Generation, "phase", phase)
	default:
		r.Logger.Warn("an observation of a target not on this cluster, or of an older generation",
			"cluster", cluster, "target", o.Target)
	}
}

func (r SQLRegistry) record(ctx context.Context, org ids.OrgID, cluster ids.ClusterID, target ids.TargetID, o protocol.Observation) (_ bool, err error) {
	t, err := r.Store.Tenant(ctx, org)
	if err != nil {
		return false, err //nolint:wrapcheck // a store error says what failed
	}
	defer func() { err = errors.Join(err, t.Rollback(ctx)) }()
	recorded, err := t.RecordRuntimeObservation(ctx, cluster, target, o.Generation, string(o.Phase),
		opt.FromPtr(o.Reason), opt.FromPtr(o.Message))
	if err != nil {
		return false, err //nolint:wrapcheck // a store error says what failed
	}
	return recorded, t.Commit(ctx) //nolint:wrapcheck // a store error says what failed
}

// Heard notes the heartbeat.
func (r SQLRegistry) Heard(ctx context.Context, cluster, device string) {
	id, ok := clusterID(cluster)
	if !ok {
		return
	}
	if err := r.Store.TouchAgent(ctx, id, device); err != nil {
		r.Logger.Debug("cannot record the agent's heartbeat", "error", err, "cluster", cluster)
	}
}

// AgentLink is AgentLink of this process: the hub, built once, and its
// listener.
type AgentLink struct {
	hub     *Hub
	tls     *tls.Config
	bind    string
	hubName string
	logger  *slog.Logger
}

// saturatingHours is n hours, the longest duration when that overflows.
func saturatingHours(n uint64) time.Duration {
	if n > uint64(math.MaxInt64/int64(time.Hour)) {
		return math.MaxInt64
	}
	return time.Duration(n) * time.Hour //nolint:gosec // bounded above
}

// Build is the hub for cfg, issuing under the cluster CA kept in stateDir;
// false while `agent.bind` is unset.
func Build(cfg config.AgentCfg, stateDir string, st *store.Store, clk clock.Clock, logger *slog.Logger) (*AgentLink, bool, error) {
	bind, ok := cfg.Bind.Get()
	if !ok {
		return nil, false, nil
	}
	ca, err := ClusterCAIn(Directory(stateDir), logger)
	if err != nil {
		return nil, false, err
	}
	identity, err := ca.ServerIdentity(cfg.HubName, hubCertificate, time.UnixMilli(clk.NowMs()))
	if err != nil {
		return nil, false, err
	}
	tlsConfig, err := protocol.HubConfig([]*x509.Certificate{ca.Certificate()}, identity, protocol.ClientAuthEnrollmentAllowed)
	if err != nil {
		return nil, false, err //nolint:wrapcheck // says what failed
	}
	settings := DefaultHubSettings()
	settings.Heartbeat = clock.Seconds(max(cfg.HeartbeatSecs, 1))
	settings.Features = protocol.NewFeatures(protocol.ApplicationRuntime)
	enrollment := NewEnrollment(ca, SQLTokens{Store: st, Logger: logger}, saturatingHours(max(cfg.CertificateHours, 1)))
	hub := NewHub(enrollment, SQLRegistry{Store: st, Logger: logger}, settings, clk)
	return &AgentLink{hub: hub, tls: tlsConfig, bind: bind, hubName: cfg.HubName, logger: logger}, true, nil
}

// Hub is the hub, the handle the materializer hands envelopes to.
func (a *AgentLink) Hub() *Hub { return a.hub }

// Serve listens for agents until ctx ends.
func (a *AgentLink) Serve(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", a.bind)
	if err != nil {
		return fmt.Errorf("cannot listen for agents on %s: %w", a.bind, err)
	}
	a.logger.Info("AgentLink listening", "bind", a.bind, "hub", a.hubName)
	var conns errgroup.Group
	stop := context.AfterFunc(ctx, func() { ln.Close() }) //nolint:errcheck,gosec // unblocks Accept
	defer stop()
	defer conns.Wait() //nolint:errcheck // every connection returns nil
	for {
		tcp, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			a.logger.Warn("AgentLink accept failed", "error", err)
			continue
		}
		conns.Go(func() error {
			a.serveConn(ctx, tcp)
			return nil
		})
	}
}

func (a *AgentLink) serveConn(ctx context.Context, tcp net.Conn) {
	peer := tcp.RemoteAddr().String()
	conn := tls.Server(tcp, a.tls)
	handshake, cancel := context.WithTimeout(ctx, handshakeTimeout)
	err := conn.HandshakeContext(handshake)
	cancel()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			a.logger.Debug("AgentLink TLS handshake timed out", "peer", peer)
		} else {
			a.logger.Debug("AgentLink TLS refused", "peer", peer, "error", err)
		}
		conn.Close() //nolint:errcheck,gosec // refused
		return
	}
	if err := a.hub.Serve(ctx, conn); err != nil {
		a.logger.Debug("AgentLink connection ended", "peer", peer, "error", err)
	}
}
