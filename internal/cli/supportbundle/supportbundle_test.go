package supportbundle_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/internal/cli/supportbundle"
	"github.com/Teamtem-dev/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/jsonx"
	"github.com/Teamtem-dev/kuben/internal/version"
)

func TestOnlyAllowlistedConfigurationIsKept(t *testing.T) {
	cfg := config.Default()
	cfg.Database.URL = "postgres://kuben:hunter22@db.example.com/kuben"
	cfg.Bootstrap.AdminPassword = "admin-secret"
	cfg.Bootstrap.AdminEmail = "ops@example.com"
	cfg.SSO.ClientSecret = "sso-secret"
	cfg.Git.GithubWebhookSecret = "hook-secret"
	cfg.Telemetry.OTLPEndpoint = opt.Some("https://u:otlp-secret@otel.example.com")
	cfg.Server.PublicURL = opt.Some("https://user:pw-secret@kuben.example.com")
	cfg.Notify.AllowHTTP = true
	view := supportbundle.AllowedConfig(cfg)
	shown, err := jsonx.CanonicalValue(view)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"hunter22", "admin-secret", "ops@example.com", "sso-secret", "hook-secret", "otlp-secret", "pw-secret"} {
		if strings.Contains(shown, secret) {
			t.Errorf("%s leaked: %s", secret, shown)
		}
	}
	values, _ := view["values"].(map[string]any)
	notify, _ := values["notify"].(map[string]any)
	if notify["allow_http"] != true {
		t.Errorf("notify.allow_http = %v", notify["allow_http"])
	}
	backup, _ := values["backup"].(map[string]any)
	if backup["keep"] != json.Number("7") {
		t.Errorf("backup.keep = %v", backup["keep"])
	}
	omitted, _ := view["omitted"].([]any)
	for _, key := range []string{"database.url", "bootstrap.admin_password"} {
		if !slices.Contains(omitted, any(key)) {
			t.Errorf("%s is not listed as omitted: %v", key, omitted)
		}
	}
	server, _ := values["server"].(map[string]any)
	if got := server["public_url"]; got != "https://***@kuben.example.com" {
		t.Errorf("server.public_url = %v", got)
	}
}

// logSections is a bundle with 1000 log lines and about.
func logSections(about map[string]any) map[string]any {
	lines := make([]any, 0, 1000)
	for i := range 1000 {
		lines = append(lines, fmt.Sprintf("line %04d %s", i, strings.Repeat("x", 80)))
	}
	return map[string]any{"about": about, "logs": map[string]any{"a": lines}}
}

func TestBundlesAreBoundedByCuttingLogs(t *testing.T) {
	data, err := supportbundle.Encode(logSections(map[string]any{"kuben": version.Version}), 20_000)
	if err != nil {
		t.Fatalf("does not fit: %v", err)
	}
	if len(data) > 20_000 {
		t.Errorf("%d bytes", len(data))
	}
	kept, err := jsonx.DecodeAny(data)
	if err != nil {
		t.Fatal(err)
	}
	logs, _ := kept.(map[string]any)["logs"].(map[string]any)
	a, _ := logs["a"].([]any)
	if len(a) == 0 || len(a) >= 1000 {
		t.Errorf("%d lines kept", len(a))
	}
	if last, _ := a[len(a)-1].(string); !strings.HasPrefix(last, "line 0999") {
		t.Errorf("newest not kept: %q", last)
	}
	big := logSections(map[string]any{"big": strings.Repeat("y", 30_000)})
	if _, err := supportbundle.Encode(big, 20_000); err == nil {
		t.Error("fits without logs")
	}
}

func TestOldBundlesArePruned(t *testing.T) {
	dir := t.TempDir()
	for _, stamp := range []string{"20260101T000000Z", "20260102T000000Z", "20260103T000000Z"} {
		if err := os.WriteFile(filepath.Join(dir, supportbundle.Prefix+stamp+".json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "other.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if removed, err := supportbundle.Prune(dir, 2); err != nil || removed != 1 {
		t.Fatalf("prune: %d, %v", removed, err)
	}
	if _, err := os.Stat(filepath.Join(dir, supportbundle.Prefix+"20260101T000000Z.json")); err == nil {
		t.Error("the oldest bundle is still there")
	}
	if _, err := os.Stat(filepath.Join(dir, "other.json")); err != nil {
		t.Error("another file was removed")
	}
	if removed, err := supportbundle.Prune(dir, 0); err != nil || removed != 1 {
		t.Errorf("one is always kept: %d, %v", removed, err)
	}
}

func TestStringsAreRedactedEverywhere(t *testing.T) {
	v := supportbundle.Redacted(map[string]any{
		"a": []any{"see postgres://u:secret@h/db"},
		"b": map[string]any{"c": "https://x:y@z"},
	})
	text, err := jsonx.CanonicalValue(v)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, "secret") || strings.Contains(text, "x:y@") {
		t.Errorf("not redacted: %s", text)
	}
}

// The bundle is written as serde_json's pretty printer wrote a Value.
func TestTheBundleIsPrettyPrintedAsBefore(t *testing.T) {
	got, err := supportbundle.Pretty(map[string]any{"b": []any{}, "a": map[string]any{"x": 1, "y": []any{"<&>"}}, "c": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "a": {
    "x": 1,
    "y": [
      "<&>"
    ]
  },
  "b": [],
  "c": {}
}`
	if diff := cmp.Diff(want, string(got)); diff != "" {
		t.Errorf("pretty (-want +got):\n%s", diff)
	}
}
