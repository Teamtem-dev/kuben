package keyring

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Teamtem-dev/kuben/internal/jsonx"
	"github.com/Teamtem-dev/kuben/internal/store"
)

const (
	keyLen = 32
	// nonceLen is AES-GCM's standard nonce (ring's NONCE_LEN).
	nonceLen = 12
	// maxVersions is the most keys a keyring holds.
	maxVersions = 64
)

// ErrOpen is a sealed value that does not open: wrong key, identity or
// content (SecretError::Open).
var ErrOpen = errors.New("a sealed value does not open: wrong key, identity or content")

// ErrRandom is a failure of the system random source (SecretError::Random).
var ErrRandom = errors.New("the system random source failed")

// Error is a keyring file that cannot be read, written or parsed
// (SecretError::Keyring).
type Error struct {
	Path   string
	Reason string
}

func (e *Error) Error() string {
	return "cannot read the keyring " + e.Path + ": " + e.Reason
}

// UnknownKeyError is a sealed value naming a key version the keyring does
// not hold (SecretError::UnknownKey).
type UnknownKeyError struct{ Version uint32 }

func (e *UnknownKeyError) Error() string {
	return "the keyring has no key version " + strconv.FormatUint(uint64(e.Version), 10)
}

// Identity is who a sealed value belongs to: the associated data of both
// seals.
type Identity struct {
	Org      string
	Secret   string
	Revision uint64
}

func (w Identity) valueAAD() []byte {
	return []byte("kuben/secret/v1|" + w.Org + "|" + w.Secret + "|" + strconv.FormatUint(w.Revision, 10))
}

func (w Identity) keyAAD(version uint32) []byte {
	return []byte("kuben/dek/v1|" + w.Org + "|" + w.Secret + "|" + strconv.FormatUint(w.Revision, 10) +
		"|" + strconv.FormatUint(uint64(version), 10))
}

// Keyring is the key-encryption keys, by version. Its zero value holds no
// key: it seals nothing and opens nothing. Formatting it shows the
// versions, never a key.
type Keyring struct {
	keys map[uint32][keyLen]byte
	// versions is the keys' versions, ascending.
	versions []uint32
}

// FromKeys is a keyring of exactly these keys (tests, restores).
func FromKeys(keys map[uint32][keyLen]byte) *Keyring {
	k := &Keyring{keys: maps.Clone(keys)}
	k.versions = slices.Sorted(maps.Keys(k.keys))
	return k
}

// Format hides the keys from every verb.
func (k *Keyring) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "Keyring { versions: %v, .. }", k.versions) //nolint:errcheck // fmt.Formatter cannot report a write error
}

// Current is the version that seals new values; 0 for an empty keyring.
func (k *Keyring) Current() uint32 {
	if len(k.versions) == 0 {
		return 0
	}
	return k.versions[len(k.versions)-1]
}

// Fingerprints is every version with the fingerprint of its key,
// sha256("kuben/kek-fingerprint/v1|" ‖ key), by version: it identifies the
// key without revealing it.
func (k *Keyring) Fingerprints() []store.KeyFingerprint {
	out := make([]store.KeyFingerprint, 0, len(k.versions))
	for _, v := range k.versions {
		h := sha256.New()
		h.Write([]byte("kuben/kek-fingerprint/v1|"))
		key := k.keys[v]
		h.Write(key[:])
		var f store.KeyFingerprint
		f.Version = v
		copy(f.SHA256[:], h.Sum(nil))
		out = append(out, f)
	}
	return out
}

func (k *Keyring) key(version uint32) ([]byte, error) {
	key, ok := k.keys[version]
	if !ok {
		return nil, &UnknownKeyError{Version: version}
	}
	return key[:], nil
}

func aead(key []byte) (cipher.AEAD, error) {
	// ring accepted AES-256 keys only; aes.NewCipher would take a 16 or
	// 24 byte data key as AES-128 or AES-192.
	if len(key) != keyLen {
		return nil, ErrOpen
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrOpen
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrOpen
	}
	return gcm, nil
}

// sealWith is nonce ‖ ciphertext ‖ tag of plaintext under key, bound to aad.
func sealWith(key []byte, aad, plaintext []byte) ([]byte, error) {
	gcm, err := aead(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, nonceLen, nonceLen+len(plaintext)+gcm.Overhead())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, ErrRandom
	}
	return gcm.Seal(nonce, nonce, plaintext, aad), nil
}

func openWith(key, aad, sealed []byte) ([]byte, error) {
	if len(sealed) < nonceLen {
		return nil, ErrOpen
	}
	gcm, err := aead(key)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, sealed[:nonceLen], sealed[nonceLen:], aad)
	if err != nil {
		return nil, ErrOpen
	}
	return plain, nil
}

// Seal seals plaintext for who under the current key, with a fresh data
// key.
func (k *Keyring) Seal(who Identity, plaintext []byte) (store.SealedBytes, error) {
	version := k.Current()
	kek, err := k.key(version)
	if err != nil {
		return store.SealedBytes{}, err
	}
	dek := make([]byte, keyLen)
	defer clear(dek)
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		return store.SealedBytes{}, ErrRandom
	}
	ciphertext, err := sealWith(dek, who.valueAAD(), plaintext)
	if err != nil {
		return store.SealedBytes{}, err
	}
	wrapped, err := sealWith(kek, who.keyAAD(version), dek)
	if err != nil {
		return store.SealedBytes{}, err
	}
	return store.SealedBytes{Ciphertext: ciphertext, WrappedKey: wrapped, KeyVersion: version}, nil
}

func (k *Keyring) openKey(who Identity, sealed store.SealedBytes) ([]byte, error) {
	kek, err := k.key(sealed.KeyVersion)
	if err != nil {
		return nil, err
	}
	return openWith(kek, who.keyAAD(sealed.KeyVersion), sealed.WrappedKey)
}

// Open opens sealed, which must belong to who.
func (k *Keyring) Open(who Identity, sealed store.SealedBytes) ([]byte, error) {
	dek, err := k.openKey(who, sealed)
	if err != nil {
		return nil, err
	}
	defer clear(dek)
	return openWith(dek, who.valueAAD(), sealed.Ciphertext)
}

// SealValues seals the values of a revision: a JSON object of key to value,
// written as serde_json wrote a BTreeMap (keys in byte order, no
// whitespace, serde's string escaping).
func (k *Keyring) SealValues(who Identity, values map[string]string) (store.SealedBytes, error) {
	text, err := ValuesJSON(values)
	if err != nil {
		return store.SealedBytes{}, ErrOpen
	}
	return k.Seal(who, text)
}

// ValuesJSON is the plaintext SealValues seals: values as serde_json wrote
// a BTreeMap<String, String>.
func ValuesJSON(values map[string]string) ([]byte, error) {
	if values == nil {
		values = map[string]string{}
	}
	text, err := jsonx.CanonicalValue(values)
	if err != nil {
		return nil, fmt.Errorf("secret values: %w", err)
	}
	return []byte(text), nil
}

// OpenValues opens the values of a revision sealed by SealValues.
func (k *Keyring) OpenValues(who Identity, sealed store.SealedBytes) (map[string]string, error) {
	text, err := k.Open(who, sealed)
	if err != nil {
		return nil, err
	}
	defer clear(text)
	values, ok := decodeValues(text)
	if !ok {
		return nil, ErrOpen
	}
	return values, nil
}

// decodeValues reads a JSON object of strings as strictly as serde_json read
// a BTreeMap<String, String>: valid UTF-8, an object (not null), string
// values only (not null), nothing after it. A repeated key keeps its last
// value, as serde's map insertion did.
func decodeValues(text []byte) (map[string]string, bool) {
	if !utf8.Valid(text) {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(text))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, false
	}
	out := map[string]string{}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil, false
		}
		value, err := dec.Token()
		if err != nil {
			return nil, false
		}
		k, isKey := key.(string)
		v, isText := value.(string)
		if !isKey || !isText {
			return nil, false
		}
		out[k] = v
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return nil, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return out, true
}

// Rewrap is sealed with its data key sealed again under the current key;
// the values are not touched.
func (k *Keyring) Rewrap(who Identity, sealed store.SealedBytes) (store.SealedBytes, error) {
	version := k.Current()
	dek, err := k.openKey(who, sealed)
	if err != nil {
		return store.SealedBytes{}, err
	}
	defer clear(dek)
	kek, err := k.key(version)
	if err != nil {
		return store.SealedBytes{}, err
	}
	wrapped, err := sealWith(kek, who.keyAAD(version), dek)
	if err != nil {
		return store.SealedBytes{}, err
	}
	return store.SealedBytes{
		Ciphertext: slices.Clone(sealed.Ciphertext),
		WrappedKey: wrapped,
		KeyVersion: version,
	}, nil
}

// parse reads `version:base64-key` lines; blank lines and `#` comments are
// ignored. The reason is the Rust text.
func parse(text string) (*Keyring, string) {
	keys := map[uint32][keyLen]byte{}
	for line := range strings.Lines(text) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		versionText, keyText, ok := strings.Cut(line, ":")
		if !ok {
			return nil, "a line is not `version:key`"
		}
		version, ok := parseVersion(strings.TrimSpace(versionText))
		if !ok {
			return nil, "a version is not a number"
		}
		key, ok := decodeKey(strings.TrimSpace(keyText))
		if !ok {
			return nil, "a key is not base64"
		}
		if len(key) != keyLen {
			clear(key)
			return nil, "a key is not 32 bytes"
		}
		if _, dup := keys[version]; version == 0 || dup {
			clear(key)
			return nil, "versions must be unique and above zero"
		}
		keys[version] = [keyLen]byte(key)
		clear(key)
	}
	if len(keys) == 0 || len(keys) > maxVersions {
		return nil, "a keyring holds 1 to " + strconv.Itoa(maxVersions) + " keys"
	}
	return FromKeys(keys), ""
}

// parseVersion is Rust's u32::from_str: decimal digits, one optional
// leading `+`.
func parseVersion(text string) (uint32, bool) {
	v, err := strconv.ParseUint(strings.TrimPrefix(text, "+"), 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(v), true
}

// decodeKey is the base64 crate's STANDARD decoding: padded, canonical
// trailing bits, no line breaks (Go's decoder skips CR and LF).
func decodeKey(text string) ([]byte, bool) {
	if strings.ContainsAny(text, "\r\n") {
		return nil, false
	}
	key, err := base64.StdEncoding.Strict().DecodeString(text)
	if err != nil {
		return nil, false
	}
	return key, true
}
