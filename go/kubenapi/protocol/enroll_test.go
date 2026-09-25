package protocol_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"fmt"
	"strings"
	"testing"

	"github.com/Teamtem-dev/kuben/go/kubenapi/protocol"
)

// Ported from crates/kuben-agent/src/enroll.rs (the tests of the parts both
// ends share; the hub's are in go/hub/internal/platform/agentlink).

func TestADeviceKeySurvivesItsPEMAndKeepsItsID(t *testing.T) {
	key, err := protocol.GenerateDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	text, err := key.PEM()
	if err != nil {
		t.Fatal(err)
	}
	back, err := protocol.ParseDeviceKey(text)
	if err != nil {
		t.Fatal(err)
	}
	if back.DeviceID() != key.DeviceID() || !strings.HasPrefix(key.DeviceID(), "sha256:") {
		t.Fatal(back.DeviceID(), key.DeviceID())
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		if shown := fmt.Sprintf(verb, key); strings.Contains(shown, "PRIVATE") || strings.Contains(shown, "D:") {
			t.Errorf("%s shows the key: %s", verb, shown)
		}
	}
	token := protocol.Enroll{Token: protocol.NewToken("kbt_secret"), ClusterID: "primary"}
	if strings.Contains(fmt.Sprintf("%#v", token), "kbt_secret") {
		t.Fatal("a token never shows in Debug")
	}
	if protocol.DeviceIDOf(key.PublicKeyInfo()) != key.DeviceID() {
		t.Fatal("device_id_of")
	}
}

// The key and CSR have rcgen's shape: an ECDSA P-256 key as PKCS#8 PEM, a
// CSR whose subject is one UTF8String common name, signed with
// ECDSA-SHA256.
func TestTheDeviceKeyAndItsCSRHaveTheShapeRcgenGaveThem(t *testing.T) {
	key, err := protocol.GenerateDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	text, err := key.PEM()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(text, "-----BEGIN PRIVATE KEY-----\n") || !strings.HasSuffix(text, "-----END PRIVATE KEY-----\n") {
		t.Fatal(text)
	}
	if k, ok := key.Signer().(*ecdsa.PrivateKey); !ok || k.Curve != elliptic.P256() {
		t.Fatalf("%T", key.Signer())
	}
	csrPEM, err := key.CSR("primary")
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(csrPEM))
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		t.Fatal(csrPEM)
	}
	req, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if req.Subject.CommonName != "cluster:primary" || req.SignatureAlgorithm != x509.ECDSAWithSHA256 {
		t.Fatal(req.Subject, req.SignatureAlgorithm)
	}
	subject, err := protocol.UTF8Subject("cluster:primary")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(req.RawSubject, subject) {
		t.Fatal("the subject is not rcgen's")
	}
	// SEQUENCE { SET { SEQUENCE { OID 2.5.4.3, UTF8String } } }
	want := append([]byte{0x30, 0x1a, 0x31, 0x18, 0x30, 0x16, 0x06, 0x03, 0x55, 0x04, 0x03, 0x0c, 0x0f}, "cluster:primary"...)
	if !bytes.Equal(subject, want) {
		t.Fatalf("% x", subject)
	}
	var tag asn1.RawValue
	if _, err := asn1.Unmarshal(subject[6+5:], &tag); err != nil || tag.Tag != asn1.TagUTF8String {
		t.Fatal(tag, err)
	}
	csr, err := protocol.ParseCSR(csrPEM)
	if err != nil {
		t.Fatal(err)
	}
	if csr.DeviceID() != key.DeviceID() {
		t.Fatal("the CSR names the key's device")
	}
	if _, err := protocol.ParseCSR("not a csr"); err == nil {
		t.Fatal("garbage parsed")
	}
	// A CSR whose signature does not verify is refused.
	tampered := append([]byte(nil), block.Bytes...)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := protocol.ParseCSR(protocol.EncodePEM("CERTIFICATE REQUEST", tampered)); err == nil {
		t.Fatal("a CSR with a broken signature parsed")
	}
}
