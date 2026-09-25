package api_test

import (
	"net/netip"
	"testing"

	"github.com/Teamtem-dev/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	api "github.com/Teamtem-dev/kuben/internal/httpapi"
)

func TestOutcomes(t *testing.T) {
	cases := map[int]string{201: "success", 403: "denied", 401: "denied", 429: "throttled", 422: "failure", 503: "error"}
	for status, want := range cases {
		if got := api.Outcome(status); got != want {
			t.Errorf("%d: %s, want %s", status, got, want)
		}
	}
}

func TestParamsFollowTheTemplate(t *testing.T) {
	got := api.PathParams("/api/v1/projects/{project}/environments/{environment}/apps/{app}",
		"/api/v1/projects/shop/environments/prod/apps/api")
	if len(got) != 3 || got[0] != [2]string{"project", "shop"} || got[2] != [2]string{"app", "api"} {
		t.Fatalf("got %v", got)
	}
	if len(api.PathParams("/api/v1/projects", "/api/v1/projects")) != 0 {
		t.Fatal("no params")
	}
}

func TestTimestampsAreRFC3339(t *testing.T) {
	if api.Timestamp(0) != "1970-01-01T00:00:00Z" || api.Timestamp(1_757_894_400_000) != "2025-09-15T00:00:00Z" ||
		api.Timestamp(1_757_894_400_120) != "2025-09-15T00:00:00.12Z" {
		t.Fatal(api.Timestamp(1_757_894_400_120))
	}
}

func TestThePasswordTravelsOnlyOverASecurePath(t *testing.T) {
	ip := func(s string) opt.Val[netip.Addr] { return opt.Some(netip.MustParseAddr(s)) }
	cfg := config.Default()
	cfg.Server.Bind = "0.0.0.0:3000"
	if api.TransportSecure(cfg, "", ip("203.0.113.9"), opt.Some("203.0.113.9")) {
		t.Fatal("plain HTTP from afar")
	}
	if !api.TransportSecure(cfg, "", ip("127.0.0.1"), opt.Some("127.0.0.1")) {
		t.Fatal("an SSH tunnel")
	}
	if api.TransportSecure(cfg, "https", ip("203.0.113.9"), opt.Some("203.0.113.9")) {
		t.Fatal("a header anyone can send is not believed")
	}
	cfg.Security.TrustForwardedFor = true
	cfg.Security.TrustedProxies = []string{"10.42.0.0/16"}
	if !api.TransportSecure(cfg, "https", ip("10.42.0.12"), opt.Some("198.51.100.4")) {
		t.Fatal("HTTPS through the Gateway")
	}
	if api.TransportSecure(cfg, "https", ip("203.0.113.9"), opt.Some("203.0.113.9")) {
		t.Fatal("not through a trusted proxy")
	}
	cfg.Security.InsecureSetup = true
	if !api.TransportSecure(cfg, "", ip("203.0.113.9"), opt.Some("203.0.113.9")) {
		t.Fatal("allowed on purpose")
	}
	cfg.Security.InsecureSetup = false
	cfg.Server.Bind = "127.0.0.1:3000"
	if !api.TransportSecure(cfg, "", opt.None[netip.Addr](), opt.None[string]()) {
		t.Fatal("a loopback-only console")
	}
	if hint := api.InsecureTransportHint(cfg); !contains(hint, "ssh -L 3000:127.0.0.1:3000") {
		t.Fatal(hint)
	}
	if api.SetupURLAt("http://localhost:3000/", opt.None[string]()) != "http://localhost:3000/setup" ||
		api.SetupURLAt("https://k.example", opt.Some("t0k")) != "https://k.example/setup#token=t0k" {
		t.Fatal("setup links")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
