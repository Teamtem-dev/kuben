package oci

// Logging in to a registry without pulling anything (2.1): what an
// organization registry is checked with before it is saved, and again on
// request.

import (
	"context"
	"io"
	"net/http"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/version"
)

// LoginChecker logs in to registries. [Registry] asks them; [Fixed]
// accepts every login.
type LoginChecker interface {
	// CheckLogin logs in to registry (a registry name as image references
	// carry it: `ghcr.io`, `docker.io`, `registry.example.com:5000`) as
	// login; nil when the registry accepts it.
	CheckLogin(ctx context.Context, registry string, login Login) error
}

// CheckLogin pings the registry's /v2/ over HTTPS, answers its challenge
// with login (a Bearer challenge is exchanged for a token without a
// repository scope, as `docker login` does) and asks /v2/ again with the
// credentials; within the same 10 s as a resolution. Failures are
// [ResolveError]s naming the registry: Unauthorized when the login is
// refused, Unreachable when the registry cannot be asked, RateLimited.
func (r Registry) CheckLogin(ctx context.Context, registry string, login Login) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	reg, err := name.NewRegistry(registry)
	if err != nil {
		return Invalid{Image: registry}
	}
	inner := r.transport
	if inner == nil {
		inner = remote.DefaultTransport
	}
	obs := &observer{inner: inner}
	s := &session{image: registry, obs: obs}
	auth := authn.FromConfig(authn.AuthConfig{Auth: login.encoded()})
	authed, err := transport.NewWithContext(ctx, reg, auth, obs, nil)
	if err != nil {
		return s.fail(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reg.Scheme()+"://"+reg.RegistryStr()+"/v2/", nil)
	if err != nil {
		return Unreachable{Image: registry, Reason: err.Error()}
	}
	req.Header.Set("User-Agent", "kuben/"+version.Version)
	resp, err := authed.RoundTrip(req)
	if err != nil {
		return s.fail(err)
	}
	defer resp.Body.Close()               //nolint:errcheck // read to the end below; nothing to report
	_, _ = io.Copy(io.Discard, resp.Body) //nolint:errcheck // the status is the answer
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	return s.statusError(&transport.Error{StatusCode: resp.StatusCode, Request: req})
}

// CheckLogin accepts every login: fixed answers know no registry.
func (Fixed) CheckLogin(context.Context, string, Login) error { return nil }
