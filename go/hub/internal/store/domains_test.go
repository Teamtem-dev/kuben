package store_test

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store/pgtest"
)

// Ported from repo/domains.rs.

const claimToken = "0123456789abcdef0123456789abcdef"

func newUUID() uuid.UUID { return uuid.Must(uuid.NewV7()) }

func verify(t *testing.T, tn *store.Tenant, claim uuid.UUID) store.Verified {
	t.Helper()
	return must[store.Verified](t, "verify")(tn.VerifyClaim(t.Context(), claim, "txt", opt.None[uuid.UUID]()))
}

type owner struct {
	Domain string
	Org    ids.OrgID
	Found  bool
}

func domainOwner(t *testing.T, s *store.Store, host string) owner {
	t.Helper()
	domain, org, found, err := s.DomainOwner(t.Context(), host)
	if err != nil {
		t.Fatalf("owner of %s: %v", host, err)
	}
	return owner{domain, org, found}
}

func TestVerifiedDomainsAreUniqueAcrossOrganizations(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	a := org(t, s, "a", "A")
	b := org(t, s, "b", "B")
	claimA, claimB, claimB2 := newUUID(), newUUID(), newUUID()

	tn := tenant(t, s, a)
	if err := tn.CreateClaim(ctx, claimA, "example.com", claimToken, "u"); err != nil {
		t.Fatal(err)
	}
	if got := verify(t, tn, claimA); got != (store.ClaimVerified{}) {
		t.Fatalf("first verification: %#v", got)
	}
	if got := verify(t, tn, claimA); got != (store.ClaimNotPending{}) {
		t.Fatalf("second verification: %#v", got)
	}
	commit(t, tn)

	tn = tenant(t, s, a)
	if err := tn.CreateClaim(ctx, newUUID(), "example.com", claimToken, "u"); !store.IsUniqueViolation(err) {
		t.Fatalf("one open claim per domain: %v", err)
	}

	tn = tenant(t, s, b)
	if err := tn.CreateClaim(ctx, claimB, "shop.example.com", claimToken, "u"); err != nil {
		t.Fatal(err)
	}
	if err := tn.CreateClaim(ctx, claimB2, "other.org", claimToken, "u"); err != nil {
		t.Fatal(err)
	}
	if got := verify(t, tn, claimB); got != (store.ClaimTaken{Domain: "example.com"}) {
		t.Fatalf("a subdomain of another organization's domain: %#v", got)
	}
	if got := verify(t, tn, claimB2); got != (store.ClaimVerified{}) {
		t.Fatalf("an unrelated domain: %#v", got)
	}
	if claims := must[[]store.DomainClaim](t, "list")(tn.Claims(ctx, false)); len(claims) != 2 {
		t.Fatalf("isolated: %d claims", len(claims))
	}
	commit(t, tn)

	if got := domainOwner(t, s, "a.shop.example.com"); got != (owner{"example.com", a, true}) {
		t.Fatalf("owner: %+v", got)
	}
	if got := domainOwner(t, s, "example.net"); got.Found {
		t.Fatalf("nobody's: %+v", got)
	}

	tn = tenant(t, s, a)
	if !must[bool](t, "revoke")(tn.RevokeClaim(ctx, claimA, "u")) {
		t.Fatal("not revoked")
	}
	if must[bool](t, "revoke")(tn.RevokeClaim(ctx, claimA, "u")) {
		t.Fatal("revoked twice")
	}
	commit(t, tn)
	if got := domainOwner(t, s, "example.com"); got.Found {
		t.Fatalf("revoked: %+v", got)
	}
	tn = tenant(t, s, b)
	if got := verify(t, tn, claimB); got != (store.ClaimVerified{}) {
		t.Fatalf("free again: %#v", got)
	}
}

func TestProvidersAndRecordsAreKept(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	o := org(t, s, "a", "A")
	id := newUUID()
	sealed := store.SealedBytes{Ciphertext: []byte{1}, WrappedKey: []byte{2}, KeyVersion: 1}
	tn := tenant(t, s, o)
	if err := tn.CreateDNSProvider(ctx, id, "cf", "cloudflare", sealed, "u"); err != nil {
		t.Fatal(err)
	}
	if providers := must[[]store.DNSProvider](t, "list")(tn.DNSProviders(ctx)); len(providers) != 1 || providers[0].Name != "cf" {
		t.Fatalf("providers: %+v", providers)
	}
	kind, got, found, err := tn.DNSProviderSecret(ctx, id)
	if err != nil || !found || kind != "cloudflare" {
		t.Fatalf("read: %q %v %v", kind, found, err)
	}
	if diff := cmp.Diff(sealed, got); diff != "" {
		t.Fatalf("sealed token (-want +got):\n%s", diff)
	}
	record := store.NewDNSRecord{
		Provider: id, Name: "shop.example.com", RecordType: "A", Content: "203.0.113.10",
		ZoneID: "z1", ProviderRef: "r1",
	}
	first := must[uuid.UUID](t, "record")(tn.RecordDNS(ctx, record))
	record.ProviderRef = "r2"
	if again := must[uuid.UUID](t, "record")(tn.RecordDNS(ctx, record)); again != first {
		t.Fatalf("one row per record: %s %s", first, again)
	}
	if err := tn.ForgetDNSRecord(ctx, first); err != nil {
		t.Fatal(err)
	}
	if !must[bool](t, "delete")(tn.DeleteDNSProvider(ctx, id)) {
		t.Fatal("not deleted")
	}
	if providers := must[[]store.DNSProvider](t, "list")(tn.DNSProviders(ctx)); len(providers) != 0 {
		t.Fatalf("deleted: %+v", providers)
	}
	commit(t, tn)
}

// A failed check keeps at most 1024 characters of its reason (the column's
// CHECK), counted in characters as Rust's chars().take(1024).
func TestAClaimKeepsTheStartOfALongFailure(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	o := org(t, s, "a", "A")
	id := newUUID()
	tn := tenant(t, s, o)
	if err := tn.CreateClaim(ctx, id, "example.com", claimToken, "u"); err != nil {
		t.Fatal(err)
	}
	if err := tn.ClaimChecked(ctx, id, opt.Some(strings.Repeat("é", 2000))); err != nil {
		t.Fatal(err)
	}
	claim, found, err := tn.Claim(ctx, id)
	if err != nil || !found {
		t.Fatalf("claim: %v %v", found, err)
	}
	if kept := claim.LastError.Or(""); kept != strings.Repeat("é", 1024) || !claim.LastCheckedAt.IsSome() {
		t.Fatalf("kept %d characters, checked %v", len([]rune(kept)), claim.LastCheckedAt)
	}
}
