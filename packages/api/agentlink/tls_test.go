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
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Teamtem-dev/kuben/packages/api/agentlink"
)

// Ported from crates/kuben-agent/src/tls.rs.

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

var serial = big.NewInt(1)

func ca(t *testing.T, name string) testCA {
	t.Helper()
	key := newKey(t)
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: name},
		NotBefore: time.Unix(0, 0), NotAfter: time.Now().Add(24 * time.Hour),
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
	return testCA{cert: cert, key: key}
}

func leaf(t *testing.T, issuer testCA, cn string, sans []string, usage x509.ExtKeyUsage) tls.Certificate {
	t.Helper()
	key := newKey(t)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: cn}, DNSNames: sans,
		NotBefore: time.Unix(0, 0), NotAfter: time.Now().Add(24 * time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{usage},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, issuer.cert, key.Public(), issuer.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func hubIdentity(t *testing.T, c testCA) tls.Certificate {
	return leaf(t, c, "kuben-hub", []string{agentlink.HubName}, x509.ExtKeyUsageServerAuth)
}

func agentIdentity(t *testing.T, c testCA) tls.Certificate {
	return leaf(t, c, "cluster:primary", nil, x509.ExtKeyUsageClientAuth)
}

func hubConfig(t *testing.T, trust testCA, identity tls.Certificate, auth agentlink.ClientAuth) *tls.Config {
	t.Helper()
	cfg, err := agentlink.HubConfig([]*x509.Certificate{trust.cert}, identity, auth)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func agentConfig(t *testing.T, pinned testCA, identity *tls.Certificate) *tls.Config {
	t.Helper()
	cfg, err := agentlink.AgentConfig([]*x509.Certificate{pinned.cert}, agentlink.HubName, identity)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// loopback is a connected pair of TCP streams.
func loopback(t *testing.T) (client, server net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			accepted <- nil
			return
		}
		accepted <- c
	}()
	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server = <-accepted
	if server == nil {
		t.Fatal("accept failed")
	}
	return client, server
}

// exchange is one round trip; the certificate the hub saw (if any) when
// both ends accepted each other.
func exchange(t *testing.T, hub, agent *tls.Config) (*x509.Certificate, error) {
	t.Helper()
	agentConn, hubConn := loopback(t)
	type seen struct {
		peer *x509.Certificate
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
		peer, _ := agentlink.PeerCertificate(conn.ConnectionState())
		hello := make([]byte, 5)
		if _, err := io.ReadFull(conn, hello); err != nil {
			hubSide <- seen{err: fmt.Errorf("hub read: %w", err)}
			return
		}
		if _, err := conn.Write([]byte("pong")); err != nil {
			hubSide <- seen{err: fmt.Errorf("hub write: %w", err)}
			return
		}
		hubSide <- seen{peer: peer, err: conn.CloseWrite()}
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
	switch {
	case agentErr != nil:
		return nil, agentErr
	case hubResult.err != nil:
		return nil, hubResult.err
	case !bytes.Equal(reply, []byte("pong")):
		return nil, fmt.Errorf("unexpected reply %q", reply)
	}
	return hubResult.peer, nil
}

func TestAnEnrolledAgentAndThePinnedHubAuthenticateEachOther(t *testing.T) {
	root := ca(t, "kuben CA")
	identity := agentIdentity(t, root)
	seen, err := exchange(t, hubConfig(t, root, hubIdentity(t, root), agentlink.ClientAuthRequired), agentConfig(t, root, &identity))
	if err != nil {
		t.Fatalf("mutual TLS: %v", err)
	}
	if seen == nil || !bytes.Equal(seen.Raw, identity.Certificate[0]) {
		t.Fatal("the hub identifies the agent by its certificate")
	}
}

func TestAClientCertificateFromAnotherCAIsRejected(t *testing.T) {
	root, rogue := ca(t, "kuben CA"), ca(t, "rogue CA")
	identity := agentIdentity(t, rogue)
	if _, err := exchange(t, hubConfig(t, root, hubIdentity(t, root), agentlink.ClientAuthEnrollmentAllowed),
		agentConfig(t, root, &identity)); err == nil {
		t.Fatal("accepted")
	}
}

func TestAnAnonymousPeerGetsInOnlyWhereEnrollmentIsAllowed(t *testing.T) {
	root := ca(t, "kuben CA")
	if _, err := exchange(t, hubConfig(t, root, hubIdentity(t, root), agentlink.ClientAuthRequired), agentConfig(t, root, nil)); err == nil {
		t.Fatal("an anonymous peer got in")
	}
	seen, err := exchange(t, hubConfig(t, root, hubIdentity(t, root), agentlink.ClientAuthEnrollmentAllowed), agentConfig(t, root, nil))
	if err != nil {
		t.Fatalf("an anonymous peer may enroll: %v", err)
	}
	if seen != nil {
		t.Fatal("and is known to be anonymous")
	}
}

func TestTheAgentRefusesAHubItHasNotPinnedOrOfAnotherName(t *testing.T) {
	root, impostor := ca(t, "kuben CA"), ca(t, "impostor CA")
	identity := agentIdentity(t, root)
	if _, err := exchange(t, hubConfig(t, root, hubIdentity(t, impostor), agentlink.ClientAuthRequired),
		agentConfig(t, root, &identity)); err == nil {
		t.Fatal("an unpinned hub")
	}
	otherName := leaf(t, root, "kuben-hub", []string{"other.example.com"}, x509.ExtKeyUsageServerAuth)
	if _, err := exchange(t, hubConfig(t, root, otherName, agentlink.ClientAuthRequired), agentConfig(t, root, &identity)); err == nil {
		t.Fatal("a hub of another name")
	}
}

func TestAServerCertificateIsNotAnAgentIdentity(t *testing.T) {
	root := ca(t, "kuben CA")
	asAgent := hubIdentity(t, root)
	if _, err := exchange(t, hubConfig(t, root, hubIdentity(t, root), agentlink.ClientAuthRequired),
		agentConfig(t, root, &asAgent)); err == nil {
		t.Fatal("a server certificate authenticated an agent")
	}
}

func TestAConfigurationWithoutATrustedCAIsRefused(t *testing.T) {
	if _, err := agentlink.AgentConfig(nil, agentlink.HubName, nil); !errors.Is(err, agentlink.ErrNoTrust) {
		t.Fatal(err)
	}
	var nameErr agentlink.ServerNameError
	if _, err := agentlink.ServerName("not a name"); !errors.As(err, &nameErr) {
		t.Fatal(err)
	}
	for _, good := range []string{agentlink.HubName, "kuben.kuben-system.svc", "a_b.example.", "10.0.0.1", "::1", "1.example"} {
		if _, err := agentlink.ServerName(good); err != nil {
			t.Errorf("%s: %v", good, err)
		}
	}
	for _, bad := range []string{"", ".", "-a.example", "a-.example", "example.123", "a..b", "é.example", strings.Repeat("a", 64) + ".example"} {
		if _, err := agentlink.ServerName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
