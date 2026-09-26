package config

import (
	"fmt"
	"io"
)

// redacted is what a [Secret] shows instead of its value.
const redacted = "[redacted]"

// Secret is a configured secret (a password, a signing secret, a URL with
// credentials). Formatting, logging and encoding it show `[redacted]`; the
// value is only reachable through [Secret.Expose], so every use is visible
// in review. The empty Secret means "not set".
//
// Keep it a plain field: inside a wrapper with unexported fields (such as
// opt.Val) the fmt package cannot call these methods and would print the
// value.
type Secret string

// Expose is the secret itself. Never log it.
func (s Secret) Expose() string { return string(s) }

// IsSet reports whether a secret is configured.
func (s Secret) IsSet() bool { return s != "" }

// String hides the value from %v, %s, %q and friends.
func (Secret) String() string { return redacted }

// GoString hides the value from %#v.
func (Secret) GoString() string { return redacted }

// Format hides the value from every verb, the ones that are wrong for text
// (%d) included: fmt would print the value next to its complaint.
func (Secret) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, redacted) //nolint:errcheck // fmt.Formatter cannot report a write error
}

// MarshalText hides the value from text encoders (and from JSON map keys).
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// MarshalJSON hides the value from JSON.
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }
