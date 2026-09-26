package config

import (
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/kerrors"
)

// Role is a part of Kuben a process runs (`server.roles`).
type Role string

// The process roles.
const (
	// RoleAll is everything in one process (the default for self-hosting).
	RoleAll        Role = "all"
	RoleAPI        Role = "api"
	RoleController Role = "controller"
	RoleActivator  Role = "activator"
)

// ParseRole reads a process role; the names are lowercase.
func ParseRole(s string) (Role, error) {
	switch r := Role(s); r {
	case RoleAll, RoleAPI, RoleController, RoleActivator:
		return r, nil
	}
	return "", kerrors.New(kerrors.Validation, "unknown variant `%s`, expected one of `all`, `api`, `controller`, `activator`", s)
}

// UnmarshalText makes configuration and JSON decoding refuse unknown roles.
func (r *Role) UnmarshalText(text []byte) error {
	parsed, err := ParseRole(string(text))
	if err != nil {
		return err
	}
	*r = parsed
	return nil
}

// CookieSecure is whether the session cookie carries `Secure` (and the
// `__Host-` prefix): [CookieFixed] (`true` or `false`) or [CookieAuto]
// (`"auto"`, the default).
//
// Browsers drop a `Secure` cookie over plain http, except on `localhost`, so
// a console reached at `http://<server-ip>:3000` could never sign in. Auto
// sets the flag exactly when the console is published over https
// (`server.public_url` starts with `https://`), so a fresh install works
// over http and becomes `Secure` the moment a TLS address is configured. Set
// `true` behind a TLS proxy that does not set `public_url`, and `false` only
// for development.
//
//sumtype:decl
type CookieSecure interface {
	cookieSecure()
}

// CookieFixed is `true` or `false`, as configured.
type CookieFixed bool

// CookieAuto is `"auto"`: `Secure` when `public_url` is https.
type CookieAuto struct{}

func (CookieFixed) cookieSecure() {}
func (CookieAuto) cookieSecure()  {}

// MarshalJSON writes the word `auto`.
func (CookieAuto) MarshalJSON() ([]byte, error) { return []byte(`"auto"`), nil }

// MarshalText writes the word `auto`.
func (CookieAuto) MarshalText() ([]byte, error) { return []byte("auto"), nil }

// DecodeCookieSecure reads `security.cookie_secure` from a decoded TOML, JSON
// or environment value: a boolean, or the string `auto`. The strings `true`
// and `false` are not booleans.
func DecodeCookieSecure(v any) (CookieSecure, error) {
	switch v := v.(type) {
	case CookieSecure:
		return v, nil
	case bool:
		return CookieFixed(v), nil
	case string:
		if v == "auto" {
			return CookieAuto{}, nil
		}
	}
	return nil, kerrors.New(kerrors.Validation, "security.cookie_secure must be true, false or \"auto\"")
}
