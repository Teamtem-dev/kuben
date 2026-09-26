package agentlink

// The hub's end of AgentLink (hub.rs), hosted by `kuben serve`:
//
//   - An anonymous peer may only enroll ([ReceiveEnrollment]); the device is
//     recorded in the [Registry] before [AnswerEnrollment] hands the
//     certificate over.
//   - An authenticated peer is the cluster its certificate names (the URI
//     SAN `kuben://cluster/<id>` the hub issued). Its Hello must name the
//     same cluster, and its device must be the cluster's current, unrevoked
//     agent device ([Registry.Admits]): a revoked device, or one a newer
//     enrollment replaced, is refused even while its certificate is valid.
//   - The handshake negotiates the protocol version and the features; the
//     hub then answers heartbeats and records the link and when it last
//     heard from each cluster.
//   - A linked agent renews its certificate over the link, for the device
//     key the link authenticated with ([Enrollment.Renew]).
//   - [Hub.Send] hands a linked agent an execution envelope, when both sides
//     negotiated [protocol.ApplicationRuntime]; the agent's observations go
//     to [Registry.Observed].

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/version"
	protocol "github.com/Teamtem-dev/kuben/packages/api/agentlink"
)

// HubSettings is what the hub offers.
type HubSettings struct {
	HubVersion string
	Versions   protocol.Versions
	Features   protocol.Features
	// Heartbeat is how often agents send a heartbeat.
	Heartbeat time.Duration
}

// DefaultHubSettings is this build's version and protocol versions, no
// features, a heartbeat every 10 seconds.
func DefaultHubSettings() HubSettings {
	return HubSettings{
		HubVersion: version.Version,
		Versions:   protocol.SupportedVersions,
		Features:   protocol.Features{},
		Heartbeat:  10 * time.Second,
	}
}

// SessionInfo is what the hub knows of a cluster's latest link.
type SessionInfo struct {
	Version      uint32
	Features     protocol.Features
	AgentVersion string
	Heartbeats   uint64
	// LastSeen is unix milliseconds.
	LastSeen int64
}

// Registry is where the hub keeps what it knows of each cluster's agent:
// SQL in `kuben serve`, memory in tests. A registry that cannot answer
// refuses.
type Registry interface {
	// Admits says whether device is the current, unrevoked agent device of
	// cluster.
	Admits(ctx context.Context, cluster, device string) bool
	// Certified: a certificate valid until notAfter was issued to device of
	// cluster; it is the cluster's agent device from now on.
	Certified(ctx context.Context, cluster, device string, notAfter time.Time)
	// Linked: device of cluster linked with session.
	Linked(ctx context.Context, cluster, device string, session SessionInfo)
	// Heard: a heartbeat of device of cluster arrived.
	Heard(ctx context.Context, cluster, device string)
	// Observed: device of cluster reported observation of an envelope.
	Observed(ctx context.Context, cluster, device string, observation protocol.Observation)
}

// linked is a cluster's latest link: what it negotiated, and the way to
// send it envelopes.
type linked struct {
	info   SessionInfo
	outbox chan protocol.Message
	// done is closed when the link ended.
	done chan struct{}
}

// Hub is the hub's AgentLink endpoint.
type Hub struct {
	enrollment *Enrollment
	registry   Registry
	settings   HubSettings
	clock      clock.Clock

	mu sync.Mutex
	// sessions are the latest link of each cluster, guarded by mu.
	sessions map[string]*linked
}

// NewHub is a hub issuing with enrollment and recording in registry.
func NewHub(enrollment *Enrollment, registry Registry, settings HubSettings, clk clock.Clock) *Hub {
	return &Hub{enrollment: enrollment, registry: registry, settings: settings, clock: clk, sessions: map[string]*linked{}}
}

// Enrollment is the hub's enrollment service.
func (h *Hub) Enrollment() *Enrollment { return h.enrollment }

// Registry is where the hub records its agents.
func (h *Hub) Registry() Registry { return h.registry }

func (h *Hub) now() time.Time { return time.UnixMilli(h.clock.NowMs()) }

// Session is the latest link of cluster this hub served, if any.
func (h *Hub) Session(cluster string) (SessionInfo, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	l := h.sessions[cluster]
	if l == nil {
		return SessionInfo{}, false
	}
	info := l.info
	info.Features = append(protocol.Features{}, l.info.Features...)
	return info, true
}

// Send hands the agent of cluster the envelope apply over its live link.
// False when it is not linked, or did not negotiate the runtime feature.
func (h *Hub) Send(ctx context.Context, cluster string, apply protocol.Apply) bool {
	h.mu.Lock()
	l := h.sessions[cluster]
	usable := l != nil && l.info.Features.Has(protocol.ApplicationRuntime)
	h.mu.Unlock()
	if !usable || l == nil {
		return false
	}
	select {
	case <-l.done:
		return false
	default:
	}
	select {
	case l.outbox <- apply:
		return true
	case <-l.done:
		return false
	case <-ctx.Done():
		return false
	}
}

// ClusterOf is the cluster a certificate the hub issued names.
func ClusterOf(cert *x509.Certificate) (string, bool) {
	for _, uri := range cert.URIs {
		if cluster, ok := strings.CutPrefix(uri.String(), "kuben://cluster/"); ok {
			return cluster, true
		}
	}
	return "", false
}

const notTheAgent = "this device is not the cluster's agent: revoked, or replaced by a newer enrollment"

func refuse(conn *tls.Conn, reason protocol.Refusal, message string) error {
	return protocol.WriteFrame(conn, protocol.Refused{Reason: reason, Message: message})
}

// Serve serves one accepted TLS connection until it ends or ctx does, and
// closes it.
func (h *Hub) Serve(ctx context.Context, conn *tls.Conn) error {
	defer conn.Close() //nolint:errcheck // the link is over either way
	if err := conn.HandshakeContext(ctx); err != nil {
		return err //nolint:wrapcheck // crypto/tls says what failed
	}
	cert, authenticated := protocol.PeerCertificate(conn.ConnectionState())
	if !authenticated {
		issued, ok, err := ReceiveEnrollment(ctx, conn, h.enrollment, h.now())
		if err != nil || !ok {
			return err
		}
		// Recorded before the agent has its certificate: it links at once,
		// and a Hello the registry does not know yet would be refused as
		// revoked.
		h.registry.Certified(ctx, issued.ClusterID, issued.DeviceID, issued.NotAfter)
		return AnswerEnrollment(conn, issued)
	}
	cluster, named := ClusterOf(cert)
	if !named {
		return refuse(conn, protocol.RefusalUnknownCluster, "the certificate names no cluster")
	}
	device := protocol.DeviceIDOf(cert.RawSubjectPublicKeyInfo)
	m, _, err := protocol.ReadFrame(conn)
	if err != nil {
		return err //nolint:wrapcheck // a frame error says what failed
	}
	hello, isHello := m.(protocol.Hello)
	switch {
	case !isHello:
		return refuse(conn, protocol.RefusalBadRequest, "a link opens with Hello")
	case hello.ClusterID != cluster:
		return refuse(conn, protocol.RefusalUnknownCluster, "Hello names another cluster than the certificate")
	case !h.registry.Admits(ctx, cluster, device):
		return refuse(conn, protocol.RefusalRevoked, notTheAgent)
	}
	agreed, err := protocol.Negotiate(h.settings.Versions, h.settings.Features, hello.ProtocolVersions, hello.Capabilities)
	if err != nil {
		var reason protocol.Refusal
		if !errors.As(err, &reason) {
			reason = protocol.RefusalUnsupportedProtocol
		}
		return refuse(conn, reason, "no protocol version in common")
	}
	welcome := protocol.Welcome{
		ProtocolVersion: agreed.Version,
		HubVersion:      h.settings.HubVersion,
		HeartbeatMs:     uint64(min(h.settings.Heartbeat.Milliseconds(), math.MaxInt64)), //nolint:gosec // a duration is not negative
		Features:        agreed.Features,
	}
	if err := protocol.WriteFrame(conn, welcome); err != nil {
		return err //nolint:wrapcheck // a frame error says what failed
	}
	session := SessionInfo{Version: agreed.Version, Features: agreed.Features, AgentVersion: hello.AgentVersion, LastSeen: h.clock.NowMs()}
	h.registry.Linked(ctx, cluster, device, session)
	l := &linked{info: session, outbox: make(chan protocol.Message, 16), done: make(chan struct{})}
	h.mu.Lock()
	h.sessions[cluster] = l
	h.mu.Unlock()
	defer close(l.done)
	return h.converse(ctx, conn, cluster, device, l)
}

// frame is one read of the link.
type frame struct {
	message protocol.Message
	open    bool
	err     error
}

// readFrames reads conn until it fails or ends, handing each read to
// frames until stop closes.
func readFrames(conn *tls.Conn, frames chan<- frame, stop <-chan struct{}) {
	for {
		m, open, err := protocol.ReadFrame(conn)
		select {
		case frames <- frame{message: m, open: open, err: err}:
		case <-stop:
			return
		}
		if err != nil || !open {
			return
		}
	}
}

// converse is the linked part of a connection until the agent hangs up.
// Frames are read by a goroutine of their own, so none is lost halfway
// while an envelope goes out.
func (h *Hub) converse(ctx context.Context, conn *tls.Conn, cluster, device string, l *linked) error {
	frames := make(chan frame, 16)
	stop := make(chan struct{})
	var reading sync.WaitGroup
	reading.Go(func() { readFrames(conn, frames, stop) })
	defer func() {
		close(stop)
		conn.Close() //nolint:errcheck,gosec // unblocks the reader; the link is over
		reading.Wait()
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case command := <-l.outbox:
			if err := protocol.WriteFrame(conn, command); err != nil {
				return err //nolint:wrapcheck // a frame error says what failed
			}
		case f := <-frames:
			switch {
			case f.err != nil:
				return f.err
			case !f.open:
				return nil
			}
			goOn, err := h.handle(ctx, conn, f.message, cluster, device)
			if err != nil || !goOn {
				return err
			}
		}
	}
}

// handle is one message of a linked agent; false when the link must
// close.
func (h *Hub) handle(ctx context.Context, conn *tls.Conn, m protocol.Message, cluster, device string) (bool, error) {
	switch m := m.(type) {
	case protocol.Heartbeat:
		h.mu.Lock()
		if l := h.sessions[cluster]; l != nil {
			l.info.Heartbeats++
			l.info.LastSeen = h.clock.NowMs()
		}
		h.mu.Unlock()
		h.registry.Heard(ctx, cluster, device)
		return true, protocol.WriteFrame(conn, protocol.HeartbeatAck(m))
	case protocol.Renew:
		if !h.registry.Admits(ctx, cluster, device) {
			return false, refuse(conn, protocol.RefusalRevoked, notTheAgent)
		}
		issued, err := h.enrollment.Renew(cluster, m.CSR, device, h.now())
		if err != nil {
			reason := protocol.RefusalBadRequest
			if r, ok := errors.AsType[protocol.Refusal](err); ok {
				reason = r
			}
			return false, refuse(conn, reason, "a renewal is for the device key of this link")
		}
		h.registry.Certified(ctx, cluster, device, issued.NotAfter)
		return true, protocol.WriteFrame(conn, protocol.Enrolled{
			Certificate: issued.CertificatePEM, NotAfter: issued.NotAfter.Unix(),
		})
	case protocol.Observation:
		h.registry.Observed(ctx, cluster, device, m)
		return true, nil
	case protocol.Unknown:
		return true, nil
	case protocol.Hello, protocol.Welcome, protocol.Refused, protocol.HeartbeatAck, protocol.Enroll,
		protocol.Enrolled, protocol.Apply:
	}
	return false, refuse(conn, protocol.RefusalBadRequest, "unexpected message")
}
