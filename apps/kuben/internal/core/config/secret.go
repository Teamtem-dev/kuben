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
//
// A struct, not a string type: encoding/json uses a string type's value
// as a map key without asking MarshalText (Go 1.27.2 on), and such a key
// would carry the secret.
type Secret struct{ value string }

// NewSecret is value as a Secret.
func NewSecret(value string) Secret { return Secret{value: value} }

// Expose is the secret itself. Never log it.
func (s Secret) Expose() string { return s.value }

// IsSet reports whether a secret is configured.
func (s Secret) IsSet() bool { return s.value != "" }

// UnmarshalText reads a secret from the configuration.
func (s *Secret) UnmarshalText(text []byte) error {
	s.value = string(text)
	return nil
}

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
