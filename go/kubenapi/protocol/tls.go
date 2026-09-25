package protocol

// TLS for AgentLink (ADR-027, from the M0 spike; tls.rs): TLS 1.3 only,
// mutual authentication.
//
//   - The agent trusts only the pinned hub CA and checks the hub's name.
//   - The hub accepts client certificates issued by the cluster CA. With
//     [ClientAuthEnrollmentAllowed] it also accepts a connection without
//     one: such a peer is anonymous and may only enroll.
//   - Extended key usage keeps the roles apart: a server certificate is
//     never an agent identity (crypto/x509 checks clientAuth on the hub and
//     serverAuth on the agent, as webpki did).

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strings"
)

// HubName is the name the hub's certificate carries unless configured
// otherwise.
const HubName = "hub.kuben.internal"

// ClientAuth is whether the hub lets a peer in without a client
// certificate.
type ClientAuth int

// The client authentication modes.
const (
	// ClientAuthRequired: every peer presents a certificate issued by the
	// cluster CA.
	ClientAuthRequired ClientAuth = iota
	// ClientAuthEnrollmentAllowed: a peer without a certificate gets in too,
	// anonymous: it may only enroll.
	ClientAuthEnrollmentAllowed
)

// ErrNoTrust is a configuration without a trusted CA certificate.
var ErrNoTrust = errors.New("no trusted CA certificate was given") //nolint:gochecknoglobals // a sentinel error

// ServerNameError is a hub name that is not a valid DNS name or IP address.
type ServerNameError struct{ Name string }

func (e ServerNameError) Error() string {
	return fmt.Sprintf("`%s` is not a valid server name", e.Name)
}

// ServerName checks that name is a DNS name or an IP address the agent can
// check the hub's certificate against (rustls' ServerName::try_from).
func ServerName(name string) (string, error) {
	if net.ParseIP(name) != nil || validDNSName(name) {
		return name, nil
	}
	return "", ServerNameError{Name: name}
}

// validDNSName follows rustls-pki-types' DnsName rules: labels of ASCII
// letters, digits, `-` and `_`, 1 to 63 bytes, not starting or ending with
// `-`, at most 253 bytes, an optional final dot, and a last label that is
// not all digits.
func validDNSName(name string) bool {
	name = strings.TrimSuffix(name, ".")
	if name == "" || len(name) > 253 {
		return false
	}
	allNumeric := true
	for label := range strings.SplitSeq(name, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		allNumeric = true
		for i := range len(label) {
			c := label[i]
			switch {
			case c >= '0' && c <= '9':
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '-', c == '_':
				allNumeric = false
			default:
				return false
			}
		}
	}
	return !allNumeric
}

func roots(cas []*x509.Certificate) (*x509.CertPool, error) {
	if len(cas) == 0 {
		return nil, ErrNoTrust
	}
	pool := x509.NewCertPool()
	for _, ca := range cas {
		pool.AddCert(ca)
	}
	return pool, nil
}

// AgentConfig is the agent's end: it trusts pinned alone for the hub named
// hubName, and authenticates with identity once enrolled (without one, it
// can only enroll).
func AgentConfig(pinned []*x509.Certificate, hubName string, identity *tls.Certificate) (*tls.Config, error) {
	pool, err := roots(pinned)
	if err != nil {
		return nil, err
	}
	name, err := ServerName(hubName)
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS13,
		MaxVersion: tls.VersionTLS13,
		RootCAs:    pool,
		ServerName: name,
	}
	if identity != nil {
		chosen := *identity
		// Always the one identity, as rustls sent it: crypto/tls would send
		// none when the hub's CA hints do not match, and a certificate the
		// hub refuses must be refused, not turned into an anonymous peer.
		cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return &chosen, nil
		}
	}
	return cfg, nil
}

// HubConfig is the hub's end: it presents identity, and accepts client
// certificates issued by clusterCAs (or, with ClientAuthEnrollmentAllowed,
// no certificate at all).
func HubConfig(clusterCAs []*x509.Certificate, identity tls.Certificate, auth ClientAuth) (*tls.Config, error) {
	pool, err := roots(clusterCAs)
	if err != nil {
		return nil, err
	}
	if len(identity.Certificate) == 0 || identity.PrivateKey == nil {
		return nil, errors.New("TLS: the hub has no certificate")
	}
	mode := tls.RequireAndVerifyClientCert
	switch auth {
	case ClientAuthRequired:
	case ClientAuthEnrollmentAllowed:
		mode = tls.VerifyClientCertIfGiven
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{identity},
		ClientCAs:    pool,
		ClientAuth:   mode,
	}, nil
}

// PeerCertificate is the certificate the peer authenticated with; false
// for an anonymous peer (hub side).
func PeerCertificate(state tls.ConnectionState) (*x509.Certificate, bool) {
	if len(state.PeerCertificates) == 0 {
		return nil, false
	}
	return state.PeerCertificates[0], true
}
