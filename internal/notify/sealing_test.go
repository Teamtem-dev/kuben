package notify_test

import (
	"bytes"
	"testing"

	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/internal/core/ids"
	secrets "github.com/Teamtem-dev/kuben/internal/keyring"
	"github.com/Teamtem-dev/kuben/internal/notify"
)

// notify.rs endpoint_secrets_are_sealed_for_their_endpoint: an endpoint
// secret opens only for the endpoint and organization it was sealed for.
func TestEndpointSecretsAreSealedForTheirEndpoint(t *testing.T) {
	var key [32]byte
	for i := range key {
		key[i] = 7
	}
	keyring := secrets.FromKeys(map[uint32][32]byte{1: key})
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
