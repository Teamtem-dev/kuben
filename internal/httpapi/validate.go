package api

// Input validation shared by the REST handlers (routes/validate.rs).
// Errors are user-facing (`422 validation_failed`), so messages say what is
// allowed. Lengths are in bytes, as Rust's `str::len`.

import (
	"strings"
	"unicode"

	"github.com/Teamtem-dev/kuben/internal/core/ascii"
	kerr "github.com/Teamtem-dev/kuben/internal/core/kerrors"
)

// isLabel reports an RFC 1123 label of at most max bytes: `a-z`, `0-9`,
// `-`, not starting or ending with `-`.
func isLabel(value string, max int) bool {
	if value == "" || len(value) > max || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	for i := range len(value) {
		b := value[i]
		if (b < 'a' || b > 'z') && (b < '0' || b > '9') && b != '-' {
			return false
		}
	}
	return true
}

// DNSLabel checks value as an RFC 1123 label of at most max bytes.
func DNSLabel(field, value string, max int) error {
	if !isLabel(value, max) {
		return kerr.New(kerr.Validation,
			"%s must use a-z, 0-9 and '-', must not start or end with '-', and be at most %d characters", field, max)
	}
	return nil
}

// Hostname checks a fully qualified hostname, e.g. `api.example.com`.
func Hostname(host string) error {
	labels := strings.Split(host, ".")
	ok := len(host) <= 253 && len(labels) >= 2
	for _, l := range labels {
		ok = ok && isLabel(ascii.Lower(l), 63)
	}
	if !ok {
		return kerr.New(kerr.Validation, "`%s` is not a valid hostname", host)
	}
	return nil
}

func isASCIIAlpha(r rune) bool { return r < unicode.MaxASCII && unicode.IsLetter(r) }

func isASCIIAlnum(r rune) bool {
	return r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r))
}

// EnvVarName checks a POSIX-style environment variable name.
func EnvVarName(name string) error {
	ok := len(name) <= 256 && name != ""
	for i, r := range name {
		if i == 0 {
			ok = ok && (isASCIIAlpha(r) || r == '_')
		} else {
			ok = ok && (isASCIIAlnum(r) || r == '_')
		}
	}
	if !ok {
		return kerr.New(kerr.Validation, "`%s` is not a valid environment variable name", name)
	}
	return nil
}

// Image checks the syntax of a container image reference; the registry is
// not contacted.
func Image(image string) error {
	image = strings.TrimFunc(image, unicode.IsSpace)
	ok := image != "" && len(image) <= 512 && !strings.HasPrefix(image, "-") &&
		!strings.ContainsFunc(image, func(r rune) bool { return r <= ' ' || r >= 0x7f })
	if !ok {
		return kerr.New(kerr.Validation, "image must be a container image reference, e.g. nginx:1.27")
	}
	return nil
}

// SecretKey checks a key inside a Secret (`[-._a-zA-Z0-9]+`).
func SecretKey(key string) error {
	ok := key != "" && len(key) <= 253 &&
		!strings.ContainsFunc(key, func(r rune) bool { return !isASCIIAlnum(r) && r != '-' && r != '_' && r != '.' })
	if !ok {
		return kerr.New(kerr.Validation, "`%s` is not a valid secret key", key)
	}
	return nil
}

// Quantity checks the syntax of a Kubernetes quantity such as `500m`, `2`
// or `1Gi`.
func Quantity(field, value string) error {
	ok := value != "" && len(value) <= 32 && value[0] >= '0' && value[0] <= '9' &&
		!strings.ContainsFunc(value, func(r rune) bool { return !isASCIIAlnum(r) && r != '.' })
	if !ok {
		return kerr.New(kerr.Validation, "%s must be a quantity such as 500m, 2 or 4Gi", field)
	}
	return nil
}

// ValidEmail checks a plausible email address: at most 254 bytes, no
// whitespace, a non-empty local part and a domain with a dot and no second
// `@`. No delivery is attempted.
func ValidEmail(value string) error {
	local, domain, found := strings.Cut(value, "@")
	ok := len(value) <= 254 && !strings.ContainsFunc(value, unicode.IsSpace) &&
		found && local != "" && strings.Contains(domain, ".") && !strings.Contains(domain, "@")
	if !ok {
		return kerr.New(kerr.Validation, "`%s` is not a valid email address", value)
	}
	return nil
}

// TimeZone checks the syntax of an IANA time zone name such as
// `Europe/Berlin`.
func TimeZone(value string) error {
	ok := value != "" && len(value) <= 64 &&
		!strings.ContainsFunc(value, func(r rune) bool { return !isASCIIAlnum(r) && !strings.ContainsRune("/_-+", r) })
	if !ok {
		return kerr.New(kerr.Validation, "`%s` is not a valid time zone", value)
	}
	return nil
}
