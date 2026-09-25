package notify_test

import (
	"bytes"
	"testing"

	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/notify"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/secrets"
)

// An endpoint secret opens only for the endpoint and organization it was
// sealed for.
func TestEndpointSecretsOpenOnlyForTheirEndpoint(t *testing.T) {
	keyring := secrets.FromKeys(map[uint32][32]byte{1: {7}})
	org, endpoint := ids.New[ids.Org](), uuid.New()
	sealed, err := notify.SealSecret(keyring, org, endpoint, []byte("whsec"))
	if err != nil {
		t.Fatal(err)
	}
	opened, err := notify.OpenSecret(keyring, org, endpoint, sealed)
	if err != nil || !bytes.Equal(opened, []byte("whsec")) {
		t.Fatalf("opened %q: %v", opened, err)
	}
	if _, err := notify.OpenSecret(keyring, org, uuid.New(), sealed); err == nil {
		t.Error("opened for another endpoint")
	}
	if _, err := notify.OpenSecret(keyring, ids.New[ids.Org](), endpoint, sealed); err == nil {
		t.Error("opened for another organization")
	}
}
