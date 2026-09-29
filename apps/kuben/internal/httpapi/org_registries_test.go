package httpapi_test

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/integrations/oci"
)

// CheckLogin accepts bot's login to ghcr.io and every login to a registry
// without logins; the registry of `down.example.com` cannot be reached.
func (privateImages) CheckLogin(_ context.Context, registry string, login oci.Login) error {
	switch registry {
	case "ghcr.io":
		if login.Username == "bot" && login.Password == "token" {
			return nil
		}
		return oci.Unauthorized{Image: registry}
	case "down.example.com":
		return oci.Unreachable{Image: registry, Reason: "HTTP 503 Service Unavailable"}
	}
	return nil
}

func TestRegistryServersFollowTheirPreset(t *testing.T) {
	for _, tc := range []struct {
		preset, given, want string
	}{
		{"dockerhub", "", "docker.io"},
		{"dockerhub", "Docker.io", "docker.io"},
		{"ghcr", "", "ghcr.io"},
		{"gitlab", " ", "registry.gitlab.com"},
		{"quay", "quay.io", "quay.io"},
		{"harbor", "harbor.example.com", "harbor.example.com"},
		{"custom", "registry.example.com:5000", "registry.example.com:5000"},
	} {
		got, err := httpapi.RegistryServer(tc.preset, tc.given)
		if err != nil || got != tc.want {
			t.Errorf("%s %q: %q %v", tc.preset, tc.given, got, err)
		}
	}
	for _, bad := range []struct{ preset, given string }{
		{"ghcr", "quay.io"},
		{"harbor", ""},
		{"custom", ""},
		{"custom", "https://registry.example.com"},
		{"custom", "registry.example.com/acme"},
	} {
		if got, err := httpapi.RegistryServer(bad.preset, bad.given); err == nil {
			t.Errorf("%s %q accepted as %q", bad.preset, bad.given, got)
		}
	}
}

func TestNewRegistryLoginsAreChecked(t *testing.T) {
	good := func() *gen.CreateOrgRegistry {
		return &gen.CreateOrgRegistry{Name: "ghcr", Preset: gen.RegistryPresetIdDtoGhcr, Username: "bot", Password: "token"}
	}
	if err := httpapi.CheckNewRegistry(good()); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(*gen.CreateOrgRegistry){
		"name":           func(r *gen.CreateOrgRegistry) { r.Name = "Not A Label" },
		"preset":         func(r *gen.CreateOrgRegistry) { r.Preset = "nexus" },
		"empty user":     func(r *gen.CreateOrgRegistry) { r.Username = "" },
		"colon":          func(r *gen.CreateOrgRegistry) { r.Username = "a:b" },
		"long user":      func(r *gen.CreateOrgRegistry) { r.Username = strings.Repeat("u", 256) },
		"empty password": func(r *gen.CreateOrgRegistry) { r.Password = "" },
		"control":        func(r *gen.CreateOrgRegistry) { r.Password = "line\nbreak" },
		"server":         func(r *gen.CreateOrgRegistry) { r.Server = gen.NewOptString("quay.io") },
	} {
		r := good()
		edit(r)
		if err := httpapi.CheckNewRegistry(r); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

const orgRegistries = "/api/v1/registries"

func TestPresetsNameTheirServers(t *testing.T) {
	f := newFixture(t)
	bob := f.signIn("bob@example.com", seedPassword)
	status, presets := bob.list(orgRegistries + "/presets")
	if status != http.StatusOK {
		t.Fatalf("a viewer reads the presets: %d", status)
	}
	servers := map[string]any{}
	for _, p := range presets {
		servers[p["id"].(string)] = p["server"]
	}
	want := map[string]any{
		"dockerhub": "docker.io", "ghcr": "ghcr.io", "gitlab": "registry.gitlab.com", "quay": "quay.io",
		"harbor": nil, "custom": nil,
	}
	if diff := cmp.Diff(want, servers); diff != "" {
		t.Error(diff)
	}
}

func TestOrgRegistriesAreManagedByAdmins(t *testing.T) {
	f := newFixture(t)
	alice := f.signIn("alice@example.com", seedPassword)
	bob := f.signIn("bob@example.com", seedPassword)
	body := map[string]any{"name": "ghcr", "preset": "ghcr", "username": "bot", "password": "token"}

	if status := bob.status("POST", orgRegistries, body); status != http.StatusForbidden {
		t.Fatalf("a viewer: %d", status)
	}
	if status, _ := bob.list(orgRegistries); status != http.StatusForbidden {
		t.Fatalf("a viewer reads no logins: %d", status)
	}
	status, check, _ := alice.do("POST", orgRegistries+"/test", map[string]any{
		"name": "ghcr", "preset": "ghcr", "username": "bot", "password": "wrong",
	})
	if status != http.StatusOK || check["ok"] != false || check["error"] != "the registry refused the username or password" {
		t.Fatalf("a wrong password: %d %v", status, check)
	}
	if status, check, _ = alice.do("POST", orgRegistries+"/test", body); status != http.StatusOK || check["ok"] != true {
		t.Fatalf("the right one: %d %v", status, check)
	}

	status, created, _ := alice.do("POST", orgRegistries, body)
	if status != http.StatusCreated || created["server"] != "ghcr.io" || created["hasPassword"] != true {
		t.Fatalf("created: %d %v", status, created)
	}
	if status := alice.status("POST", orgRegistries, body); status != http.StatusConflict {
		t.Fatalf("the name again: %d", status)
	}
	other := map[string]any{"name": "other", "preset": "custom", "server": "ghcr.io", "username": "x", "password": "y"}
	if status := alice.status("POST", orgRegistries, other); status != http.StatusConflict {
		t.Fatalf("the server again: %d", status)
	}
	if raw := alice.raw("GET", orgRegistries); strings.Contains(raw, "token") {
		t.Fatalf("passwords are never returned: %s", raw)
	}

	byID := orgRegistries + "/" + created["id"].(string)
	status, got, _ := alice.do("GET", orgRegistries+"/ghcr", nil)
	if status != http.StatusOK || got["id"] != created["id"] {
		t.Fatalf("by name: %d %v", status, got)
	}
	status, check, _ = alice.do("POST", byID+"/test", nil)
	if status != http.StatusOK || check["ok"] != true {
		t.Fatalf("the saved login: %d %v", status, check)
	}

	status, rotated, _ := alice.do("PUT", byID, map[string]any{"password": "wrong"})
	if status != http.StatusOK || rotated["updatedAt"] == nil {
		t.Fatalf("rotated: %d %v", status, rotated)
	}
	if status, check, _ = alice.do("POST", byID+"/test", nil); status != http.StatusOK || check["ok"] != false {
		t.Fatalf("the rotated login: %d %v", status, check)
	}
	status, got, _ = alice.do("GET", byID, nil)
	if status != http.StatusOK || got["lastError"] != "the registry refused the username or password" || got["lastCheckedAt"] == nil {
		t.Fatalf("the check is recorded: %d %v", status, got)
	}
	if status := alice.status("PUT", byID, map[string]any{"server": "quay.io"}); status != http.StatusUnprocessableEntity {
		t.Fatalf("a preset keeps its server: %d", status)
	}
	if status, renamed, _ := alice.do("PUT", byID, map[string]any{"name": "github"}); status != http.StatusOK || renamed["name"] != "github" {
		t.Fatalf("renamed: %d %v", status, renamed)
	}

	if status := bob.status("DELETE", byID, nil); status != http.StatusForbidden {
		t.Fatalf("a viewer deletes: %d", status)
	}
	if status := alice.status("DELETE", byID, nil); status != http.StatusNoContent {
		t.Fatalf("delete: %d", status)
	}
	if status := alice.status("DELETE", byID, nil); status != http.StatusNotFound {
		t.Fatalf("delete again: %d", status)
	}

	events, err := f.store.ListAudit(t.Context(), f.org, opt.None[int64](), 100)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, e := range events {
		if strings.HasPrefix(e.Action, "registry.") {
			actions = append(actions, e.Action)
		}
	}
	slices.Sort(actions)
	want := []string{"registry.created", "registry.deleted", "registry.updated", "registry.updated"}
	if diff := cmp.Diff(want, actions); diff != "" {
		t.Errorf("audit: %s", diff)
	}
}

func TestUnreachableRegistriesFailTheCheck(t *testing.T) {
	f := newFixture(t)
	alice := f.signIn("alice@example.com", seedPassword)
	status, check, _ := alice.do("POST", orgRegistries+"/test", map[string]any{
		"name": "down", "preset": "harbor", "server": "down.example.com", "username": "robot$x", "password": "p",
	})
	if status != http.StatusOK || check["ok"] != false ||
		check["error"] != "cannot reach the registry: HTTP 503 Service Unavailable" {
		t.Fatalf("%d %v", status, check)
	}
}
