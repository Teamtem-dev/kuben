package secrets_test

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	secrets "github.com/Teamtem-dev/kuben/internal/keyring"
	"github.com/Teamtem-dev/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/internal/store/pgtest"
)

// Prepare had no Rust test of its own (serve.rs ran it at every start);
// this pins its three outcomes against PostgreSQL.
func TestPrepareResealsUnderTheCurrentKeyAndRefusesAForeignKeyring(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	o, err := s.CreateOrg(ctx, "a", "A")
	if err != nil {
		t.Fatal(err)
	}
	tn, err := s.Tenant(ctx, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tn.Rollback(ctx) }()
	project, err := tn.CreateProject(ctx, "shop", "Shop")
	if err != nil {
		t.Fatal(err)
	}
	env, err := tn.CreateEnvironment(ctx, project, "prod", "Prod", false)
	if err != nil {
		t.Fatal(err)
	}
	old := ring(1)
	if n, err := secrets.Prepare(ctx, s, old, logger); err != nil || n != 0 {
		t.Fatalf("first start: %d %v", n, err)
	}
	reservation, err := tn.ReserveSecretRevision(ctx, project, env, "db", store.SecretOpaque{}, "user:alice")
	if err != nil {
		t.Fatal(err)
	}
	reserved := reservation.(store.ReservationReserved).Reserved
	who := secrets.Identity{Org: o.ID.String(), Secret: reserved.Secret.String(), Revision: reserved.Revision}
	values := map[string]string{"url": "postgres://db"}
	sealed, err := old.SealValues(who, values)
	if err != nil {
		t.Fatal(err)
	}
	if err := tn.InsertSecretRevision(ctx, reserved, []string{"url"}, sealed, "user:alice", store.NewAudit{}); err != nil {
		t.Fatal(err)
	}
	if err := tn.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	foreign := secrets.FromKeys(map[uint32][32]byte{1: filled(9), 2: filled(2)})
	if _, err := secrets.Prepare(ctx, s, foreign, logger); err == nil ||
		!strings.Contains(err.Error(), "under versions [1]: every replica must read the same keyring") {
		t.Fatalf("a foreign keyring: %v", err)
	}
	rotated := ring(1, 2)
	if n, err := secrets.Prepare(ctx, s, rotated, logger); err != nil || n != 1 {
		t.Fatalf("rotation: %d %v", n, err)
	}
	if n, err := secrets.Prepare(ctx, s, rotated, logger); err != nil || n != 0 {
		t.Fatalf("again: %d %v", n, err)
	}
	tn, err = s.Tenant(ctx, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tn.Rollback(ctx) }()
	stale, err := tn.StaleSeals(ctx, 3, 10)
	if err != nil || len(stale) != 1 || stale[0].Sealed.KeyVersion != 2 {
		t.Fatalf("%v %v", stale, err)
	}
	opened, err := ring(2).OpenValues(who, stale[0].Sealed)
	if err != nil || opened["url"] != "postgres://db" {
		t.Fatalf("the retired key is not needed: %v %v", opened, err)
	}
}
