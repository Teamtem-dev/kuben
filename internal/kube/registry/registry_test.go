package registry_test

import (
	"io"
	"log/slog"
	"testing"

	"github.com/Teamtem-dev/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/kube/registry"
)

func TestCredentialsAreRedactedFromURLs(t *testing.T) {
	cases := map[string]string{
		"configured proxy http://user:s3cret@proxy.local:3128/ requires a feature": "configured proxy http://***@proxy.local:3128/ requires a feature",
		"a postgres://kuben:pw@db:5432/kuben b https://token@api.example.com":      "a postgres://***@db:5432/kuben b https://***@api.example.com",
	}
	for in, want := range cases {
		if got := registry.RedactCredentials(in); got != want {
			t.Errorf("got %q", got)
		}
	}
}

func TestTextWithoutCredentialsIsUntouched(t *testing.T) {
	for _, s := range []string{"https://10.0.0.1:6443/api", "no url here", "https://h/path/a@b", "trailing ://", ""} {
		if got := registry.RedactCredentials(s); got != s {
			t.Errorf("%q became %q", s, got)
		}
	}
}

func TestConfiguredNamespaceWins(t *testing.T) {
	if got := registry.OwnNamespace(opt.Some(" kuben-system ")); got != opt.Some("kuben-system") {
		t.Fatalf("got %v", got)
	}
}

func TestUnusableConfigDegradesUnlessRequired(t *testing.T) {
	cfg := config.KubeCfg{Kubeconfig: opt.Some("/nonexistent/kubeconfig")}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	r, err := registry.FromConfig(cfg, quiet)
	if err != nil || r.IsSome() {
		t.Fatalf("degrades: %v %v", r, err)
	}
	cfg.Required = true
	if _, err := registry.FromConfig(cfg, quiet); err == nil {
		t.Fatal("required")
	}
}
