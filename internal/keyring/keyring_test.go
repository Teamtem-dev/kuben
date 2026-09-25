package keyring_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/internal/keyring"
	"github.com/Teamtem-dev/kuben/internal/store"
)

func filled(b byte) [32]byte {
	var k [32]byte
	for i := range k {
		k[i] = b
	}
	return k
}

func ring(versions ...uint32) *keyring.Keyring {
	keys := map[uint32][32]byte{}
	for _, v := range versions {
		keys[v] = filled(byte(v))
	}
	return keyring.FromKeys(keys)
}

var who = keyring.Identity{Org: "org-1", Secret: "sec-1", Revision: 3}

func TestValuesRoundTrip(t *testing.T) {
	k := ring(1)
	plain := []byte(`{"DB_PASSWORD":"hunter2"}`)
	sealed, err := k.Seal(who, plain)
	if err != nil {
		t.Fatal(err)
	}
	if sealed.KeyVersion != 1 {
		t.Fatalf("key version %d", sealed.KeyVersion)
	}
	if bytes.Contains(sealed.Ciphertext, []byte("hunter2")) {
		t.Fatal("the value is in the clear")
	}
	opened, err := k.Open(who, sealed)
	if err != nil || !bytes.Equal(opened, plain) {
		t.Fatalf("open: %q %v", opened, err)
	}
	again, err := k.Seal(who, plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(again.Ciphertext, sealed.Ciphertext) {
		t.Fatal("fresh nonce and key every time")
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%x"} {
		if s := fmt.Sprintf(verb, sealed); strings.Contains(s, "hunter2") {
			t.Fatalf("%s shows the value: %s", verb, s)
		}
	}
}

func TestValuesAreSealedAsOneObject(t *testing.T) {
	k := ring(1)
	values := map[string]string{"url": "postgres://x"}
	sealed, err := k.SealValues(who, values)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := k.OpenValues(who, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(values, opened); diff != "" {
		t.Fatal(diff)
	}
	for _, text := range []string{`[1]`, `null`, `{"a":null}`, `{"a":1}`, `{"a":"b"} x`, "{\"a\":\"\xff\"}"} {
		notAnObject, err := k.Seal(who, []byte(text))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := k.OpenValues(who, notAnObject); !errors.Is(err, keyring.ErrOpen) {
			t.Fatalf("%q: %v", text, err)
		}
	}
}

func TestAValueMovedToAnotherIdentityDoesNotOpen(t *testing.T) {
	k := ring(1)
	sealed, err := k.Seal(who, []byte("v"))
	if err != nil {
		t.Fatal(err)
	}
	for _, other := range []keyring.Identity{
		{Org: "org-2", Secret: who.Secret, Revision: who.Revision},
		{Org: who.Org, Secret: "sec-2", Revision: who.Revision},
		{Org: who.Org, Secret: who.Secret, Revision: 4},
	} {
		if _, err := k.Open(other, sealed); !errors.Is(err, keyring.ErrOpen) {
			t.Fatalf("%+v: %v", other, err)
		}
	}
	tampered := sealed
	tampered.Ciphertext = bytes.Clone(sealed.Ciphertext)
	tampered.Ciphertext[len(tampered.Ciphertext)-1] ^= 1
	if _, err := k.Open(who, tampered); !errors.Is(err, keyring.ErrOpen) {
		t.Fatalf("tampered: %v", err)
	}
	relabelled := sealed
	relabelled.KeyVersion = 2
	var unknown *keyring.UnknownKeyError
	if _, err := k.Open(who, relabelled); !errors.As(err, &unknown) || unknown.Version != 2 {
		t.Fatalf("relabelled: %v", err)
	}
	truncated := sealed
	truncated.Ciphertext = []byte{1, 2}
	if _, err := k.Open(who, truncated); !errors.Is(err, keyring.ErrOpen) {
		t.Fatalf("truncated: %v", err)
	}
}

func TestRotationKeepsOldRevisionsReadableAndRewrapsThem(t *testing.T) {
	old := ring(1)
	sealed, err := old.Seal(who, []byte("v"))
	if err != nil {
		t.Fatal(err)
	}
	rotated := ring(1, 2)
	if rotated.Current() != 2 {
		t.Fatalf("current %d", rotated.Current())
	}
	if v, err := rotated.Open(who, sealed); err != nil || string(v) != "v" {
		t.Fatalf("old key: %q %v", v, err)
	}
	rewrapped, err := rotated.Rewrap(who, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if rewrapped.KeyVersion != 2 || !bytes.Equal(rewrapped.Ciphertext, sealed.Ciphertext) {
		t.Fatalf("rewrapped: %v, values untouched: %t", rewrapped, bytes.Equal(rewrapped.Ciphertext, sealed.Ciphertext))
	}
	retired := ring(2)
	if v, err := retired.Open(who, rewrapped); err != nil || string(v) != "v" {
		t.Fatalf("new key: %q %v", v, err)
	}
	if _, err := retired.Open(who, sealed); err == nil {
		t.Fatal("the retired key is gone")
	}
	wrong := keyring.FromKeys(map[uint32][32]byte{1: filled(9)})
	if _, err := wrong.Open(who, sealed); !errors.Is(err, keyring.ErrOpen) {
		t.Fatalf("another installation: %v", err)
	}
}

func TestRegistryLoginsBecomePullSecrets(t *testing.T) {
	login := keyring.RegistryLogin{Username: "bot", Password: "s3cret"}
	back, ok := keyring.RegistryLoginFrom(login.Values())
	if !ok || back != login {
		t.Fatalf("round trip: %v %t", back, ok)
	}
	if _, ok := keyring.RegistryLoginFrom(map[string]string{}); ok {
		t.Fatal("an empty secret is not a login")
	}
	if got := login.Basic(); got != "Basic Ym90OnMzY3JldA==" {
		t.Fatal(got)
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%x", "%d"} {
		if s := fmt.Sprintf(verb, login); strings.Contains(s, "s3cret") {
			t.Fatalf("%s shows the password: %s", verb, s)
		}
	}
	type entry struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Auth     string `json:"auth"`
	}
	var config struct {
		Auths map[string]entry `json:"auths"`
	}
	if err := json.Unmarshal([]byte(login.DockerConfig("ghcr.io")), &config); err != nil {
		t.Fatal(err)
	}
	if config.Auths["ghcr.io"].Auth != "Ym90OnMzY3JldA==" {
		t.Fatalf("%+v", config)
	}
	hub := login.DockerConfig(keyring.DockerHub)
	if err := json.Unmarshal([]byte(hub), &config); err != nil {
		t.Fatal(err)
	}
	if config.Auths["https://index.docker.io/v1/"].Username != "bot" {
		t.Fatalf("%+v", config)
	}
	// serde_json's bytes: keys sorted, no whitespace.
	want := `{"auths":{"https://index.docker.io/v1/":{"auth":"Ym90OnMzY3JldA==","password":"s3cret","username":"bot"}}}`
	if hub != want {
		t.Fatalf("docker config\n got %s\nwant %s", hub, want)
	}
}

func TestFingerprintsNameKeysWithoutRevealingThem(t *testing.T) {
	a := keyring.FromKeys(map[uint32][32]byte{1: filled(1), 2: filled(2)}).Fingerprints()
	b := keyring.FromKeys(map[uint32][32]byte{1: filled(1), 2: filled(9)}).Fingerprints()
	if len(a) != 2 || a[0] != b[0] || a[1] == b[1] {
		t.Fatalf("%v %v", a, b)
	}
	if a[0].SHA256 == filled(1) {
		t.Fatal("the fingerprint is the key")
	}
}

func TestKeyringsParseStrictly(t *testing.T) {
	dir := t.TempDir()
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	install := func(name, text string) (*keyring.Keyring, error) {
		return keyring.Install(filepath.Join(dir, name), text)
	}
	k, err := install("good", "# comment\n\n1:"+key+"\n 3 : "+key+"\r\n+4:"+key+"\n")
	if err != nil {
		t.Fatal(err)
	}
	if k.Current() != 4 {
		t.Fatalf("current %d", k.Current())
	}
	// A last base64 character with bits the key does not use.
	sloppy := key[:len(key)-2] + "d="
	for i, bad := range []string{
		"",
		"0:" + key,
		"1:" + key + "\n1:" + key,
		"1:short",
		"x:" + key,
		key,
		"1:" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 16)),
		"1:" + sloppy,
		"-1:" + key,
		"++1:" + key,
		"4294967296:" + key,
		"1:" + key[:20] + "\r" + key[20:],
	} {
		if _, err := install(fmt.Sprintf("bad%d", i), bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%x"} {
		if s := fmt.Sprintf(verb, k); strings.Contains(s, key) || strings.Contains(s, "7 7 7") {
			t.Fatalf("%s shows a key: %s", verb, s)
		}
	}
}

func TestANewKeyringIsPrivateAndReused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "secrets.keyring")
	first, err := keyring.LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := first.Seal(who, []byte("v"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := keyring.LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := second.Open(who, sealed); err != nil || string(v) != "v" {
		t.Fatalf("same key: %q %v", v, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := keyring.LoadOrCreate(path); err != nil {
		t.Fatalf("group read, as fsGroup makes it: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	var keyringErr *keyring.Error
	if _, err := keyring.LoadOrCreate(path); !errors.As(err, &keyringErr) {
		t.Fatalf("readable by others: %v", err)
	}
}

func TestARestoredKeyringIsInstalledOnceAndLoaded(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	path := filepath.Join(dir, "secrets.keyring")
	if _, err := keyring.Load(path); err == nil {
		t.Fatal("missing")
	}
	text := "1:" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32)) + "\n"
	installed, err := keyring.Install(path, text)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keyring.Install(path, text); err == nil {
		t.Fatal("never over an existing file")
	}
	if _, err := keyring.Install(filepath.Join(dir, "other"), "garbage"); err == nil {
		t.Fatal("garbage installed")
	}
	loaded, err := keyring.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(installed.Fingerprints(), loaded.Fingerprints()); diff != "" {
		t.Fatal(diff)
	}
}

func TestRevisionObjectsAreNamedAfterTheSecret(t *testing.T) {
	if got := keyring.ObjectName("db", 12); got != "db.r12" {
		t.Fatal(got)
	}
}

// secretFixture is testdata/compat/secrets.json, written by the Rust
// keyring (crates/kuben-platform/src/compat_fixtures.rs).
type secretFixture struct {
	Keys []struct {
		Version uint32 `json:"version"`
		Key     []byte `json:"key"`
	} `json:"keys"`
	Identity struct {
		Org      string `json:"org"`
		Secret   string `json:"secret"`
		Revision uint64 `json:"revision"`
	} `json:"identity"`
	Values map[string]string `json:"values"`
	Sealed struct {
		Ciphertext []byte `json:"ciphertext"`
		WrappedKey []byte `json:"wrappedKey"`
		KeyVersion uint32 `json:"keyVersion"`
	} `json:"sealed"`
	Fingerprints []struct {
		Version uint32 `json:"version"`
		SHA256  []byte `json:"sha256"`
	} `json:"fingerprints"`
}

func TestRustSealedSecretsOpen(t *testing.T) {
	data, err := os.ReadFile("../../testdata/compat/secrets.json")
	if err != nil {
		t.Fatal(err)
	}
	var f secretFixture
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	keys := map[uint32][32]byte{}
	for _, k := range f.Keys {
		keys[k.Version] = [32]byte(k.Key)
	}
	k := keyring.FromKeys(keys)
	for i, fp := range k.Fingerprints() {
		want := f.Fingerprints[i]
		if fp.Version != want.Version || !bytes.Equal(fp.SHA256[:], want.SHA256) {
			t.Fatalf("fingerprint %d: %x, Rust %x", fp.Version, fp.SHA256, want.SHA256)
		}
	}
	id := keyring.Identity{Org: f.Identity.Org, Secret: f.Identity.Secret, Revision: f.Identity.Revision}
	sealed := store.SealedBytes{Ciphertext: f.Sealed.Ciphertext, WrappedKey: f.Sealed.WrappedKey, KeyVersion: f.Sealed.KeyVersion}
	// The plaintext is serde_json's text of the BTreeMap: Go must write the
	// same bytes when it seals.
	plain, err := k.Open(id, sealed)
	if err != nil {
		t.Fatal(err)
	}
	ours, err := keyring.ValuesJSON(f.Values)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plain, ours) {
		t.Fatalf("plaintext\nRust %s\n  Go %s", plain, ours)
	}
	values, err := k.OpenValues(id, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(f.Values, values); diff != "" {
		t.Fatal(diff)
	}
	// The layout: nonce (12) ‖ ciphertext ‖ tag (16); the data key is 32.
	if len(sealed.Ciphertext) != 12+len(plain)+16 || len(sealed.WrappedKey) != 12+32+16 {
		t.Fatalf("layout: %d, %d", len(sealed.Ciphertext), len(sealed.WrappedKey))
	}
	// What Go seals opens under the Rust identity rules and moves to v2.
	resealed, err := k.SealValues(id, f.Values)
	if err != nil {
		t.Fatal(err)
	}
	if resealed.KeyVersion != 2 || len(resealed.Ciphertext) != len(sealed.Ciphertext) {
		t.Fatalf("resealed: %v %d", resealed, len(resealed.Ciphertext))
	}
	other := id
	other.Revision++
	if _, err := k.Open(other, sealed); !errors.Is(err, keyring.ErrOpen) {
		t.Fatalf("another revision: %v", err)
	}
}
