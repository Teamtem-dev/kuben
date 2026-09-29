package keyring_test

import (
	"errors"
	"testing"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/keyring"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

func TestIntegrationSealsBelongToTheirIntegration(t *testing.T) {
	ring := keyring.FromKeys(map[uint32][32]byte{1: filled(1)})
	org := ids.New[ids.Org]()
	registry := ids.New[ids.OrgRegistry]()
	sealed, err := ring.Seal(keyring.OrgRegistryIdentity(org, registry), []byte("s3cret"))
	if err != nil {
		t.Fatal(err)
	}
	login, err := ring.OpenOrgRegistry(store.OrgRegistry{ID: registry, Org: org, Username: "bot", Password: sealed})
	if err != nil || login != (keyring.RegistryLogin{Username: "bot", Password: "s3cret"}) {
		t.Fatalf("open: %v %v", login, err)
	}
	// Not another registry's, organization's, or a connection's.
	if _, err := ring.OpenOrgRegistry(store.OrgRegistry{ID: ids.New[ids.OrgRegistry](), Org: org, Password: sealed}); err == nil {
		t.Fatal("opened as another registry's")
	}
	if _, err := ring.OpenOrgRegistry(store.OrgRegistry{ID: registry, Org: ids.New[ids.Org](), Password: sealed}); err == nil {
		t.Fatal("opened as another organization's")
	}
	connection := ids.From[ids.GitConnection](registry.UUID())
	for _, who := range []keyring.Identity{keyring.GitTokenIdentity(org, connection), keyring.GitWebhookIdentity(org, connection)} {
		if _, err := ring.Open(who, sealed); !errors.Is(err, keyring.ErrOpen) {
			t.Fatalf("opened as %v: %v", who, err)
		}
	}
	seal := store.IntegrationSeal{Kind: store.SealRegistryPassword, ID: registry.UUID(), Sealed: sealed}
	if keyring.IntegrationIdentity(org, seal) != keyring.OrgRegistryIdentity(org, registry) {
		t.Fatal("a stale seal names another identity")
	}
}
