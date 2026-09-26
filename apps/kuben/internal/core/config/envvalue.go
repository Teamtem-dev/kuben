package config

import (
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"
)

// envLiteral is a bare `true`, `false` or number from an environment
// variable, with the text it was written as: a text setting takes the text
// (`KUBEN_QUOTA__ORG_CPU=16`), every other setting the value. The fields are
// exported only because koanf copies values by reflection.
type envLiteral struct {
	Text  string
	Value any
}

var errEnvSyntax = errors.New("not a structured value")

// parseEnvValue reads the value of an environment variable the way the Rust
// implementation (figment's Env provider) does: `true` and `false`,
// integers, decimals, `"quoted strings"` with TOML escapes, `'c'`, arrays
// `[a, b]` and tables `{key = value}`, nested at will. Text that is none of
// these is a string: trimmed when it is a bare word, and taken exactly as
// written when it only looks structured (`[broken`, `a,b`, `trueish`).
func parseEnvValue(raw string) any {
	p := envParser{rest: raw}
	v, err := p.value()
	if err != nil || p.rest != "" {
		return raw
	}
	return v
}

type envParser struct{ rest string }

func (p *envParser) skipSpace() {
	p.rest = strings.TrimLeft(p.rest, " \t\n\f\r")
}

func (p *envParser) eat(token string) bool {
	if !strings.HasPrefix(p.rest, token) {
		return false
	}
	p.rest = p.rest[len(token):]
	return true
}

func (p *envParser) value() (any, error) {
	p.skipSpace()
	var (
		v   any
		err error
	)
	switch {
	case p.eat("true"):
		v = envLiteral{Text: "true", Value: true}
	case p.eat("false"):
		v = envLiteral{Text: "false", Value: false}
	case strings.HasPrefix(p.rest, "{"):
		v, err = p.table()
	case strings.HasPrefix(p.rest, "["):
		v, err = p.array()
	case strings.HasPrefix(p.rest, `"`):
		v, err = p.quoted()
	case strings.HasPrefix(p.rest, "'"):
		v, err = p.char()
	default:
		v = p.bare()
	}
	p.skipSpace()
	return v, err
}

// bare is everything up to the next separator: a number, else a string.
func (p *envParser) bare() any {
	end := strings.IndexAny(p.rest, ",{}[]")
	if end < 0 {
		end = len(p.rest)
	}
	word := strings.TrimSpace(p.rest[:end])
	p.rest = p.rest[end:]
	if strings.Contains(word, ".") && strings.Trim(word, "0123456789+-.eE") == "" {
		if f, err := strconv.ParseFloat(word, 64); err == nil {
			return envLiteral{Text: word, Value: f}
		}
	}
	if n, err := strconv.ParseInt(word, 10, 64); err == nil {
		return envLiteral{Text: word, Value: n}
	}
	// Rust's unsigned parser takes one leading `+`; strconv takes none.
	if n, err := strconv.ParseUint(strings.TrimPrefix(word, "+"), 10, 64); err == nil {
		return envLiteral{Text: word, Value: n}
	}
	return word
}

func (p *envParser) char() (any, error) {
	p.eat("'")
	r, size := utf8.DecodeRuneInString(p.rest)
	if size == 0 || (r == utf8.RuneError && size == 1) {
		return nil, errEnvSyntax
	}
	p.rest = p.rest[size:]
	if !p.eat("'") {
		return nil, errEnvSyntax
	}
	return string(r), nil
}

func (p *envParser) quoted() (string, error) {
	p.eat(`"`)
	escaped := false
	end := strings.IndexFunc(p.rest, func(r rune) bool {
		switch {
		case escaped:
			escaped = false
		case r == '\\':
			escaped = true
		case r == '"':
			return true
		}
		return false
	})
	if end < 0 {
		return "", errEnvSyntax
	}
	inner := p.rest[:end]
	p.rest = p.rest[end+1:]
	return unescape(inner)
}

func simpleEscape(c byte) (rune, bool) {
	switch c {
	case '"', '\\':
		return rune(c), true
	case 'b':
		return '\b', true
	case 'f':
		return '\f', true
	case 'n':
		return '\n', true
	case 'r':
		return '\r', true
	case 't':
		return '\t', true
	}
	return 0, false
}

// unescape resolves the escapes of a TOML basic string.
func unescape(s string) (string, error) {
	var out strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		if r != '\\' {
			if r != '\t' && (r < 0x20 || r == 0x7f) {
				return "", errEnvSyntax
			}
			out.WriteRune(r)
			continue
		}
		if i >= len(s) {
			return "", errEnvSyntax
		}
		c := s[i]
		i++
		if r, ok := simpleEscape(c); ok {
			out.WriteRune(r)
			continue
		}
		width := 4
		if c == 'U' {
			width = 8
		}
		if (c != 'u' && c != 'U') || i+width > len(s) {
			return "", errEnvSyntax
		}
		code, err := strconv.ParseUint(s[i:i+width], 16, 32)
		// At most 32 bits: a code over MaxInt32 wraps negative and is invalid.
		if err != nil || !utf8.ValidRune(rune(code)) { //nolint:gosec // see above
			return "", errEnvSyntax
		}
		out.WriteRune(rune(code)) //nolint:gosec // a valid rune, checked above
		i += width
	}
	return out.String(), nil
}

func (p *envParser) array() (any, error) {
	p.eat("[")
	items := []any{}
	for !p.eat("]") {
		item, err := p.value()
		if err != nil {
			return nil, err
		}
		items = append(items, item)
		if !p.eat(",") {
			if !p.eat("]") {
				return nil, errEnvSyntax
			}
			break
		}
	}
	return items, nil
}

func (p *envParser) table() (any, error) {
	p.eat("{")
	table := map[string]any{}
	for !p.eat("}") {
		key, err := p.key()
		if err != nil {
			return nil, err
		}
		if !p.eat("=") {
			return nil, errEnvSyntax
		}
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		table[key] = v
		if !p.eat(",") {
			if !p.eat("}") {
				return nil, errEnvSyntax
			}
			break
		}
	}
	return table, nil
}

// key is a quoted string or a word of letters, digits, `_` and `-`.
func (p *envParser) key() (string, error) {
	p.skipSpace()
	defer p.skipSpace()
	if strings.HasPrefix(p.rest, `"`) {
		return p.quoted()
	}
	end := strings.IndexFunc(p.rest, func(r rune) bool {
		letter := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
		return !letter && (r < '0' || r > '9') && r != '_' && r != '-'
	})
	if end < 0 {
		end = len(p.rest)
	}
	if end == 0 {
		return "", errEnvSyntax
	}
	key := p.rest[:end]
	p.rest = p.rest[end:]
	return key, nil
}
