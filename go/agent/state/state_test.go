package state_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Teamtem-dev/kuben/go/agent/state"
	"github.com/Teamtem-dev/kuben/go/kubenapi/protocol"
)

// Ported from crates/kuben-agent/src/state.rs. The hub's ClusterCA lives in
// the hub module; the certificates here are made the same way with
// crypto/x509.

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestTheDeviceKeyIsMadeOnceAndKeptPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	key, err := s.DeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.DeviceKey()
	if err != nil || again.DeviceID() != key.DeviceID() {
		t.Fatal("read back another key", err)
	}
	if m := mode(t, filepath.Join(dir, state.DeviceKey)); m != 0o600 {
		t.Fatalf("key mode %o", m)
	}
	if m := mode(t, dir); m != 0o700 {
		t.Fatalf("dir mode %o", m)
	}
}

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newCA(t *testing.T) testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "kuben cluster CA"},
		NotBefore: time.Unix(0, 0), NotAfter: time.Unix(1<<33, 0), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign,
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

// issue is a client certificate for key, valid from 5 minutes before now
// for lifetime, as the hub's ClusterCA issues them.
func (c testCA) issue(t *testing.T, key protocol.DeviceKey, now time.Time, lifetime time.Duration) string {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "cluster:primary"},
		NotBefore: now.Add(-5 * time.Minute), NotAfter: now.Add(lifetime),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, key.Signer().Public(), c.key)
	if err != nil {
		t.Fatal(err)
	}
	return protocol.EncodePEM("CERTIFICATE", der)
}

func TestAStoredCertificateCarriesItsExpiryAndKey(t *testing.T) {
	dir := t.TempDir()
	s, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.Certificate(); err != nil || found {
		t.Fatal("no certificate yet", found, err)
	}
	key, err := s.DeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_789_000_000, 0)
	pem := newCA(t).issue(t, key, now, 24*time.Hour)
	if err := s.SaveCertificate(pem); err != nil {
		t.Fatal(err)
	}
	stored, found, err := s.Certificate()
	if err != nil || !found {
		t.Fatal(found, err)
	}
	if !stored.NotAfter.Equal(now.Add(24*time.Hour)) || !stored.NotBefore.Equal(now.Add(-5*time.Minute)) ||
		!bytes.Equal(stored.PublicKeyInfo, key.PublicKeyInfo()) {
		t.Fatalf("%+v", stored)
	}
	if m := mode(t, filepath.Join(dir, state.Certificate)); m != 0o600 {
		t.Fatalf("certificate mode %o", m)
	}
	if err := os.WriteFile(filepath.Join(dir, state.Certificate), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Certificate(); !errors.As(err, new(state.InvalidError)) {
		t.Fatal(err)
	}
}

func TestTokensComeTrimmedFromAFileAndNeverFromNowhere(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("kbt_abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if token, ok, err := state.ReadToken(state.TokenFile{Path: file}, nil); err != nil || !ok || token != "kbt_abc" {
		t.Fatal(token, ok, err)
	}
	if err := os.WriteFile(file, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := state.ReadToken(state.TokenFile{Path: file}, nil); err != nil || ok {
		t.Fatal("a blank file holds no token", err)
	}
	if _, ok, err := state.ReadToken(state.NoToken{}, nil); err != nil || ok {
		t.Fatal("none", err)
	}
	if token, ok, err := state.ReadToken(state.TokenStdin{}, strings.NewReader("\tkbt_stdin \n")); err != nil || !ok || token != "kbt_stdin" {
		t.Fatal(token, ok, err)
	}
}

func TestThePinnedCAFileMustHoldACertificate(t *testing.T) {
	ca := newCA(t)
	file := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(file, []byte(protocol.EncodePEM("CERTIFICATE", ca.cert.Raw)), 0o600); err != nil {
		t.Fatal(err)
	}
	pinned, err := state.PinnedCA(file)
	if err != nil || len(pinned) != 1 || !bytes.Equal(pinned[0].Raw, ca.cert.Raw) {
		t.Fatal(pinned, err)
	}
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := state.PinnedCA(file); !errors.As(err, new(state.InvalidError)) {
		t.Fatal(err)
	}
}
