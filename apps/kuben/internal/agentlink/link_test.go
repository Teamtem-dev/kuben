package agentlink_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Teamtem-dev/kuben/apps/kuben-agent/agenttest"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/agentlink"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/clock"
	protocol "github.com/Teamtem-dev/kuben/packages/api/agentlink"
)

// Ported from crates/kuben-agent/tests/link.rs: AgentLink end to end, the
// agent's link loop (apps/kuben-agent/internal/link, through agenttest)
// against the hub over TLS on loopback TCP.

const agentVersion = "2.0.0-test"

type world struct {
	hub      *agentlink.Hub
	tokens   *memoryTokens
	registry *memoryRegistry
	tls      *tls.Config
	pinned   *x509.Certificate
}

func newWorld(t *testing.T, settings agentlink.HubSettings) *world {
	return newWorldWith(t, settings, newMemoryRegistry(0))
}

func newWorldWith(t *testing.T, settings agentlink.HubSettings, registry *memoryRegistry) *world {
	t.Helper()
	ca := newCA(t)
	identity, err := ca.ServerIdentity(protocol.HubName, day, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := protocol.HubConfig([]*x509.Certificate{ca.Certificate()}, identity, protocol.ClientAuthEnrollmentAllowed)
	if err != nil {
		t.Fatal(err)
	}
	tokens := newMemoryTokens()
	hub := agentlink.NewHub(agentlink.NewEnrollment(ca, tokens, day), registry, settings, clock.System{})
	return &world{hub: hub, tokens: tokens, registry: registry, tls: cfg, pinned: ca.Certificate()}
}

// dialer dials the hub; the first failFirst dials fail as refused
// connections.
type dialer struct {
	address   string
	dials     atomic.Uint32
	failFirst uint32
}

func (d *dialer) Connect(ctx context.Context) (net.Conn, error) {
	if n := d.dials.Add(1); n <= d.failFirst {
		return nil, syscall.ECONNREFUSED
	}
	var nd net.Dialer
	return nd.DialContext(ctx, "tcp", d.address)
}

// serveHub serves the hub on a loopback listener; its address.
func (w *world) serveHub(t *testing.T) string {
	return listenTLS(t, w.tls, func(ctx context.Context, conn *tls.Conn) { _ = w.hub.Serve(ctx, conn) })
}

// listenTLS is listen with the TLS server side done by crypto/tls.
func listenTLS(t *testing.T, cfg *tls.Config, serve func(ctx context.Context, conn *tls.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var conns sync.WaitGroup
	conns.Go(func() {
		for {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			conns.Go(func() {
				conn := tls.Server(raw, cfg)
				defer conn.Close()
				stop := context.AfterFunc(ctx, func() { conn.Close() })
				defer stop()
				if err := conn.HandshakeContext(ctx); err != nil {
					return
				}
				serve(ctx, conn)
			})
		}
	})
	t.Cleanup(func() {
		cancel()
		ln.Close()
		conns.Wait()
	})
	return ln.Addr().String()
}

func (w *world) dialer(t *testing.T, failFirst uint32) *dialer {
	return &dialer{address: w.serveHub(t), failFirst: failFirst}
}

// issuedIdentity is an identity the hub's CA issued for cluster, recorded
// as the cluster's agent device (as enrollment records it).
func (w *world) issuedIdentity(t *testing.T, cluster string) tls.Certificate {
	t.Helper()
	device := deviceKey(t)
	csr, err := protocol.ParseCSR(csrOf(t, device, cluster))
	if err != nil {
		t.Fatal(err)
	}
	issued, err := w.hub.Enrollment().CA().Issue(cluster, csr, day, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	w.registry.Certified(t.Context(), cluster, issued.DeviceID, issued.NotAfter)
	identity, err := device.Identity(issued.CertificatePEM)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func (w *world) config(t *testing.T, cluster string, identity tls.Certificate) agenttest.Config {
	t.Helper()
	credentials, err := agenttest.NewCredentials([]*x509.Certificate{w.pinned}, protocol.HubName, identity, agenttest.Lifetime{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := agenttest.NewConfig(cluster, agentVersion, credentials, quiet())
	cfg.MinBackoff, cfg.MaxBackoff = 10*time.Millisecond, 50*time.Millisecond
	return cfg
}

// eventually waits up to ten seconds for check.
func eventually(t *testing.T, what string, check func() bool) {
	t.Helper()
	for range 1000 {
		if check() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// start runs the link loop until the returned stop is called, which waits
// for it to end.
func start(t *testing.T, connector agenttest.Connector, cfg agenttest.Config) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		agenttest.Run(ctx, connector, cfg)
	}()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the link loop did not end when cancelled")
		}
	}
	t.Cleanup(stop)
	return stop
}

func (w *world) heartbeats(cluster string) uint64 {
	s, ok := w.hub.Session(cluster)
	if !ok {
		return 0
	}
	return s.Heartbeats
}

func quick() agentlink.HubSettings {
	s := agentlink.DefaultHubSettings()
	s.Heartbeat = 20 * time.Millisecond
	return s
}

func TestAnEnrolledAgentLinksAndKeepsTheLinkAlive(t *testing.T) {
	settings := quick()
	settings.Features = protocol.NewFeatures("applicationRuntime", "executionTask")
	w := newWorld(t, settings)
	cfg := w.config(t, "primary", w.issuedIdentity(t, "primary"))
	cfg.Capabilities = protocol.NewFeatures("executionTask", "logs")
	stop := start(t, w.dialer(t, 0), cfg)
	eventually(t, "three heartbeats", func() bool { return w.heartbeats("primary") >= 3 })
	session, ok := w.hub.Session("primary")
	if !ok || !slices.Equal(session.Features, protocol.NewFeatures("executionTask")) {
		t.Fatalf("features both know: %+v", session)
	}
	if session.AgentVersion != agentVersion {
		t.Fatal(session.AgentVersion)
	}
	stop()
}

func TestTheAgentDialsAgainAfterFailedDials(t *testing.T) {
	w := newWorld(t, quick())
	d := w.dialer(t, 2)
	stop := start(t, d, w.config(t, "primary", w.issuedIdentity(t, "primary")))
	eventually(t, "a link after two refused dials", func() bool { return w.heartbeats("primary") >= 1 })
	if d.dials.Load() < 3 {
		t.Fatalf("%d dials", d.dials.Load())
	}
	stop()
}

func TestARevokedDeviceIsRefusedAndTheAgentKeepsTrying(t *testing.T) {
	w := newWorld(t, agentlink.DefaultHubSettings())
	identity := w.issuedIdentity(t, "primary")
	w.registry.revoke("primary")
	d := w.dialer(t, 0)
	stop := start(t, d, w.config(t, "primary", identity))
	eventually(t, "three dials", func() bool { return d.dials.Load() >= 3 })
	if _, ok := w.hub.Session("primary"); ok {
		t.Fatal("never linked")
	}
	stop()
}

func TestADeviceANewerEnrollmentReplacedIsRefused(t *testing.T) {
	w := newWorld(t, agentlink.DefaultHubSettings())
	old := w.issuedIdentity(t, "primary")
	_ = w.issuedIdentity(t, "primary")
	d := w.dialer(t, 0)
	stop := start(t, d, w.config(t, "primary", old))
	eventually(t, "two dials", func() bool { return d.dials.Load() >= 2 })
	if _, ok := w.hub.Session("primary"); ok {
		t.Fatal("a still valid certificate of a replaced device")
	}
	stop()
}

func TestAHelloForAnotherClusterThanTheCertificateIsRefused(t *testing.T) {
	w := newWorld(t, agentlink.DefaultHubSettings())
	d := w.dialer(t, 0)
	stop := start(t, d, w.config(t, "other", w.issuedIdentity(t, "primary")))
	eventually(t, "two dials", func() bool { return d.dials.Load() >= 2 })
	for _, cluster := range []string{"other", "primary"} {
		if _, ok := w.hub.Session(cluster); ok {
			t.Fatal(cluster, "linked")
		}
	}
	stop()
}

func TestAnAgentWithoutACommonProtocolVersionIsRefused(t *testing.T) {
	settings := agentlink.DefaultHubSettings()
	settings.Versions = protocol.Versions{Min: 90, Max: 91}
	w := newWorld(t, settings)
	d := w.dialer(t, 0)
	stop := start(t, d, w.config(t, "primary", w.issuedIdentity(t, "primary")))
	eventually(t, "two dials", func() bool { return d.dials.Load() >= 2 })
	if _, ok := w.hub.Session("primary"); ok {
		t.Fatal("linked")
	}
	stop()
}

func TestAHubThatStopsAnsweringIsLeftAndDialedAgain(t *testing.T) {
	w := newWorld(t, agentlink.DefaultHubSettings())
	// A hub that welcomes the agent and then never answers.
	silent := &dialer{address: listenTLS(t, w.tls, func(_ context.Context, conn *tls.Conn) {
		_, _, _ = protocol.ReadFrame(conn)
		welcome := protocol.Welcome{ProtocolVersion: 1, HubVersion: "silent", HeartbeatMs: 20, Features: protocol.Features{}}
		if protocol.WriteFrame(conn, welcome) != nil {
			return
		}
		for {
			if _, open, err := protocol.ReadFrame(conn); err != nil || !open {
				return
			}
		}
	})}
	stop := start(t, silent, w.config(t, "primary", w.issuedIdentity(t, "primary")))
	eventually(t, "a second dial after the heartbeat timeout", func() bool { return silent.dials.Load() >= 2 })
	stop()
}

// enrollThroughTheHub enrolls device over an anonymous TLS connection.
func (w *world) enrollThroughTheHub(t *testing.T, d *dialer, token string, device protocol.DeviceKey) protocol.Enrolled {
	t.Helper()
	raw, err := d.Connect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	anonymous, err := protocol.AgentConfig([]*x509.Certificate{w.pinned}, protocol.HubName, nil)
	if err != nil {
		t.Fatal(err)
	}
	conn := tls.Client(raw, anonymous)
	defer conn.Close()
	if err := conn.HandshakeContext(t.Context()); err != nil {
		t.Fatal("TLS without a client certificate:", err)
	}
	enrolled, err := protocol.RequestEnrollment(conn, token, "primary", device)
	if err != nil {
		t.Fatal("enrolled through the hub:", err)
	}
	return enrolled
}

func TestAnAgentEnrollsThroughTheHubAndThenLinks(t *testing.T) {
	w := newWorld(t, quick())
	token := w.tokens.issue("primary", 30*time.Minute, time.Now())
	d := w.dialer(t, 0)
	device := deviceKey(t)
	enrolled := w.enrollThroughTheHub(t, d, token, device)
	identity, err := device.Identity(enrolled.Certificate)
	if err != nil {
		t.Fatal(err)
	}
	stop := start(t, d, w.config(t, "primary", identity))
	eventually(t, "a link with the enrolled identity", func() bool { return w.heartbeats("primary") >= 1 })
	stop()
}

// The hub records a new device before it hands over the certificate. In CI
// (run 34979693959) the SQL registry was still writing when the freshly
// enrolled agent said Hello; the hub refused it as revoked and the agent
// waited five minutes. With a registry that takes its time, the agent links
// on its first dial after enrolling.
func TestAFreshlyEnrolledAgentLinksAtOnceEvenWhenRecordingIsSlow(t *testing.T) {
	w := newWorldWith(t, quick(), newMemoryRegistry(300*time.Millisecond))
	token := w.tokens.issue("primary", 30*time.Minute, time.Now())
	d := w.dialer(t, 0)
	device := deviceKey(t)
	enrolled := w.enrollThroughTheHub(t, d, token, device)
	identity, err := device.Identity(enrolled.Certificate)
	if err != nil {
		t.Fatal(err)
	}
	stop := start(t, d, w.config(t, "primary", identity))
	eventually(t, "a link with the enrolled identity", func() bool { return w.heartbeats("primary") >= 1 })
	if n := d.dials.Load(); n != 2 {
		t.Fatalf("the enrollment and one link: the first Hello was admitted; %d dials", n)
	}
	stop()
}

func TestTheAgentEnrollsOnceAndThenUsesItsStoredIdentity(t *testing.T) {
	w := newWorld(t, quick())
	now := time.Now()
	token := w.tokens.issue("primary", 30*time.Minute, now)
	st, err := agenttest.OpenState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key, err := st.DeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	d := w.dialer(t, 0)
	hub := agenttest.HubAddress{Connector: d, Pinned: []*x509.Certificate{w.pinned}, Name: protocol.HubName, Logger: quiet()}
	ctx := t.Context()
	none := func() (string, bool, error) { return "", false, nil }
	if _, err := agenttest.EnsureIdentity(ctx, st, key, hub, "primary", none, now); !errors.Is(err, agenttest.ErrNeedsEnrollment) {
		t.Fatal(err)
	}
	withToken := func() (string, bool, error) { return token, true, nil }
	if _, err := agenttest.EnsureIdentity(ctx, st, key, hub, "primary", withToken, now); err != nil {
		t.Fatal("enrolled:", err)
	}
	never := func() (string, bool, error) { return "", false, errors.New("an enrolled agent never reads a token") }
	stored, err := agenttest.EnsureIdentity(ctx, st, key, hub, "primary", never, now)
	if err != nil {
		t.Fatal("the stored identity:", err)
	}
	if _, err := agenttest.EnsureIdentity(ctx, st, key, hub, "primary", none, now.Add(2*day)); !errors.Is(err, agenttest.ErrNeedsEnrollment) {
		t.Fatal("an expired certificate:", err)
	}
	stop := start(t, d, w.config(t, "primary", stored))
	eventually(t, "a link with the stored identity", func() bool { return w.heartbeats("primary") >= 1 })
	stop()
}

func TestAnAgentRenewsItsCertificateOverTheLinkOnce(t *testing.T) {
	w := newWorld(t, quick())
	key := deviceKey(t)
	now := time.Now()
	csr, err := protocol.ParseCSR(csrOf(t, key, "primary"))
	if err != nil {
		t.Fatal(err)
	}
	// A minute of life left: the renewal is due at once.
	issued, err := w.hub.Enrollment().CA().Issue("primary", csr, time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	w.registry.Certified(t.Context(), "primary", issued.DeviceID, issued.NotAfter)
	identity, err := key.Identity(issued.CertificatePEM)
	if err != nil {
		t.Fatal(err)
	}
	first := agenttest.Lifetime{NotBefore: now.Add(-5 * time.Minute), NotAfter: issued.NotAfter}
	credentials, err := agenttest.NewCredentials([]*x509.Certificate{w.pinned}, protocol.HubName, identity, first)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var stored []string
	cfg := agenttest.NewConfig("primary", agentVersion, credentials, quiet())
	cfg.MinBackoff, cfg.MaxBackoff = 10*time.Millisecond, 50*time.Millisecond
	cfg.Renewal = &agenttest.Renewal{Key: key, Store: func(pem string) error {
		mu.Lock()
		defer mu.Unlock()
		stored = append(stored, pem)
		return nil
	}}
	storedCount := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(stored)
	}
	stop := start(t, w.dialer(t, 0), cfg)
	eventually(t, "a renewed certificate", func() bool { return storedCount() > 0 })
	renewed, known := credentials.Lifetime()
	if !known || !renewed.NotAfter.After(first.NotAfter.Add(time.Hour)) {
		t.Fatal("the hub's day-long certificate replaced the old one", renewed)
	}
	atRenewal := w.heartbeats("primary")
	eventually(t, "more heartbeats", func() bool { return w.heartbeats("primary") >= atRenewal+5 })
	if n := storedCount(); n != 1 {
		t.Fatalf("renewed once, not again: %d", n)
	}
	stop()
}

// fake is an executor that records envelopes and reports them ready.
type fake struct {
	mu      sync.Mutex
	applied []protocol.Apply
}

func (f *fake) Apply(ctx context.Context, apply protocol.Apply, reports chan<- protocol.Observation) {
	f.mu.Lock()
	f.applied = append(f.applied, apply)
	f.mu.Unlock()
	var spec struct {
		Generation int64 `json:"generation"`
	}
	_ = json.Unmarshal([]byte(apply.Spec), &spec)
	for _, phase := range []protocol.RuntimePhase{protocol.RuntimePhaseAccepted, protocol.RuntimePhaseReady} {
		agenttest.Report(ctx, reports, protocol.Observation{Target: apply.Target, Generation: spec.Generation, Phase: phase})
	}
}

func (f *fake) specs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, a := range f.applied {
		out = append(out, a.Spec)
	}
	return out
}

func envelope(generation int) protocol.Apply {
	spec, _ := json.Marshal(map[string]int{"generation": generation})
	return protocol.Apply{Target: "0199a0c0-0000-7000-8000-000000000001", Namespace: "kb-shop-prod", Name: "web", Spec: string(spec)}
}

func (w *world) ready(cluster string, generation int64) bool {
	return slices.ContainsFunc(w.registry.observed(cluster), func(o protocol.Observation) bool {
		return o.Phase == protocol.RuntimePhaseReady && o.Generation == generation
	})
}

func TestTheHubHandsAnAgentAnEnvelopeAndHearsHowItWent(t *testing.T) {
	settings := quick()
	settings.Features = protocol.NewFeatures(protocol.ApplicationRuntime)
	w := newWorld(t, settings)
	f := &fake{}
	cfg := w.config(t, "primary", w.issuedIdentity(t, "primary"))
	cfg.Capabilities = protocol.NewFeatures(protocol.ApplicationRuntime)
	cfg.Executor = f
	stop := start(t, w.dialer(t, 0), cfg)
	eventually(t, "a link", func() bool { return w.heartbeats("primary") >= 1 })
	if !w.hub.Send(t.Context(), "primary", envelope(5)) {
		t.Fatal("sent over the live link")
	}
	eventually(t, "a ready observation", func() bool { return w.ready("primary", 5) })
	if n := len(f.specs()); n != 1 {
		t.Fatal(n)
	}
	if w.hub.Send(t.Context(), "elsewhere", envelope(5)) {
		t.Fatal("no link to that cluster")
	}
	stop()
}

// Two clusters on one hub, the contract harness of plan §18.1: each agent
// receives only its own cluster's envelopes, and what it reports is filed
// under its own cluster.
func TestTwoClustersOnOneHubEachHearOnlyTheirOwnEnvelopes(t *testing.T) {
	settings := quick()
	settings.Features = protocol.NewFeatures(protocol.ApplicationRuntime)
	w := newWorld(t, settings)
	primary, secondary := &fake{}, &fake{}
	for cluster, f := range map[string]*fake{"primary": primary, "secondary": secondary} {
		cfg := w.config(t, cluster, w.issuedIdentity(t, cluster))
		cfg.Capabilities = protocol.NewFeatures(protocol.ApplicationRuntime)
		cfg.Executor = f
		start(t, w.dialer(t, 0), cfg)
	}
	eventually(t, "both links", func() bool { return w.heartbeats("primary") >= 1 && w.heartbeats("secondary") >= 1 })
	if !w.hub.Send(t.Context(), "primary", envelope(5)) || !w.hub.Send(t.Context(), "secondary", envelope(7)) {
		t.Fatal("sent")
	}
	eventually(t, "both envelopes ready", func() bool { return w.ready("primary", 5) && w.ready("secondary", 7) })
	if got := primary.specs(); !slices.Equal(got, []string{`{"generation":5}`}) {
		t.Fatal("primary got only its own:", got)
	}
	if got := secondary.specs(); !slices.Equal(got, []string{`{"generation":7}`}) {
		t.Fatal("secondary got only its own:", got)
	}
	if slices.ContainsFunc(w.registry.observed("primary"), func(o protocol.Observation) bool { return o.Generation == 7 }) {
		t.Fatal("secondary's reports are not filed under primary")
	}
}

func TestEnvelopesNeedTheNegotiatedFeature(t *testing.T) {
	w := newWorld(t, quick())
	cfg := w.config(t, "primary", w.issuedIdentity(t, "primary"))
	cfg.Capabilities = protocol.NewFeatures(protocol.ApplicationRuntime)
	cfg.Executor = &fake{}
	stop := start(t, w.dialer(t, 0), cfg)
	eventually(t, "a link", func() bool { return w.heartbeats("primary") >= 1 })
	if w.hub.Send(t.Context(), "primary", envelope(5)) {
		t.Fatal("the hub did not offer the feature")
	}
	stop()
}

func TestAnAgentRejectsAnEnvelopeOutsideTheNegotiatedFeatures(t *testing.T) {
	w := newWorld(t, agentlink.DefaultHubSettings())
	answers := make(chan protocol.Observation, 1)
	// A hub that sends an envelope right after a Welcome without features
	// and keeps what the agent answers.
	pushy := &dialer{address: listenTLS(t, w.tls, func(_ context.Context, conn *tls.Conn) {
		_, _, _ = protocol.ReadFrame(conn)
		welcome := protocol.Welcome{ProtocolVersion: 1, HubVersion: "pushy", HeartbeatMs: 20, Features: protocol.Features{}}
		if protocol.WriteFrame(conn, welcome) != nil || protocol.WriteFrame(conn, envelope(5)) != nil {
			return
		}
		for {
			m, open, err := protocol.ReadFrame(conn)
			if err != nil || !open {
				return
			}
			switch m := m.(type) {
			case protocol.Heartbeat:
				_ = protocol.WriteFrame(conn, protocol.HeartbeatAck(m))
			case protocol.Observation:
				select {
				case answers <- m:
				default:
				}
			case protocol.Hello, protocol.Welcome, protocol.Refused, protocol.HeartbeatAck, protocol.Enroll,
				protocol.Renew, protocol.Enrolled, protocol.Apply, protocol.Unknown:
			}
		}
	})}
	f := &fake{}
	cfg := w.config(t, "primary", w.issuedIdentity(t, "primary"))
	cfg.Executor = f
	stop := start(t, pushy, cfg)
	var answer protocol.Observation
	select {
	case answer = <-answers:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for an answer")
	}
	if answer.Phase != protocol.RuntimePhaseRejected || answer.Reason == nil || *answer.Reason != "UnsupportedCapability" {
		t.Fatalf("%+v", answer)
	}
	if len(f.specs()) != 0 {
		t.Fatal("never applied")
	}
	stop()
}
