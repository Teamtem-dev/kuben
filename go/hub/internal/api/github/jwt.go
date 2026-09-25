package github

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// GitHub refuses JWTs that live longer than ten minutes; clocks drift.
const (
	jwtBackdate = 60     // seconds before now
	jwtLifetime = 9 * 60 // seconds after now
)

// ring's RSA signing keys: 2048 to 8192 bits, public exponent at least 65537.
const (
	minKeyBits  = 2048
	maxKeyBits  = 8192
	minExponent = 65537
)

// signer signs the App's JWTs (RS256).
type signer func(message []byte) ([]byte, error)

// rsaSigner signs with key (RSASSA-PKCS1-v1_5 over SHA-256).
func rsaSigner(key *rsa.PrivateKey) signer {
	return func(message []byte) ([]byte, error) {
		digest := sha256.Sum256(message)
		sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
		if err != nil {
			return nil, errors.New("signing failed")
		}
		return sig, nil
	}
}

// noKey is the signer of an App without a private key.
func noKey([]byte) ([]byte, error) { return nil, errors.New("this App has no private key") }

// pemDER is the DER inside the PEM block of text, and whether it is PKCS#8.
// It reads the block as the Rust code did (the first `RSA PRIVATE KEY`
// block, else the first `PRIVATE KEY` block; whitespace removed; standard
// base64 with padding), not with encoding/pem, which also accepts headers.
func pemDER(text string) ([]byte, bool, error) {
	var label string
	var pkcs8 bool
	switch {
	case strings.Contains(text, "-----BEGIN RSA PRIVATE KEY-----"):
		label = "RSA PRIVATE KEY"
	case strings.Contains(text, "-----BEGIN PRIVATE KEY-----"):
		label, pkcs8 = "PRIVATE KEY", true
	default:
		return nil, false, errors.New("not a PEM RSA private key")
	}
	_, rest, _ := strings.Cut(text, "-----BEGIN "+label+"-----")
	body, _, ok := strings.Cut(rest, "-----END "+label+"-----")
	if !ok {
		return nil, false, errors.New("an unterminated PEM block")
	}
	encoded := strings.Map(func(r rune) rune {
		if isRustWhitespace(r) {
			return -1
		}
		return r
	}, body)
	der, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return nil, false, fmt.Errorf("%w", err)
	}
	return der, pkcs8, nil
}

// isRustWhitespace is Rust's char::is_whitespace (White_Space=yes).
func isRustWhitespace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\v', '\f', '\r', 0x85, 0xA0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000:
		return true
	}
	return r >= 0x2000 && r <= 0x200A
}

// keyFromPEM is the RSA key of text, PKCS#1 or PKCS#8. Keys ring would not
// sign with are refused with ring's words.
func keyFromPEM(text string) (*rsa.PrivateKey, error) {
	der, pkcs8, err := pemDER(text)
	if err != nil {
		return nil, err
	}
	var key *rsa.PrivateKey
	if pkcs8 {
		parsed, err := x509.ParsePKCS8PrivateKey(der)
		if err != nil {
			return nil, errors.New("InvalidEncoding")
		}
		rsaKey, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("WrongAlgorithm")
		}
		key = rsaKey
	} else {
		key, err = x509.ParsePKCS1PrivateKey(der)
		if err != nil {
			return nil, errors.New("InvalidEncoding")
		}
	}
	switch bits := key.N.BitLen(); {
	case bits < minKeyBits || key.E < minExponent:
		return nil, errors.New("TooSmall")
	case bits > maxKeyBits:
		return nil, errors.New("TooLarge")
	}
	return key, nil
}

// appJWT is the App JWT at nowMs (RFC 7519, RS256): issued a minute ago,
// valid for nine minutes, issued by the App.
func appJWT(sign signer, appID uint64, nowMs int64) (string, error) {
	now := max(nowMs/1000, 0)
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	// serde_json's object: members in key order.
	claims := fmt.Sprintf(`{"exp":%d,"iat":%d,"iss":"%d"}`, now+jwtLifetime, max(now-jwtBackdate, 0), appID)
	input := header + "." + base64.RawURLEncoding.EncodeToString([]byte(claims))
	sig, err := sign([]byte(input))
	if err != nil {
		return "", err
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}
