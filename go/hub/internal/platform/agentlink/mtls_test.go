package agentlink_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
	"testing"
	"time"
)

// Ported from crates/kuben-platform/tests/agent_link_mtls.rs, the M0 spike
// (ADR-027) of the AgentLink transport: the agent dials the hub over TLS
// 1.3 with mutual authentication, pins the hub's bootstrap CA, and the hub
// accepts only client certificates issued by that CA and identifies the
// agent by its certificate. The configurations are built here as the spike
// built them, with crypto/tls in place of rustls.

const spikeHubName = "hub.kuben.internal"

type spikeCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func spikeKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func newSpikeCA(t *testing.T, name string) spikeCA {
	t.Helper()
	key := spikeKey(t)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Unix(0, 0), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return spikeCA{cert: cert, key: key}
}

func spikeLeaf(t *testing.T, ca spikeCA, cn string, sans []string, usage x509.ExtKeyUsage) tls.Certificate {
	t.Helper()
	key := spikeKey(t)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: cn}, DNSNames: sans,
		NotBefore: time.Unix(0, 0), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{usage},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, key.Public(), ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func spikeRoots(ca spikeCA) *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	return pool
}

// spikeHub is the hub: TLS 1.3 only, a client certificate required and
// issued by trust.
func spikeHub(trust spikeCA, identity tls.Certificate) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
		Certificates: []tls.Certificate{identity}, ClientCAs: spikeRoots(trust), ClientAuth: tls.RequireAndVerifyClientCert,
	}
}

// spikeAgent is the agent: it pins pinned as the only trusted CA for the
// hub.
func spikeAgent(pinned spikeCA, identity *tls.Certificate) *tls.Config {
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
		RootCAs: spikeRoots(pinned), ServerName: spikeHubName,
	}
	if identity != nil {
		chosen := *identity
		cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &chosen, nil }
	}
	return cfg
}

// spikeExchange is one request/response over mTLS: the reply and the
// agent's certificate, only when both sides authenticated each other.
func spikeExchange(t *testing.T, hub, agent *tls.Config) (string, []byte, error) {
	t.Helper()
	agentConn, hubConn := loopback(t)
	type seen struct {
		peer []byte
		err  error
	}
	hubSide := make(chan seen, 1)
	go func() {
		conn := tls.Server(hubConn, hub)
		defer conn.Close()
		if err := conn.Handshake(); err != nil {
			hubSide <- seen{err: fmt.Errorf("hub: %w", err)}
			return
		}
		state := conn.ConnectionState()
		if len(state.PeerCertificates) == 0 {
			hubSide <- seen{err: errors.New("hub: no client certificate")}
			return
		}
		hello := make([]byte, 5)
		if _, err := io.ReadFull(conn, hello); err != nil {
			hubSide <- seen{err: fmt.Errorf("hub read: %w", err)}
			return
		}
		if _, err := conn.Write([]byte("pong")); err != nil {
			hubSide <- seen{err: fmt.Errorf("hub write: %w", err)}
			return
		}
		hubSide <- seen{peer: state.PeerCertificates[0].Raw, err: conn.CloseWrite()}
	}()
	reply, agentErr := func() ([]byte, error) {
		conn := tls.Client(agentConn, agent)
		defer conn.Close()
		if err := conn.Handshake(); err != nil {
			return nil, fmt.Errorf("agent: %w", err)
		}
		if _, err := conn.Write([]byte("hello")); err != nil {
			return nil, fmt.Errorf("agent write: %w", err)
		}
		return io.ReadAll(conn)
	}()
	hubResult := <-hubSide
	if agentErr != nil {
		return "", nil, agentErr
	}
	if hubResult.err != nil {
		return "", nil, hubResult.err
	}
	return string(reply), hubResult.peer, nil
}

func spikeHubIdentity(t *testing.T, ca spikeCA) tls.Certificate {
	return spikeLeaf(t, ca, "kuben-hub", []string{spikeHubName}, x509.ExtKeyUsageServerAuth)
}

func spikeAgentIdentity(t *testing.T, ca spikeCA, cluster string) tls.Certificate {
	return spikeLeaf(t, ca, "cluster:"+cluster, nil, x509.ExtKeyUsageClientAuth)
}

func TestAnEnrolledAgentAndThePinnedHubAuthenticateEachOther(t *testing.T) {
	bootstrap := newSpikeCA(t, "kuben bootstrap CA")
	agent := spikeAgentIdentity(t, bootstrap, "primary")
	reply, seen, err := spikeExchange(t, spikeHub(bootstrap, spikeHubIdentity(t, bootstrap)), spikeAgent(bootstrap, &agent))
	if err != nil {
		t.Fatalf("mutual TLS succeeds: %v", err)
	}
	if reply != "pong" || !bytes.Equal(seen, agent.Certificate[0]) {
		t.Fatal("the hub identifies the agent by its certificate")
	}
}

func TestAClientCertificateFromAnotherCAIsRejected(t *testing.T) {
	bootstrap, rogue := newSpikeCA(t, "kuben bootstrap CA"), newSpikeCA(t, "rogue CA")
	agent := spikeAgentIdentity(t, rogue, "primary")
	if _, _, err := spikeExchange(t, spikeHub(bootstrap, spikeHubIdentity(t, bootstrap)), spikeAgent(bootstrap, &agent)); err == nil {
		t.Fatal("unknown client CA must fail")
	}
}

func TestAnAgentWithoutACertificateIsRejected(t *testing.T) {
	bootstrap := newSpikeCA(t, "kuben bootstrap CA")
	if _, _, err := spikeExchange(t, spikeHub(bootstrap, spikeHubIdentity(t, bootstrap)), spikeAgent(bootstrap, nil)); err == nil {
		t.Fatal("anonymous agents must fail")
	}
}

func TestTheAgentRefusesAHubNotSignedByThePinnedCA(t *testing.T) {
	bootstrap, impostor := newSpikeCA(t, "kuben bootstrap CA"), newSpikeCA(t, "impostor CA")
	agent := spikeAgentIdentity(t, bootstrap, "primary")
	if _, _, err := spikeExchange(t, spikeHub(bootstrap, spikeHubIdentity(t, impostor)), spikeAgent(bootstrap, &agent)); err == nil {
		t.Fatal("the agent must not talk to an unpinned hub")
	}
}

func TestTheAgentRefusesAHubCertificateForAnotherName(t *testing.T) {
	bootstrap := newSpikeCA(t, "kuben bootstrap CA")
	wrongName := spikeLeaf(t, bootstrap, "kuben-hub", []string{"other.example.com"}, x509.ExtKeyUsageServerAuth)
	agent := spikeAgentIdentity(t, bootstrap, "primary")
	if _, _, err := spikeExchange(t, spikeHub(bootstrap, wrongName), spikeAgent(bootstrap, &agent)); err == nil {
		t.Fatal("hostname verification must hold")
	}
}

// Extended key usage keeps roles apart: a stolen hub certificate is not a
// cluster identity.
func TestAServerCertificateCannotBeUsedAsAnAgentIdentity(t *testing.T) {
	bootstrap := newSpikeCA(t, "kuben bootstrap CA")
	asAgent := spikeLeaf(t, bootstrap, "kuben-hub", []string{spikeHubName}, x509.ExtKeyUsageServerAuth)
	if _, _, err := spikeExchange(t, spikeHub(bootstrap, spikeHubIdentity(t, bootstrap)), spikeAgent(bootstrap, &asAgent)); err == nil {
		t.Fatal("serverAuth-only certificates must not authenticate agents")
	}
}
