// Package oidc verifies OpenID Connect tokens: the GitHub Actions tokens CI
// exchanges (M4.2), and later the ID tokens of SSO. It replaces the Rust
// module crates/kuben-api/src/oidc.rs with the standard library
// (crypto/rsa, encoding/base64, encoding/json) in place of ring and the
// base64 crate.
//
// Only RS256 is accepted (no `none`, no HMAC with a public key as secret),
// the header must name its key, and keys come only from the configured
// issuer's JWKS URL, never from the token. Keys are cached and refreshed at
// most once a minute when a token names an unknown key.
package oidc

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"strings"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/jsonx"
)

// MaxToken is the longest token considered, in bytes.
const MaxToken = 16 << 10

// Why a token was not accepted. The caller answers every one the same way.
// A token whose claims are refused fails with the [ci.TokenError] itself,
// and keys that cannot be read with an [UnavailableError].
var (
	ErrMalformed  = errors.New("the token is malformed")
	ErrAlgorithm  = errors.New("the token is not signed with RS256")
	ErrUnknownKey = errors.New("the token names no known key")
	ErrSignature  = errors.New("the token signature is wrong")
)

// UnavailableError is an issuer whose keys cannot be read: the only
// refusal that is not the token's fault.
type UnavailableError struct{ Reason string }

func (e UnavailableError) Error() string { return "the issuer's keys are unavailable: " + e.Reason }

// JWK is one JSON Web Key; members Kuben does not read are ignored.
type JWK struct {
	Kid opt.Val[string]
	Kty string
	Alg opt.Val[string]
	N   opt.Val[string]
	E   opt.Val[string]
}

// UnmarshalJSON reads a key: `kty` must be there.
func (k *JWK) UnmarshalJSON(data []byte) error {
	var o jsonx.Object
	if err := json.Unmarshal(data, &o); err != nil {
		return err //nolint:wrapcheck // the JSON error is the answer
	}
	var out JWK
	for _, err := range []error{
		jsonx.Optional(o, "kid", &out.Kid),
		jsonx.Required(o, "kty", &out.Kty),
		jsonx.Optional(o, "alg", &out.Alg),
		jsonx.Optional(o, "n", &out.N),
		jsonx.Optional(o, "e", &out.E),
	} {
		if err != nil {
			return err
		}
	}
	*k = out
	return nil
}

// JWKS is a JSON Web Key Set.
type JWKS struct {
	Keys []JWK
}

// UnmarshalJSON reads a key set: `keys` must be there.
func (s *JWKS) UnmarshalJSON(data []byte) error {
	var o jsonx.Object
	if err := json.Unmarshal(data, &o); err != nil {
		return err //nolint:wrapcheck // the JSON error is the answer
	}
	var keys []JWK
	if err := jsonx.Required(o, "keys", &keys); err != nil {
		return err //nolint:wrapcheck // names the member
	}
	s.Keys = keys
	return nil
}

// part decodes one base64url part of a token or key. Go's decoder skips
// line breaks; the Rust one refused them, and so does this.
func part(text string) ([]byte, error) {
	if strings.ContainsAny(text, "\r\n") {
		return nil, ErrMalformed
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(text)
	if err != nil {
		return nil, ErrMalformed
	}
	return b, nil
}

// header is what Kuben reads of a token's header.
type header struct {
	alg  string
	kid  opt.Val[string]
	crit bool
}

func readHeader(text string) (header, error) {
	raw, err := part(text)
	if err != nil {
		return header{}, err
	}
	var o jsonx.Object
	if json.Unmarshal(raw, &o) != nil {
		return header{}, ErrMalformed
	}
	var h header
	var crit opt.Val[json.RawMessage]
	if jsonx.Required(o, "alg", &h.alg) != nil || jsonx.Optional(o, "kid", &h.kid) != nil ||
		jsonx.Optional(o, "crit", &crit) != nil {
		return header{}, ErrMalformed
	}
	h.crit = crit.IsSome()
	return h, nil
}

// findKey is the RSA key of jwks named kid that may sign RS256.
func findKey(jwks JWKS, kid string) (JWK, bool) {
	for _, k := range jwks.Keys {
		if id, ok := k.Kid.Get(); ok && id == kid && k.Kty == "RSA" && k.Alg.Or("RS256") == "RS256" {
			return k, true
		}
	}
	return JWK{}, false
}

// VerifyRS256 is the payload of token if its RS256 signature verifies with
// a key of jwks.
func VerifyRS256(token string, jwks JWKS) ([]byte, error) {
	if len(token) > MaxToken {
		return nil, ErrMalformed
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, ErrMalformed
	}
	head, body, sig := parts[0], parts[1], parts[2]
	h, err := readHeader(head)
	if err != nil {
		return nil, err
	}
	if h.alg != "RS256" || h.crit {
		return nil, ErrAlgorithm
	}
	kid, ok := h.kid.Get()
	if !ok {
		return nil, ErrUnknownKey
	}
	key, ok := findKey(jwks, kid)
	if !ok {
		return nil, ErrUnknownKey
	}
	nText, hasN := key.N.Get()
	eText, hasE := key.E.Get()
	if !hasN || !hasE {
		return nil, ErrUnknownKey
	}
	n, err := part(nText)
	if err != nil {
		return nil, err
	}
	e, err := part(eText)
	if err != nil {
		return nil, err
	}
	signature, err := part(sig)
	if err != nil {
		return nil, err
	}
	public, ok := rsaKey(n, e)
	if !ok {
		return nil, ErrSignature
	}
	digest := sha256.Sum256([]byte(head + "." + body))
	if rsa.VerifyPKCS1v15(public, crypto.SHA256, digest[:], signature) != nil {
		return nil, ErrSignature
	}
	return part(body)
}

// rsaKey is the public key of modulus n and exponent e as ring's
// RSA_PKCS1_2048_8192_SHA256 accepted it: both without leading zeros, a
// modulus of 2048 to 8192 bits, an odd exponent of at least 3. Go refuses
// exponents above 2^31-1 where ring went to 2^33-1; no issuer uses one.
func rsaKey(n, e []byte) (*rsa.PublicKey, bool) {
	if len(n) == 0 || n[0] == 0 || len(e) == 0 || e[0] == 0 || len(e) > 4 {
		return nil, false
	}
	modulus := new(big.Int).SetBytes(n)
	if bits := modulus.BitLen(); bits < 2048 || bits > 8192 {
		return nil, false
	}
	var exponent int64
	for _, b := range e {
		exponent = exponent<<8 | int64(b)
	}
	if exponent < 3 || exponent%2 == 0 || exponent > 1<<31-1 {
		return nil, false
	}
	return &rsa.PublicKey{N: modulus, E: int(exponent)}, true
}
