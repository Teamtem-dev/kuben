package agentlink_test

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"errors"
	"io"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/agentlink"
	"github.com/Teamtem-dev/kuben/go/kubenapi/protocol"
)

// Ported from crates/kuben-agent/src/enroll.rs (the hub's side; the device
// key's test is in kubenapi/protocol).

const day = 24 * time.Hour

func now() time.Time { return time.Unix(1_789_000_000, 0) }

func newCA(t *testing.T) agentlink.ClusterCA {
	t.Helper()
	ca, err := agentlink.GenerateClusterCA("kuben cluster CA")
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

func enrollment(t *testing.T) (*agentlink.Enrollment, string) {
	t.Helper()
	tokens := newMemoryTokens()
	token := tokens.issue("primary", 30*time.Minute, now())
	return agentlink.NewEnrollment(newCA(t), tokens, day), token
}

func deviceKey(t *testing.T) protocol.DeviceKey {
	t.Helper()
	k, err := protocol.GenerateDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func csrOf(t *testing.T, key protocol.DeviceKey, cluster string) string {
	t.Helper()
	csr, err := key.CSR(cluster)
	if err != nil {
		t.Fatal(err)
	}
	return csr
}

func parsePEM(t *testing.T, text string) *x509.Certificate {
	t.Helper()
	cert, err := protocol.ParseCertificatePEM(text)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func TestACertificateIsIssuedFromTheKeyAlone(t *testing.T) {
	service, token := enrollment(t)
	// A CSR that asks for far more than a cluster identity.
	key, err := protocol.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	eku, err := asn1.Marshal([]asn1.ObjectIdentifier{{1, 3, 6, 1, 5, 5, 7, 3, 1}})
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:         pkix.Name{CommonName: "cluster:someone-else"},
		DNSNames:        []string{protocol.HubName},
		ExtraExtensions: []pkix.Extension{{Id: asn1.ObjectIdentifier{2, 5, 29, 37}, Value: eku}},
	}, key.Signer())
	if err != nil {
		t.Fatal(err)
	}
	issued, err := service.Enroll(t.Context(), token, "primary", protocol.EncodePEM("CERTIFICATE REQUEST", der), now())
	if err != nil {
		t.Fatal(err)
	}
	cert := parsePEM(t, issued.CertificatePEM)
	if cert.Subject.CommonName != "cluster:primary" {
		t.Fatal(cert.Subject)
	}
	if len(cert.URIs) != 1 || cert.URIs[0].String() != "kuben://cluster/primary" ||
		len(cert.DNSNames)+len(cert.IPAddresses)+len(cert.EmailAddresses) != 0 {
		t.Fatalf("names: %v %v", cert.URIs, cert.DNSNames)
	}
	if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth || len(cert.UnknownExtKeyUsage) != 0 {
		t.Fatal("client authentication only")
	}
	if !cert.NotAfter.Equal(now().Add(day)) {
		t.Fatal(cert.NotAfter)
	}
	if issued.DeviceID != protocol.DeviceIDOf(key.PublicKeyInfo()) {
		t.Fatal(issued.DeviceID)
	}
}

func TestATokenIsRedeemedOnceAndResumedOnlyByItsDevice(t *testing.T) {
	service, token := enrollment(t)
	ctx := t.Context()
	device := deviceKey(t)
	csr := csrOf(t, device, "primary")
	first, err := service.Enroll(ctx, token, "primary", csr, now())
	if err != nil {
		t.Fatal(err)
	}
	again, err := service.Enroll(ctx, token, "primary", csr, now().Add(5*time.Minute))
	if err != nil {
		t.Fatal("the same device resumes:", err)
	}
	if first.DeviceID != again.DeviceID || first.CertificatePEM == again.CertificatePEM {
		t.Fatal("a fresh certificate for the same device")
	}
	other := deviceKey(t)
	if _, err := service.Enroll(ctx, token, "primary", csrOf(t, other, "primary"), now()); !errors.Is(err, protocol.RefusalInvalidToken) {
		t.Fatal("another device:", err)
	}
	if _, err := service.Enroll(ctx, token, "primary", csr, now().Add(30*time.Minute+agentlink.ResumeGrace)); !errors.Is(err, protocol.RefusalInvalidToken) {
		t.Fatal("resuming ends with the grace period:", err)
	}
}

func TestEveryBadTokenGetsTheSameRefusal(t *testing.T) {
	service, token := enrollment(t)
	ctx := t.Context()
	csr := csrOf(t, deviceKey(t), "primary")
	for what, try := range map[string]func() error{
		"unknown": func() error { _, err := service.Enroll(ctx, "kbt_unknown", "primary", csr, now()); return err },
		"another cluster": func() error {
			_, err := service.Enroll(ctx, token, "secondary", csr, now())
			return err
		},
		"expired": func() error {
			_, err := service.Enroll(ctx, token, "primary", csr, now().Add(31*time.Minute))
			return err
		},
	} {
		if err := try(); !errors.Is(err, protocol.RefusalInvalidToken) {
			t.Errorf("%s: %v", what, err)
		}
	}
}

func TestACSRThatDoesNotVerifyNeverConsumesTheToken(t *testing.T) {
	service, token := enrollment(t)
	if _, err := service.Enroll(t.Context(), token, "primary", "not a csr", now()); !errors.Is(err, protocol.RefusalBadRequest) {
		t.Fatal(err)
	}
	if _, err := service.Enroll(t.Context(), token, "primary", csrOf(t, deviceKey(t), "primary"), now()); err != nil {
		t.Fatal("the token is still unused:", err)
	}
}

func TestARenewalIsOnlyForTheDeviceKeyOfTheLink(t *testing.T) {
	service, _ := enrollment(t)
	device := deviceKey(t)
	renewed, err := service.Renew("primary", csrOf(t, device, "primary"), device.DeviceID(), now())
	if err != nil {
		t.Fatal(err)
	}
	if renewed.DeviceID != device.DeviceID() || !renewed.NotAfter.Equal(now().Add(day)) {
		t.Fatalf("%+v", renewed)
	}
	other := deviceKey(t)
	if _, err := service.Renew("primary", csrOf(t, other, "primary"), device.DeviceID(), now()); !errors.Is(err, protocol.RefusalBadRequest) {
		t.Fatal("another key:", err)
	}
}

func TestAClusterCASurvivesItsPEMAndKeepsIssuingForTheSameTrust(t *testing.T) {
	ca := newCA(t)
	kept, err := agentlink.ClusterCAFromPEM(ca.CertificatePEM(), ca.KeyPEM())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(kept.Certificate().Raw, ca.Certificate().Raw) {
		t.Fatal("another certificate")
	}
	csr, err := protocol.ParseCSR(csrOf(t, deviceKey(t), "primary"))
	if err != nil {
		t.Fatal(err)
	}
	issued, err := kept.Issue("primary", csr, day, now())
	if err != nil {
		t.Fatal(err)
	}
	if issued.ClusterID != "primary" {
		t.Fatal(issued.ClusterID)
	}
	if err := parsePEM(t, issued.CertificatePEM).CheckSignatureFrom(ca.Certificate()); err != nil {
		t.Fatal("signed by the original CA's key:", err)
	}
}

// The certificates have the shape rcgen gave them.
func TestTheCertificatesHaveTheShapeRcgenGaveThem(t *testing.T) {
	ca := newCA(t)
	root := ca.Certificate()
	spki := sha256.Sum256(root.RawSubjectPublicKeyInfo)
	if !root.IsCA || root.MaxPathLen != 0 || !root.MaxPathLenZero ||
		root.KeyUsage != x509.KeyUsageCertSign|x509.KeyUsageCRLSign || !bytes.Equal(root.SubjectKeyId, spki[:20]) ||
		len(root.AuthorityKeyId) != 0 {
		t.Fatalf("CA: %+v", root)
	}
	if !root.NotBefore.Equal(time.Date(1975, 1, 1, 0, 0, 0, 0, time.UTC)) || !root.NotAfter.Equal(time.Date(4096, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatal(root.NotBefore, root.NotAfter)
	}
	subject, err := protocol.UTF8Subject("kuben cluster CA")
	if err != nil || !bytes.Equal(root.RawSubject, subject) || !bytes.Equal(root.RawIssuer, subject) {
		t.Fatal("the CA's name is a UTF8String common name")
	}
	point, err := root.PublicKey.(interface{ Bytes() ([]byte, error) }).Bytes()
	if err != nil {
		t.Fatal(err)
	}
	serial := sha256.Sum256(point)
	serial[0] &= 0x7f
	if root.SerialNumber.Cmp(new(big.Int).SetBytes(serial[:20])) != 0 {
		t.Fatal("rcgen's serial of the public key")
	}
	csr, err := protocol.ParseCSR(csrOf(t, deviceKey(t), "primary"))
	if err != nil {
		t.Fatal(err)
	}
	issued, err := ca.Issue("primary", csr, day, now())
	if err != nil {
		t.Fatal(err)
	}
	client := parsePEM(t, issued.CertificatePEM)
	if !bytes.Equal(client.AuthorityKeyId, root.SubjectKeyId) || client.KeyUsage != x509.KeyUsageDigitalSignature ||
		client.BasicConstraintsValid || len(client.SubjectKeyId) != 0 || !client.NotBefore.Equal(now().Add(-5*time.Minute)) ||
		client.SerialNumber.Sign() <= 0 || client.SignatureAlgorithm != x509.ECDSAWithSHA256 {
		t.Fatalf("client: %+v", client)
	}
	for _, ext := range client.Extensions {
		critical := ext.Id.Equal(asn1.ObjectIdentifier{2, 5, 29, 15}) // key usage
		if ext.Critical != critical {
			t.Errorf("extension %v critical %v", ext.Id, ext.Critical)
		}
	}
	server, err := ca.ServerIdentity(protocol.HubName, day, now())
	if err != nil {
		t.Fatal(err)
	}
	if len(server.Leaf.AuthorityKeyId) != 0 || server.Leaf.Subject.CommonName != "kuben-hub" ||
		len(server.Leaf.DNSNames) != 1 || server.Leaf.DNSNames[0] != protocol.HubName ||
		server.Leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Fatalf("server: %+v", server.Leaf)
	}
}

// loopback is a connected pair of TCP streams (the Rust tests' duplex; a
// synchronous net.Pipe would stall TLS 1.3 session tickets).
func loopback(t *testing.T) (client, server net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
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
	t.Cleanup(func() { client.Close(); server.Close() })
	return client, server
}

// hubTLS is the hub config for service: its CA signs the hub's own
// certificate too, so the agent pins that one CA.
func hubTLS(t *testing.T, service *agentlink.Enrollment, auth protocol.ClientAuth) *tls.Config {
	t.Helper()
	identity, err := service.CA().ServerIdentity(protocol.HubName, day, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := protocol.HubConfig([]*x509.Certificate{service.CA().Certificate()}, identity, auth)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func agentTLS(t *testing.T, service *agentlink.Enrollment, identity *tls.Certificate) *tls.Config {
	t.Helper()
	cfg, err := protocol.AgentConfig([]*x509.Certificate{service.CA().Certificate()}, protocol.HubName, identity)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestAnAgentEnrollsAnonymouslyThenDialsInWithItsCertificate(t *testing.T) {
	tokens := newMemoryTokens()
	token := tokens.issue("primary", 30*time.Minute, time.Now())
	service := agentlink.NewEnrollment(newCA(t), tokens, day)
	device := deviceKey(t)

	// Anonymous: enroll.
	agentConn, hubConn := loopback(t)
	type served struct {
		issued agentlink.Issued
		err    error
	}
	hubSide := make(chan served, 1)
	go func() {
		conn := tls.Server(hubConn, hubTLS(t, service, protocol.ClientAuthEnrollmentAllowed))
		if err := conn.Handshake(); err != nil {
			hubSide <- served{err: err}
			return
		}
		if _, hasCert := protocol.PeerCertificate(conn.ConnectionState()); hasCert {
			hubSide <- served{err: errors.New("not anonymous")}
			return
		}
		issued, ok, err := agentlink.ServeEnrollment(t.Context(), conn, service, time.Now())
		if err == nil && !ok {
			err = errors.New("nothing issued")
		}
		hubSide <- served{issued: issued, err: err}
	}()
	tlsConn := tls.Client(agentConn, agentTLS(t, service, nil))
	enrolled, err := protocol.RequestEnrollment(tlsConn, token, "primary", device)
	if err != nil {
		t.Fatal(err)
	}
	result := <-hubSide
	if result.err != nil {
		t.Fatal(result.err)
	}
	if result.issued.DeviceID != device.DeviceID() || enrolled.Certificate != result.issued.CertificatePEM {
		t.Fatalf("%+v", result.issued)
	}

	// Enrolled: mTLS with the issued certificate.
	identity, err := device.Identity(enrolled.Certificate)
	if err != nil {
		t.Fatal(err)
	}
	agentConn, hubConn = loopback(t)
	peers := make(chan *x509.Certificate, 1)
	go func() {
		conn := tls.Server(hubConn, hubTLS(t, service, protocol.ClientAuthRequired))
		var b [1]byte
		if _, err := io.ReadFull(conn, b[:]); err != nil {
			peers <- nil
			return
		}
		peer, _ := protocol.PeerCertificate(conn.ConnectionState())
		peers <- peer
	}()
	tlsConn = tls.Client(agentConn, agentTLS(t, service, &identity))
	if _, err := tlsConn.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	peer := <-peers
	if peer == nil || peer.Subject.CommonName != "cluster:primary" {
		t.Fatal("a client certificate for the cluster")
	}
}

func TestARefusedEnrollmentReachesTheAgentAsARefusal(t *testing.T) {
	service, _ := enrollment(t)
	device := deviceKey(t)
	agentConn, hubConn := net.Pipe()
	defer agentConn.Close()
	type served struct {
		ok  bool
		err error
	}
	hubSide := make(chan served, 1)
	go func() {
		defer hubConn.Close()
		_, ok, err := agentlink.ServeEnrollment(t.Context(), hubConn, service, now())
		hubSide <- served{ok: ok, err: err}
	}()
	_, err := protocol.RequestEnrollment(agentConn, "kbt_wrong", "primary", device)
	var refused protocol.EnrollRefusedError
	if !errors.As(err, &refused) || refused.Reason != protocol.RefusalInvalidToken {
		t.Fatal(err)
	}
	if result := <-hubSide; result.err != nil || result.ok {
		t.Fatalf("%+v", result)
	}
}
