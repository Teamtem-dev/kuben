// Package artifact has OCI artifact references (ADR-026): a release names
// its images by digest, never by tag. It replaces the Rust module
// kuben-core/src/artifact.rs.
package artifact

import (
	"fmt"
	"strings"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/kerrors"
)

// Digest is an OCI content digest: `sha256:` with 64 or `sha512:` with 128
// lowercase hex digits. It is comparable and a valid map key. Only
// [ParseDigest] and decoding make a non-zero Digest, so a non-zero Digest is
// well formed; the zero Digest names nothing.
type Digest struct{ s string }

// InvalidDigestError says that a string is not a digest. It is a
// kerrors.Validation error to errors.Is, errors.As and kerrors.CodeOf.
type InvalidDigestError struct {
	// Value is the refused string.
	Value string
}

func (e *InvalidDigestError) Error() string {
	return fmt.Sprintf("not an OCI sha256 or sha512 digest: %q", e.Value)
}

// Unwrap is the validation error the API reports.
func (e *InvalidDigestError) Unwrap() error {
	return kerrors.New(kerrors.Validation, "%s", e.Error())
}

// ParseDigest reads a digest, or fails with an [*InvalidDigestError].
func ParseDigest(s string) (Digest, error) {
	algorithm, hex, found := strings.Cut(s, ":")
	var want int
	switch {
	case found && algorithm == "sha256":
		want = 64
	case found && algorithm == "sha512":
		want = 128
	default:
		return Digest{}, &InvalidDigestError{Value: s}
	}
	if len(hex) != want || !isLowerHex(hex) {
		return Digest{}, &InvalidDigestError{Value: s}
	}
	return Digest{s: s}, nil
}

func isLowerHex(s string) bool {
	for i := range len(s) {
		if b := s[i]; (b < '0' || b > '9') && (b < 'a' || b > 'f') {
			return false
		}
	}
	return true
}

// String is the digest as written, `algorithm:hex`.
func (d Digest) String() string { return d.s }

// IsZero reports whether the digest is the zero Digest, which names nothing.
func (d Digest) IsZero() bool { return d.s == "" }

// Compare orders digests by their text.
func (d Digest) Compare(other Digest) int { return strings.Compare(d.s, other.s) }

// MarshalText makes the digest a JSON string.
func (d Digest) MarshalText() ([]byte, error) { return []byte(d.s), nil }

// UnmarshalText validates what it reads, as [ParseDigest] does.
func (d *Digest) UnmarshalText(text []byte) error {
	parsed, err := ParseDigest(string(text))
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}
