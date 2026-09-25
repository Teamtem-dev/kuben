package jsonx

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Canonical is JSON text as the Rust render::canonical wrote it: object keys
// sorted by their bytes at every level, no whitespace, numbers and strings
// printed exactly as serde_json prints them. Render plans are addressed by
// the SHA-256 of this text and the hashes are stored, so one differing byte
// would make every stored plan look changed (plan §6, contract 3).
//
// The input is JSON text; numbers keep serde_json's reading: an integer
// literal that fits u64 (or i64 when negative) stays an integer, anything
// else is a float64 (`-0` is the float -0.0).
func Canonical(text []byte) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(text))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", fmt.Errorf("canonical JSON: %w", err)
	}
	if _, err := dec.Token(); err == nil {
		return "", errors.New("canonical JSON: trailing data")
	}
	var b strings.Builder
	if err := writeCanonical(&b, v); err != nil {
		return "", err
	}
	return b.String(), nil
}

// CanonicalValue is [Canonical] of v marshalled first (with HTML escaping
// off, which does not matter once the text is re-read).
func CanonicalValue(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", fmt.Errorf("canonical JSON: %w", err)
	}
	return Canonical(buf.Bytes())
}

// SHA256 is `sha256:` and the lowercase hex digest of text, as the Rust
// render::sha256 wrote it.
func SHA256(text string) string {
	sum := sha256.Sum256([]byte(text))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func writeCanonical(b *strings.Builder, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case string:
		writeString(b, x)
	case json.Number:
		return writeNumber(b, string(x))
	case []any:
		b.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeCanonical(b, item); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeString(b, k)
			b.WriteByte(':')
			if err := writeCanonical(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("canonical JSON: unexpected %T", v)
	}
	return nil
}

// writeString escapes as serde_json does: `"` and `\`, the short escapes
// for \b \f \n \r \t, \u00XX (lowercase hex) for the other control
// characters below 0x20, and nothing else (not DEL, not U+2028/U+2029,
// not <, > or &).
func writeString(b *strings.Builder, s string) {
	const hexDigits = "0123456789abcdef"
	b.WriteByte('"')
	for i := 0; i < len(s); {
		c := s[i]
		if c >= utf8.RuneSelf {
			r, size := utf8.DecodeRuneInString(s[i:])
			b.WriteRune(r)
			i += size
			continue
		}
		switch c {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if c < 0x20 {
				b.WriteString(`\u00`)
				b.WriteByte(hexDigits[c>>4])
				b.WriteByte(hexDigits[c&0xf])
			} else {
				b.WriteByte(c)
			}
		}
		i++
	}
	b.WriteByte('"')
}

// writeNumber reads number text the way serde_json does and prints it the
// way serde_json does.
func writeNumber(b *strings.Builder, text string) error {
	n, err := parseSerdeNumber(text)
	if err != nil {
		return fmt.Errorf("canonical JSON: number %q: %w", text, err)
	}
	switch n.kind {
	case numU64:
		b.WriteString(strconv.FormatUint(n.u, 10))
	case numI64:
		b.WriteString(strconv.FormatInt(n.i, 10))
	case numF64:
		b.WriteString(FormatFloat(n.f))
	}
	return nil
}

// FormatFloat prints a finite float64 as serde_json 1.0.151 does: the
// shortest digits that read back to f; plain notation with at least one
// fractional digit (`100.0`, `0.01`) for decimal exponents from -5 to 15,
// scientific otherwise (`1e-6`, `1.5e+16`, `5e-324`).
func FormatFloat(f float64) string {
	if f == 0 {
		if math.Signbit(f) {
			return "-0.0"
		}
		return "0.0"
	}
	sci := strconv.FormatFloat(f, 'e', -1, 64) // -d.ddde±XX
	sign := ""
	if sci[0] == '-' {
		sign, sci = "-", sci[1:]
	}
	mant, expText, _ := strings.Cut(sci, "e")
	exp, _ := strconv.Atoi(expText) //nolint:errcheck // strconv printed it
	digits := strings.Replace(mant, ".", "", 1)
	if exp < minPlainExp || exp > maxPlainExp {
		frac := ""
		if len(digits) > 1 {
			frac = "." + digits[1:]
		}
		expSign := "+"
		if exp < 0 {
			expSign, exp = "-", -exp
		}
		return sign + digits[:1] + frac + "e" + expSign + strconv.Itoa(exp)
	}
	point := exp + 1 // digits before the decimal point
	switch {
	case point <= 0:
		return sign + "0." + strings.Repeat("0", -point) + digits
	case point >= len(digits):
		return sign + digits + strings.Repeat("0", point-len(digits)) + ".0"
	default:
		return sign + digits[:point] + "." + digits[point:]
	}
}

// The decimal exponents printed in plain notation.
const (
	minPlainExp = -5
	maxPlainExp = 15
)
