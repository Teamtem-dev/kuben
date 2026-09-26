package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"strings"
)

// SignatureHeader is the header GitHub signs deliveries in.
const SignatureHeader = "x-hub-signature-256"

// VerifySignature reports whether signature (`sha256=<64 hex digits>`) is
// the HMAC-SHA256 of body under secret, compared in constant time.
func VerifySignature(secret, body []byte, signature string) bool {
	hex, ok := strings.CutPrefix(signature, "sha256=")
	if !ok {
		return false
	}
	tag, ok := decodeHex(hex)
	if !ok {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body) //nolint:errcheck // a hash never fails to write
	return hmac.Equal(mac.Sum(nil), tag)
}

// decodeHex reads 64 hexadecimal digits, two per byte, as Rust's
// `u8::from_str_radix(pair, 16)` read each pair: either case, and a pair may
// also be `+` and one digit.
func decodeHex(hex string) ([]byte, bool) {
	if len(hex) != 64 {
		return nil, false
	}
	out := make([]byte, 0, 32)
	for i := 0; i < len(hex); i += 2 {
		hi, lo := hex[i], hex[i+1]
		if hi == '+' {
			v, ok := hexDigit(lo)
			if !ok {
				return nil, false
			}
			out = append(out, v)
			continue
		}
		h, ok1 := hexDigit(hi)
		l, ok2 := hexDigit(lo)
		if !ok1 || !ok2 {
			return nil, false
		}
		out = append(out, h<<4|l)
	}
	return out, true
}

func hexDigit(b byte) (byte, bool) {
	switch {
	case b >= '0' && b <= '9':
		return b - '0', true
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10, true
	case b >= 'A' && b <= 'F':
		return b - 'A' + 10, true
	}
	return 0, false
}
