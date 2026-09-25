package agentlink

// The cluster CA that issues agent identities (enroll.rs ClusterCa). The
// certificates have the shape rcgen gave them: ECDSA P-256 keys signed with
// ECDSA-SHA256, UTF8String common names, the same extensions, validity and
// serial rules; only the order of the extensions differs (crypto/x509
// writes its own order), which no verifier reads.

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"time"

	"github.com/Teamtem-dev/kuben/go/kubenapi/protocol"
)

// certSkew is how far before "now" an issued certificate starts, for
// clocks that lag.
const certSkew = 5 * time.Minute

// ClusterCA is the CA that issues agent identities. Its private key never
// prints.
type ClusterCA struct {
	cert    *x509.Certificate
	key     protocol.PrivateKey
	certPEM string
	keyPEM  string
	// keyID is the authority key identifier of what it issues: its subject
	// key identifier, or rcgen's truncated SHA-256 of its public key.
	keyID []byte
}

// Issued is a certificate the hub issued.
type Issued struct {
	// ClusterID is the cluster the certificate names.
	ClusterID      string
	CertificatePEM string
	DeviceID       string
	NotAfter       time.Time
}

// keyIdentifier is rcgen's KeyIdMethod::Sha256: the first 20 bytes of the
// SHA-256 of the whole SubjectPublicKeyInfo.
func keyIdentifier(spki []byte) []byte {
	sum := sha256.Sum256(spki)
	return sum[:20]
}

// randomSerial is 16 random bytes with the top bit cleared: a positive DER
// integer.
func randomSerial() (*big.Int, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, fmt.Errorf("certificate: %w", err)
	}
	b[0] &= 0x7f
	return new(big.Int).SetBytes(b[:]), nil
}

// GenerateClusterCA is a new self-signed CA named name: rcgen's defaults
// (valid from 1975 to 4096, the serial derived from the public key), a
// path length of 0, key usages keyCertSign and cRLSign.
func GenerateClusterCA(name string) (ClusterCA, error) {
	key, err := protocol.GenerateKey()
	if err != nil {
		return ClusterCA{}, err
	}
	subject, err := protocol.UTF8Subject(name)
	if err != nil {
		return ClusterCA{}, err
	}
	spki := key.PublicKeyInfo()
	parsed, err := x509.ParsePKIXPublicKey(spki)
	if err != nil {
		return ClusterCA{}, fmt.Errorf("certificate: %w", err)
	}
	serial, err := publicKeySerial(parsed)
	if err != nil {
		return ClusterCA{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		RawSubject:            subject,
		NotBefore:             time.Date(1975, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:              time.Date(4096, 1, 1, 0, 0, 0, 0, time.UTC),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		SubjectKeyId:          keyIdentifier(spki),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Signer().Public(), key.Signer())
	if err != nil {
		return ClusterCA{}, fmt.Errorf("certificate: %w", err)
	}
	keyPEM, err := key.PEM()
	if err != nil {
		return ClusterCA{}, err
	}
	return ClusterCAFromPEM(protocol.EncodePEM("CERTIFICATE", der), keyPEM)
}

// publicKeySerial is rcgen's serial for a certificate without one: the
// first 20 bytes of the SHA-256 of the raw public key, top bit cleared.
func publicKeySerial(pub any) (*big.Int, error) {
	var raw []byte
	switch k := pub.(type) {
	case interface{ Bytes() ([]byte, error) }:
		b, err := k.Bytes()
		if err != nil {
			return nil, fmt.Errorf("certificate: %w", err)
		}
		raw = b
	default:
		return randomSerial()
	}
	sum := sha256.Sum256(raw)
	serial := sum[:20]
	serial[0] &= 0x7f
	return new(big.Int).SetBytes(serial), nil
}

// ClusterCAFromPEM is a CA kept earlier: its certificate and private key in
// PEM.
func ClusterCAFromPEM(certificatePEM, keyPEM string) (ClusterCA, error) {
	cert, err := protocol.ParseCertificatePEM(certificatePEM)
	if err != nil {
		return ClusterCA{}, fmt.Errorf("the CA certificate is not valid PEM: %w", err)
	}
	key, err := protocol.ParsePrivateKey(keyPEM)
	if err != nil {
		return ClusterCA{}, err
	}
	keyID := cert.SubjectKeyId
	if len(keyID) == 0 {
		keyID = keyIdentifier(cert.RawSubjectPublicKeyInfo)
	}
	return ClusterCA{cert: cert, key: key, certPEM: certificatePEM, keyPEM: keyPEM, keyID: keyID}, nil
}

// Certificate is the CA certificate agents pin.
func (c ClusterCA) Certificate() *x509.Certificate { return c.cert }

// CertificatePEM is the CA certificate in PEM.
func (c ClusterCA) CertificatePEM() string { return c.certPEM }

// KeyPEM is the CA's private key; keep it readable by its owner only.
func (c ClusterCA) KeyPEM() string { return c.keyPEM }

// String hides the key.
func (ClusterCA) String() string { return "ClusterCa { .. }" }

// GoString hides the key from %#v.
func (c ClusterCA) GoString() string { return c.String() }

// Valid says whether the CA was made or read (the zero ClusterCA is not).
func (c ClusterCA) Valid() bool { return c.cert != nil && c.key.Valid() }

// issuer is the CA as the parent of a certificate; without its subject key
// identifier crypto/x509 writes no authority key identifier of its own.
func (c ClusterCA) issuer() *x509.Certificate {
	parent := *c.cert
	parent.SubjectKeyId = nil
	return &parent
}

// Issue is a client certificate for clusterID, bound to the CSR's key and
// valid for lifetime from now. Only the CSR's public key is used: the
// subject, names, key usage and lifetime are the hub's.
func (c ClusterCA) Issue(clusterID string, csr protocol.CSR, lifetime time.Duration, now time.Time) (Issued, error) {
	if !c.Valid() {
		return Issued{}, errors.New("certificate: no CA")
	}
	subject, err := protocol.UTF8Subject(protocol.ClusterSubject(clusterID))
	if err != nil {
		return Issued{}, err
	}
	uri, err := url.Parse("kuben://cluster/" + clusterID)
	if err != nil {
		return Issued{}, fmt.Errorf("certificate: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return Issued{}, err
	}
	notAfter := now.Add(lifetime)
	tmpl := &x509.Certificate{
		SerialNumber:   serial,
		RawSubject:     subject,
		NotBefore:      now.Add(-certSkew),
		NotAfter:       notAfter,
		URIs:           []*url.URL{uri},
		KeyUsage:       x509.KeyUsageDigitalSignature,
		ExtKeyUsage:    []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		AuthorityKeyId: c.keyID,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.issuer(), csr.PublicKey(), c.key.Signer())
	if err != nil {
		return Issued{}, fmt.Errorf("certificate: %w", err)
	}
	return Issued{
		ClusterID:      clusterID,
		CertificatePEM: protocol.EncodePEM("CERTIFICATE", der),
		DeviceID:       csr.DeviceID(),
		NotAfter:       notAfter,
	}, nil
}

// ServerIdentity is a server identity for the hub named name, with a key
// of its own, valid for lifetime from now.
func (c ClusterCA) ServerIdentity(name string, lifetime time.Duration, now time.Time) (tls.Certificate, error) {
	if !c.Valid() {
		return tls.Certificate{}, errors.New("certificate: no CA")
	}
	key, err := protocol.GenerateKey()
	if err != nil {
		return tls.Certificate{}, err
	}
	subject, err := protocol.UTF8Subject("kuben-hub")
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := randomSerial()
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		RawSubject:   subject,
		NotBefore:    now.Add(-certSkew),
		NotAfter:     now.Add(lifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	// rcgen made an IP name of a name that parses as one.
	if ip := net.ParseIP(name); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{name}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.issuer(), key.Signer().Public(), c.key.Signer())
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("certificate: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key.Signer(), Leaf: leaf}, nil
}
