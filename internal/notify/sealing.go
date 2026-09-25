// Package notify delivers Kuben's events to webhooks and keeps incidents
// (crates/kuben-api/src/notify.rs). Endpoint secrets — a webhook's signing
// secret, a DNS provider's token — are sealed with the secret keyring under
// the endpoint's id, at revision 0.
package notify

import (
	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/keyring"
	"github.com/Teamtem-dev/kuben/internal/store"
)

func endpointIdentity(org ids.OrgID, endpoint uuid.UUID) keyring.Identity {
	return keyring.Identity{Org: org.String(), Secret: endpoint.String(), Revision: 0}
}

// SealSecret seals a new endpoint secret for endpoint of org (seal_secret).
func SealSecret(keyring *keyring.Keyring, org ids.OrgID, endpoint uuid.UUID, secret []byte) (store.SealedBytes, error) {
	sealed, err := keyring.Seal(endpointIdentity(org, endpoint), secret)
	if err != nil {
		return store.SealedBytes{}, err //nolint:wrapcheck // the keyring's text, as Rust's to_string
	}
	return sealed, nil
}

// OpenSecret opens the secret of endpoint of org (open_secret).
func OpenSecret(keyring *keyring.Keyring, org ids.OrgID, endpoint uuid.UUID, sealed store.SealedBytes) ([]byte, error) {
	secret, err := keyring.Open(endpointIdentity(org, endpoint), sealed)
	if err != nil {
		return nil, err //nolint:wrapcheck // the keyring's text, as Rust's to_string
	}
	return secret, nil
}
