package httpapi_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/artifact"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/integrations/oci"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/keyring"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

// privateDigest is what `ghcr.io/acme/private:1` resolves to, for bot's
// login only (tests/http.rs PRIVATE).
const privateDigest = "sha256:3333333333333333333333333333333333333333333333333333333333333333"

// privateImages is tests/http.rs TestImages: public images resolve
// anonymously; `ghcr.io/acme/private:1` only with the login `bot`.
type privateImages struct{ oci.Fixed }

func (p privateImages) ResolveAs(ctx context.Context, image string, login opt.Val[oci.Login]) (oci.Resolved, error) {
	if image != "ghcr.io/acme/private:1" {
		return p.Fixed.ResolveAs(ctx, image, login)
	}
	if l, ok := login.Get(); ok && l.Username == "bot" && l.Password == "token" {
		d, err := artifact.ParseDigest(privateDigest)
		if err != nil {
			return oci.Resolved{}, err
		}
		return oci.Resolved{Repository: "ghcr.io/acme/private", Digest: d, Given: image}, nil
	}
	return oci.Resolved{}, oci.Unauthorized{Image: image}
}

// testKeyring is tests/http.rs's keyring: version 1, 32 bytes of 7.
func testKeyring() *keyring.Keyring {
	var key [32]byte
	for i := range key {
		key[i] = 7
	}
	return keyring.FromKeys(map[uint32][32]byte{1: key})
}

// routes/registries.rs registries_are_named_as_image_references_name_them.
func TestRegistriesAreNamedAsImageReferencesNameThem(t *testing.T) {
	for _, tc := range []struct{ given, want string }{
		{"ghcr.io", "ghcr.io"},
		{" GHCR.io ", "ghcr.io"},
		{"registry.example.com:5000", "registry.example.com:5000"},
		{"localhost:5000", "localhost:5000"},
		{"docker.io", "docker.io"},
	} {
		got, err := httpapi.RegistryName(tc.given)
		if err != nil || got != tc.want {
			t.Errorf("%q: %q %v", tc.given, got, err)
		}
	}
	for _, bad := range []string{"", "ghcr.io/acme", "acme", "ghcr.io@x", "https://ghcr.io"} {
		if _, err := httpapi.RegistryName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// routes/registries.rs logins_are_bounded.
func TestLoginsAreBounded(t *testing.T) {
	login := func(username, password string) *gen.PutRegistryLogin {
		return &gen.PutRegistryLogin{Registry: "ghcr.io", Username: username, Password: password}
	}
	if err := httpapi.CheckLogin(login("bot", "token")); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []*gen.PutRegistryLogin{
		login("", "token"),
		login("bot", ""),
		login("a:b", "token"),
		login("bot", "line\nbreak"),
		login("bot", strings.Repeat("x", httpapi.MaxLoginField+1)),
	} {
		if err := httpapi.CheckLogin(bad); err == nil {
			t.Errorf("%q accepted", bad.Username)
		}
	}
}

const registries = "/api/v1/projects/shop/environments/prod/registries"

// setGhcrLogin is tests/http.rs set_ghcr_login: shop's login for ghcr.io,
// set by alice after the refusals it gets on the way; its path.
func setGhcrLogin(t *testing.T, alice, bob *client) string {
	t.Helper()
	ghcr := registries + "/ghcr"
	body := func(registry string) map[string]any {
		return map[string]any{"registry": registry, "username": "bot", "password": "token"}
	}
	if status := bob.status("PUT", ghcr, body("ghcr.io")); status != http.StatusForbidden {
		t.Fatalf("a viewer: %d", status)
	}
	if status := alice.status("PUT", ghcr, body("ghcr.io/acme")); status != http.StatusUnprocessableEntity {
		t.Fatalf("a repository: %d", status)
	}
	status, saved, _ := alice.do("PUT", ghcr, body("GHCR.io"))
	if status != http.StatusOK || saved["registry"] != "ghcr.io" || saved["revision"] != float64(1) {
		t.Fatalf("saved: %d %v", status, saved)
	}
	if status := alice.status("PUT", registries+"/other", body("ghcr.io")); status != http.StatusConflict {
		t.Fatalf("one login per registry: %d", status)
	}
	if status := alice.status("PUT", secretsPath+"/ghcr", map[string]any{"data": map[string]any{"url": "x"}}); status != http.StatusConflict {
		t.Fatalf("the name is a login: %d", status)
	}
	status, listed := alice.list(registries)
	if status != http.StatusOK || len(listed) != 1 {
		t.Fatalf("listed: %d %v", status, listed)
	}
	if raw := alice.raw("GET", registries); strings.Contains(raw, "token") {
		t.Fatalf("passwords are never returned: %s", raw)
	}
	if status, secrets := alice.list(secretsPath); status != http.StatusOK || len(secrets) != 0 {
		t.Fatalf("logins are not app secrets: %d %v", status, secrets)
	}
	return ghcr
}

// tests/http.rs m4_registry_logins_pull_private_images.
func TestM4RegistryLoginsPullPrivateImages(t *testing.T) {
	f := newFixture(t)
	f.sqlApp()
	alice := f.signIn("alice@example.com", seedPassword)
	bob := f.signIn("bob@example.com", seedPassword)
	apps := "/api/v1/projects/shop/environments/prod/apps"
	private := map[string]any{"name": "private", "image": "ghcr.io/acme/private:1", "port": 80}
	if status := alice.status("POST", apps, private); status != http.StatusUnprocessableEntity {
		t.Fatalf("no login yet: %d", status)
	}

	ghcr := setGhcrLogin(t, alice, bob)

	if status, created, _ := alice.do("POST", apps, private); status != http.StatusCreated {
		t.Fatalf("created: %d %v", status, created)
	}
	status, runs := alice.list(apps + "/private/deployments")
	if status != http.StatusOK || len(runs) == 0 {
		t.Fatalf("runs: %d %v", status, runs)
	}
	bindings := f.runBindings(runs[0]["run"])
	if len(bindings) != 1 || bindings[0].Registry.Or("") != "ghcr.io" {
		t.Fatalf("%+v", bindings)
	}

	if status := alice.status("DELETE", ghcr, nil); status != http.StatusNoContent {
		t.Fatalf("delete: %d", status)
	}
	if status := alice.status("DELETE", ghcr, nil); status != http.StatusNotFound {
		t.Fatalf("delete again: %d", status)
	}
}

// runBindings is the secret bindings of the run id (a JSON string).
func (f fixture) runBindings(id any) []store.SecretBinding {
	f.t.Helper()
	text, _ := id.(string)
	u, err := uuid.Parse(text)
	if err != nil {
		f.t.Fatalf("run %v: %v", id, err)
	}
	ctx := f.t.Context()
	tn, err := f.store.Tenant(ctx, f.org)
	if err != nil {
		f.t.Fatal(err)
	}
	defer tn.Rollback(ctx) //nolint:errcheck // read only
	bindings, err := tn.RunSecretBindings(ctx, ids.From[ids.DeploymentRun](u))
	if err != nil {
		f.t.Fatal(err)
	}
	return bindings
}
