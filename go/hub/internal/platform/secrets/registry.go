package secrets

import (
	"encoding/base64"
	"fmt"
	"io"

	"github.com/Teamtem-dev/kuben/go/hub/internal/wire"
)

// DockerHub is Docker Hub's registry name in image references.
const DockerHub = "docker.io"

// The keys of a `registry` secret's values.
const (
	loginUsername = "username"
	loginPassword = "password"
)

// RegistryLogin is the credentials for pulling from a private registry: the
// values of a `registry` secret. Formatting it never shows the password.
// The API's image resolution takes it as an oci.Login (same fields).
type RegistryLogin struct {
	Username string
	Password string
}

// Format names the user and leaves the password out, for every verb.
func (l RegistryLogin) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, fmt.Sprintf("RegistryLogin { username: %q, .. }", l.Username)) //nolint:errcheck // fmt.Formatter cannot report a write error
}

// RegistryLoginFrom is the login in the values of a `registry` secret.
func RegistryLoginFrom(values map[string]string) (RegistryLogin, bool) {
	username, ok := values[loginUsername]
	if !ok {
		return RegistryLogin{}, false
	}
	password, ok := values[loginPassword]
	if !ok {
		return RegistryLogin{}, false
	}
	return RegistryLogin{Username: username, Password: password}, true
}

// Values is what a `registry` secret stores.
func (l RegistryLogin) Values() map[string]string {
	return map[string]string{loginUsername: l.Username, loginPassword: l.Password}
}

func (l RegistryLogin) encoded() string {
	return base64.StdEncoding.EncodeToString([]byte(l.Username + ":" + l.Password))
}

// Basic is an `Authorization: Basic …` value.
func (l RegistryLogin) Basic() string { return "Basic " + l.encoded() }

// DockerConfig is the `.dockerconfigjson` of a pull secret for registry (a
// registry name as image references carry it), byte for byte what
// serde_json wrote: keys sorted, no whitespace.
func (l RegistryLogin) DockerConfig(registry string) string {
	// The kubelet knows Docker Hub by its legacy index URL.
	key := registry
	if registry == DockerHub {
		key = "https://index.docker.io/v1/"
	}
	config := map[string]any{"auths": map[string]any{key: map[string]any{
		"username": l.Username,
		"password": l.Password,
		"auth":     l.encoded(),
	}}}
	text, err := wire.CanonicalValue(config)
	if err != nil {
		// Strings always encode; unreachable.
		return ""
	}
	return text
}
