package httpapi_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

// An app's custom domains come from its desired spec, else from the
// `domains[].host` members of its configuration (routes/domains.rs
// app_domain_hosts).
func TestAppDomainHostsReadTheSpecOrTheConfiguration(t *testing.T) {
	for _, c := range []struct {
		name   string
		config opt.Val[any]
		image  opt.Val[string]
		want   []string
	}{
		{name: "no configuration", config: opt.None[any]()},
		{
			name:   "a valid spec",
			config: opt.Some(jsonValue(t, `{"runtime":{"processes":{"web":{"port":8080}}},"domains":[{"host":"shop.example.com"},{"host":"www.example.com"}]}`)),
			image:  opt.Some("nginx:1.27"),
			want:   []string{"shop.example.com", "www.example.com"},
		},
		{
			name:   "a configuration that is no spec",
			config: opt.Some(jsonValue(t, `{"domains":[{"host":"a.example.com"},{"host":7},"b.example.com",{"host":"c.example.com"}]}`)),
			want:   []string{"a.example.com", "c.example.com"},
		},
		{name: "domains that are no list", config: opt.Some(jsonValue(t, `{"domains":{"host":"a.example.com"}}`))},
		{name: "a configuration that is no object", config: opt.Some(jsonValue(t, `[1]`))},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := httpapi.AppDomainHosts(store.AppRecord{Config: c.config, Image: c.image})
			if diff := cmp.Diff(c.want, got); diff != "" {
				t.Fatalf("hosts (-want +got):\n%s", diff)
			}
		})
	}
}

// ClaimDto and DnsChangeDto write every member, absent ones as null
// (serde without skip_serializing_if), and never a revocation. Member
// order is not part of the contract: the documents are compared decoded.
func TestDomainDtosWriteNulls(t *testing.T) {
	id := uuid.MustParse("01890a5d-ac96-774b-bcce-b302099a8057")
	claim := httpapi.ClaimDtoOf(store.DomainClaim{
		ID: id, Domain: "shop.example.com", Token: "kuben-x", Status: "revoked", CreatedBy: "user:a",
		CreatedAt: 0, RevokedAt: opt.Some[int64](5), RevokedBy: opt.Some("user:a"),
	})
	data, err := claim.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"01890a5d-ac96-774b-bcce-b302099a8057","domain":"shop.example.com","status":"revoked",` +
		`"challengeName":"_kuben-challenge.shop.example.com","challengeValue":"kuben-x","method":null,` +
		`"createdBy":"user:a","createdAt":"1970-01-01T00:00:00Z","verifiedAt":null,"lastCheckedAt":null,"lastError":null}`
	if diff := cmp.Diff(jsonValue(t, want), jsonValue(t, string(data))); diff != "" {
		t.Fatalf("claim (-want +got):\n%s", diff)
	}
	change := httpapi.HostSkipped("shop.example.com", "the provider holds no zone for it")
	data, err = change.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	want = `{"host":"shop.example.com","recordType":null,"content":null,"action":"skipped",` +
		`"detail":"the provider holds no zone for it"}`
	if diff := cmp.Diff(jsonValue(t, want), jsonValue(t, string(data))); diff != "" {
		t.Fatalf("change (-want +got):\n%s", diff)
	}
}
