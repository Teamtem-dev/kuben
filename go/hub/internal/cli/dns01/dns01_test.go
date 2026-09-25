package dns01_test

import (
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/go/hub/internal/cli/dns01"
)

func opts(staging bool) dns01.Options {
	return dns01.Options{
		Name: "letsencrypt-dns", Email: "ops@example.com", TokenFile: "/nonexistent",
		Namespace: "cert-manager", Staging: staging,
	}
}

// member walks obj along keys; an array step takes its first item.
func member(t *testing.T, obj any, keys ...string) any {
	t.Helper()
	for _, k := range keys {
		if list, ok := obj.([]any); ok {
			obj = list[0]
		}
		m, ok := obj.(map[string]any)
		if !ok {
			t.Fatalf("%s: not an object: %v", k, obj)
		}
		obj = m[k]
	}
	return obj
}

func TestIssuersAnswerDNS01ThroughCloudflare(t *testing.T) {
	secret, issuer := dns01.Manifests(opts(false), "tok")
	checks := []struct {
		got, want any
	}{
		{member(t, secret, "metadata", "name"), "letsencrypt-dns-cloudflare"},
		{member(t, secret, "metadata", "namespace"), "cert-manager"},
		{member(t, secret, "stringData", "api-token"), "tok"},
		{member(t, issuer, "spec", "acme", "server"), dns01.LetsEncrypt},
		{
			member(t, issuer, "spec", "acme", "solvers", "dns01", "cloudflare", "apiTokenSecretRef"),
			map[string]any{"name": "letsencrypt-dns-cloudflare", "key": "api-token"},
		},
		{member(t, issuer, "metadata", "name"), "letsencrypt-dns"},
	}
	for i, c := range checks {
		if diff := cmp.Diff(c.want, c.got); diff != "" {
			t.Errorf("check %d (-want +got):\n%s", i, diff)
		}
	}
	_, staging := dns01.Manifests(opts(true), "tok")
	if got := member(t, staging, "spec", "acme", "server"); got != dns01.LetsEncryptStaging {
		t.Errorf("staging server %v", got)
	}
}

func TestBadInputIsRefused(t *testing.T) {
	o := opts(false)
	o.Email = "nobody"
	if _, err := dns01.Check(o); err == nil {
		t.Error("an address without @ was accepted")
	}
	o = opts(false)
	o.TokenFile = filepath.Join(t.TempDir(), "missing")
	if _, err := dns01.Check(o); err == nil {
		t.Error("a missing token file was accepted")
	}
}

// The dry run prints what serde_yaml_ng printed: keys sorted (a
// serde_json::Value), sequences not indented under their key.
func TestTheDryRunPrintsBothObjects(t *testing.T) {
	got, err := dns01.DryRun(opts(false))
	if err != nil {
		t.Fatal(err)
	}
	want := `apiVersion: v1
kind: Secret
metadata:
  labels:
    app.kubernetes.io/managed-by: kuben
  name: letsencrypt-dns-cloudflare
  namespace: cert-manager
stringData:
  api-token: <redacted>
type: Opaque

---
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  labels:
    app.kubernetes.io/managed-by: kuben
  name: letsencrypt-dns
spec:
  acme:
    email: ops@example.com
    privateKeySecretRef:
      name: letsencrypt-dns-account
    server: https://acme-v02.api.letsencrypt.org/directory
    solvers:
    - dns01:
        cloudflare:
          apiTokenSecretRef:
            key: api-token
            name: letsencrypt-dns-cloudflare

`
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("dry run (-want +got):\n%s", diff)
	}
}
