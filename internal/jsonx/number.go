package wire

import (
	"errors"
	"math"
	"strconv"
)

// serde_json (1.0.151, without the float_roundtrip feature, as Kuben builds
// it) reads a JSON number with its own fast algorithm, which is not always
// correctly rounded: "1.00000000000000011102230246251565404236316680908203125"
// is 1.0000000000000002 there and 1.0 for strconv. Content hashes depend on
// the result, so this is a port of serde_json's parser (de.rs: parse_integer,
// parse_number, parse_decimal, parse_exponent, f64_from_parts), not a call
// to strconv.ParseFloat.

type numKind uint8

const (
	numU64 numKind = iota + 1
	numI64
	numF64
)

type serdeNumber struct {
	kind numKind
	u    uint64
	i    int64
	f    float64
}

var errNumberOutOfRange = errors.New("number out of range")

// pow10 holds 1e0 … 1e308 as serde_json's POW10 table does: the correctly
// rounded values of the literals. (math.Pow10 multiplies two table entries
// for large exponents and can differ in the last bit.)
var pow10 = func() [309]float64 {
	var t [309]float64
	for i := range t {
		t[i], _ = strconv.ParseFloat("1e"+strconv.Itoa(i), 64) //nolint:errcheck // 1e0…1e308 are all in range
	}
	return t
}()

// overflows10 reports whether a*10+digit exceeds limit.
func overflows10(a, digit, limit uint64) bool {
	return a >= limit/10 && (a > limit/10 || digit > limit%10)
}

type numScanner struct {
	text string
	pos  int
}

func (s *numScanner) peek() byte {
	if s.pos < len(s.text) {
		return s.text[s.pos]
	}
	return 0
}

func isDigitByte(c byte) bool { return c >= '0' && c <= '9' }

// parseSerdeNumber reads text, which must be a valid JSON number.
func parseSerdeNumber(text string) (serdeNumber, error) {
	s := &numScanner{text: text}
	positive := true
	if s.peek() == '-' {
		positive = false
		s.pos++
	}
	if !isDigitByte(s.peek()) {
		return serdeNumber{}, errors.New("invalid number")
	}
	significand := uint64(s.peek() - '0')
	s.pos++
	if significand != 0 {
		for isDigitByte(s.peek()) {
			digit := uint64(s.peek() - '0')
			if overflows10(significand, digit, math.MaxUint64) {
				f, err := s.longInteger(positive, significand)
				return serdeNumber{kind: numF64, f: f}, err
			}
			s.pos++
			significand = significand*10 + digit
		}
	}
	switch s.peek() {
	case '.':
		f, err := s.decimal(positive, significand, 0)
		return serdeNumber{kind: numF64, f: f}, err
	case 'e', 'E':
		f, err := s.exponent(positive, significand, 0)
		return serdeNumber{kind: numF64, f: f}, err
	}
	if positive {
		return serdeNumber{kind: numU64, u: significand}, nil
	}
	neg := -int64(significand) //nolint:gosec // serde's wrapping_neg of `significand as i64`
	if neg >= 0 {              // underflow, or -0
		return serdeNumber{kind: numF64, f: -float64(significand)}, nil
	}
	return serdeNumber{kind: numI64, i: neg}, nil
}

func (s *numScanner) longInteger(positive bool, significand uint64) (float64, error) {
	exponent := int32(0)
	for {
		switch c := s.peek(); {
		case isDigitByte(c):
			s.pos++
			exponent++
		case c == '.':
			return s.decimal(positive, significand, exponent)
		case c == 'e' || c == 'E':
			return s.exponent(positive, significand, exponent)
		default:
			return fromParts(positive, significand, exponent)
		}
	}
}

func (s *numScanner) decimal(positive bool, significand uint64, before int32) (float64, error) {
	s.pos++ // '.'
	after := int32(0)
	for isDigitByte(s.peek()) {
		digit := uint64(s.peek() - '0')
		if overflows10(significand, digit, math.MaxUint64) {
			for isDigitByte(s.peek()) { // ignore every further digit
				s.pos++
			}
			break
		}
		s.pos++
		significand = significand*10 + digit
		after--
	}
	if c := s.peek(); c == 'e' || c == 'E' {
		return s.exponent(positive, significand, before+after)
	}
	return fromParts(positive, significand, before+after)
}

func (s *numScanner) exponent(positive bool, significand uint64, starting int32) (float64, error) {
	s.pos++ // 'e'
	positiveExp := true
	switch s.peek() {
	case '+':
		s.pos++
	case '-':
		positiveExp = false
		s.pos++
	}
	exp := int32(0)
	for isDigitByte(s.peek()) {
		digit := int32(s.peek() - '0')
		if exp >= math.MaxInt32/10 && (exp > math.MaxInt32/10 || digit > math.MaxInt32%10) {
			if significand != 0 && positiveExp {
				return 0, errNumberOutOfRange
			}
			if positive {
				return 0, nil
			}
			return math.Copysign(0, -1), nil
		}
		s.pos++
		exp = exp*10 + digit
	}
	final := saturatingAdd32(starting, exp)
	if !positiveExp {
		final = saturatingAdd32(starting, -exp)
	}
	return fromParts(positive, significand, final)
}

func saturatingAdd32(a, b int32) int32 {
	sum := int64(a) + int64(b)
	switch {
	case sum > math.MaxInt32:
		return math.MaxInt32
	case sum < math.MinInt32:
		return math.MinInt32
	default:
		return int32(sum)
	}
}

// fromParts is serde_json's f64_from_parts.
func fromParts(positive bool, significand uint64, exponent int32) (float64, error) {
	f := float64(significand)
	for {
		abs := int64(exponent)
		if abs < 0 {
			abs = -abs
		}
		if abs < int64(len(pow10)) {
			if exponent >= 0 {
				f *= pow10[abs]
				if math.IsInf(f, 0) {
					return 0, errNumberOutOfRange
				}
			} else {
				f /= pow10[abs]
			}
			break
		}
		if f == 0 {
			break
		}
		if exponent >= 0 {
			return 0, errNumberOutOfRange
		}
		f /= 1e308
		exponent += 308
	}
	if !positive {
		f = -f
	}
	return f, nil
}
