package agentlink

// Enrollment, the parts both ends share (enroll.rs; plan §11.4): the
// agent's device key and CSR, the device id, and the agent's side of the
// exchange. The hub's side (the cluster CA, the tokens) lives in the hub.
//
// The device key is ECDSA P-256 with SHA-256, as rcgen's KeyPair::generate
// made it, kept as PKCS#8 PEM ("PRIVATE KEY"); names are UTF8String common
// names, as rcgen wrote them.

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
)

// HexDigest is `sha256:` and the hex SHA-256 of b.
func HexDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// DeviceIDOf is the device id of a public key: `sha256:` of its
// SubjectPublicKeyInfo (DER).
func DeviceIDOf(publicKeyInfo []byte) string { return HexDigest(publicKeyInfo) }

// ClusterSubject is the common name of a cluster's agent certificate.
func ClusterSubject(clusterID string) string { return "cluster:" + clusterID }

// oidCommonName is id-at-commonName.
var oidCommonName = asn1.ObjectIdentifier{2, 5, 4, 3} //nolint:gochecknoglobals // a constant OID

// UTF8Subject is the DER of a name holding the common name cn as a
// UTF8String, as rcgen wrote names (crypto/x509 would choose
// PrintableString where it fits).
func UTF8Subject(cn string) ([]byte, error) {
	value := asn1.RawValue{Tag: asn1.TagUTF8String, Class: asn1.ClassUniversal, Bytes: []byte(cn)}
	der, err := asn1.Marshal(pkix.RDNSequence{{{Type: oidCommonName, Value: value}}})
	if err != nil {
		return nil, fmt.Errorf("certificate: %w", err)
	}
	return der, nil
}

// firstPEM is the DER of the first PEM section of text whose label is
// label, or of the first section at all when label is empty.
func firstPEM(text []byte, label string) ([]byte, error) {
	for {
		block, rest := pem.Decode(text)
		if block == nil {
			return nil, errors.New("no valid PEM section found")
		}
		if label == "" || block.Type == label {
			return block.Bytes, nil
		}
		text = rest
	}
}

// ParseCertificatePEM is the first certificate in a PEM text.
func ParseCertificatePEM(text string) (*x509.Certificate, error) {
	der, err := firstPEM([]byte(text), "CERTIFICATE")
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("certificate: %w", err)
	}
	return cert, nil
}

// ParseCertificatesPEM is every certificate in a PEM text, in order.
func ParseCertificatesPEM(text []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	for {
		block, rest := pem.Decode(text)
		if block == nil {
			return out, nil
		}
		text = rest
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("certificate: %w", err)
		}
		out = append(out, cert)
	}
}

// EncodePEM is der as a PEM section labelled label.
func EncodePEM(label string, der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: label, Bytes: der}))
}

// PrivateKey is a signing key: its public key's SubjectPublicKeyInfo and
// its PKCS#8 PEM. It never prints its secret.
type PrivateKey struct {
	signer crypto.Signer
}

// GenerateKey is a new ECDSA P-256 key.
func GenerateKey() (PrivateKey, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return PrivateKey{}, fmt.Errorf("certificate: %w", err)
	}
	return PrivateKey{signer: k}, nil
}

// ParsePrivateKey reads a PKCS#8 key in PEM.
func ParsePrivateKey(text string) (PrivateKey, error) {
	der, err := firstPEM([]byte(text), "")
	if err != nil {
		return PrivateKey{}, fmt.Errorf("certificate: %w", err)
	}
	k, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return PrivateKey{}, fmt.Errorf("certificate: %w", err)
	}
	signer, ok := k.(crypto.Signer)
	if !ok {
		return PrivateKey{}, errors.New("certificate: the key cannot sign")
	}
	return PrivateKey{signer: signer}, nil
}

// Signer is the key, for signing certificates and TLS.
func (k PrivateKey) Signer() crypto.Signer { return k.signer }

// Valid says whether the key was made or read (the zero PrivateKey is not).
func (k PrivateKey) Valid() bool { return k.signer != nil }

// PEM is the key as PKCS#8 PEM ("PRIVATE KEY").
func (k PrivateKey) PEM() (string, error) {
	der, err := x509.MarshalPKCS8PrivateKey(k.signer)
	if err != nil {
		return "", fmt.Errorf("certificate: %w", err)
	}
	return EncodePEM("PRIVATE KEY", der), nil
}

// PublicKeyInfo is the public key as a SubjectPublicKeyInfo (DER).
func (k PrivateKey) PublicKeyInfo() []byte {
	der, err := x509.MarshalPKIXPublicKey(k.signer.Public())
	if err != nil {
		return nil
	}
	return der
}

// String hides the key.
func (PrivateKey) String() string { return "PrivateKey(***)" }

// GoString hides the key from %#v.
func (PrivateKey) GoString() string { return "PrivateKey(***)" }

// DeviceKey is the agent's device key, generated where it is used; the
// digest of its public key is the device id.
type DeviceKey struct {
	PrivateKey
}

// GenerateDeviceKey is a new device key.
func GenerateDeviceKey() (DeviceKey, error) {
	k, err := GenerateKey()
	return DeviceKey{k}, err
}

// ParseDeviceKey reads a device key kept in PEM.
func ParseDeviceKey(text string) (DeviceKey, error) {
	k, err := ParsePrivateKey(text)
	return DeviceKey{k}, err
}

// DeviceID is `sha256:` of the public key (its SubjectPublicKeyInfo).
func (k DeviceKey) DeviceID() string { return DeviceIDOf(k.PublicKeyInfo()) }

// String names the device, never the key.
func (k DeviceKey) String() string {
	return fmt.Sprintf("DeviceKey { device_id: %q, .. }", k.DeviceID())
}

// GoString names the device, never the key.
func (k DeviceKey) GoString() string { return k.String() }

// CSR is a CSR (PEM) for clusterID, signed by this key.
func (k DeviceKey) CSR(clusterID string) (string, error) {
	subject, err := UTF8Subject(ClusterSubject(clusterID))
	if err != nil {
		return "", err
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{RawSubject: subject}, k.signer)
	if err != nil {
		return "", fmt.Errorf("certificate: %w", err)
	}
	return EncodePEM("CERTIFICATE REQUEST", der), nil
}

// Identity is the TLS identity of this key with the certificate the hub
// issued (PEM).
func (k DeviceKey) Identity(certificatePEM string) (tls.Certificate, error) {
	cert, err := ParseCertificatePEM(certificatePEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("the certificate from the hub is not valid PEM: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{cert.Raw}, PrivateKey: k.signer, Leaf: cert}, nil
}

// CSR is a certificate signing request whose signature was checked: the
// sender holds the key.
type CSR struct {
	request *x509.CertificateRequest
}

// ParseCSR reads a CSR in PEM and checks its signature.
func ParseCSR(text string) (CSR, error) {
	der, err := firstPEM([]byte(text), "")
	if err != nil {
		return CSR{}, fmt.Errorf("certificate: %w", err)
	}
	r, err := x509.ParseCertificateRequest(der)
	if err != nil {
		return CSR{}, fmt.Errorf("certificate: %w", err)
	}
	if err := r.CheckSignature(); err != nil {
		return CSR{}, fmt.Errorf("certificate: %w", err)
	}
	return CSR{request: r}, nil
}

// DeviceID is `sha256:` of the requested public key: the device id.
func (c CSR) DeviceID() string { return DeviceIDOf(c.request.RawSubjectPublicKeyInfo) }

// PublicKey is the requested public key: the only part of a CSR the hub
// uses.
func (c CSR) PublicKey() crypto.PublicKey { return c.request.PublicKey }

// EnrollRefusedError is the hub refusing to enroll this agent.
type EnrollRefusedError struct {
	Reason  Refusal
	Message string
}

func (e EnrollRefusedError) Error() string {
	return fmt.Sprintf("the hub refused to enroll this agent (%s): %s", e.Reason.Error(), e.Message)
}

// The hub's other answers an enrolling agent cannot use.
var (
	// ErrEnrollClosed: the hub closed the link before answering.
	ErrEnrollClosed = errors.New("the hub closed the link before answering") //nolint:gochecknoglobals // a sentinel error
	// ErrEnrollUnexpected: the hub answered with another message.
	ErrEnrollUnexpected = errors.New("the hub answered with an unexpected message") //nolint:gochecknoglobals // a sentinel error
)

// RequestEnrollment is the agent's side: enroll key into clusterID with
// token over stream, a TLS connection to the pinned hub without a client
// certificate. The answer is the certificate (PEM) and its expiry.
func RequestEnrollment(stream io.ReadWriter, token, clusterID string, key DeviceKey) (Enrolled, error) {
	csr, err := key.CSR(clusterID)
	if err != nil {
		return Enrolled{}, err
	}
	if err := WriteFrame(stream, Enroll{Token: NewToken(token), ClusterID: clusterID, CSR: csr}); err != nil {
		return Enrolled{}, err
	}
	m, ok, err := ReadFrame(stream)
	switch {
	case err != nil:
		return Enrolled{}, err
	case !ok:
		return Enrolled{}, ErrEnrollClosed
	}
	switch m := m.(type) {
	case Enrolled:
		return m, nil
	case Refused:
		return Enrolled{}, EnrollRefusedError(m)
	case Hello, Welcome, Heartbeat, HeartbeatAck, Enroll, Renew, Apply, Observation, Unknown:
	}
	return Enrolled{}, ErrEnrollUnexpected
}
