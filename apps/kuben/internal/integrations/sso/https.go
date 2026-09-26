package sso

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/integrations/outbound"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/version"
)

// HTTPS is the [IdentityProvider] over the outgoing transport: https only,
// proxies from the environment (Rust HttpsGet).
type HTTPS struct{ Client *outbound.Client }

// Get is the body of url if it answers 200, at most limit bytes.
func (h HTTPS) Get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err //nolint:wrapcheck // the text becomes an UnavailableError
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "kuben/"+version.Version)
	status, body, err := h.Client.Fetch(ctx, req, limit)
	if err != nil {
		return nil, err //nolint:wrapcheck // the text becomes an UnavailableError
	}
	if status != http.StatusOK {
		return nil, errors.New(httpStatus(status))
	}
	return body, nil
}

// PostForm posts form to url with HTTP Basic client authentication.
func (h HTTPS) PostForm(ctx context.Context, url, form, basic string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(form))
	if err != nil {
		return 0, nil, err //nolint:wrapcheck // the text becomes an UnavailableError
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Basic "+basic)
	req.Header.Set("User-Agent", "kuben/"+version.Version)
	return h.Client.Fetch(ctx, req, MaxDocument) //nolint:wrapcheck // the text becomes an UnavailableError
}
