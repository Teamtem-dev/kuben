package bundlelock_test

import (
	"os"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/Teamtem-dev/kuben/internal/bundlelock"
	"github.com/Teamtem-dev/kuben/internal/core/compat"
)

// root is the repository root, relative to this package's directory.
const root = "../../"

func get(t *testing.T) bundlelock.Bundle {
	t.Helper()
	b, err := bundlelock.Get()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The embedded copy is the repository's lock, byte for byte.
func TestTheEmbeddedLockIsTheRepositorysLock(t *testing.T) {
	want, err := os.ReadFile(root + "bundle.lock.json")
	if err != nil {
		t.Fatal(err)
	}
	if bundlelock.Lock != string(want) {
		t.Fatal("internal/bundlelock/bundle.lock.json differs from the repository's bundle.lock.json: copy it over")
	}
}

func TestWhatAReleaseInstallsIsInsideTheSupportEnvelope(t *testing.T) {
	b := get(t)
	fit := func(r compat.VersionRange, version string) {
		minor, ok := compat.ParseMinor(version)
		if !ok {
			t.Fatalf("%s does not parse", version)
		}
		if got := r.Fit(minor); got != compat.Supported {
			t.Errorf("%s %s: %s", r.Name, version, got)
		}
	}
	fit(compat.Kubernetes(), b.K3s.Version)
	fit(compat.GatewayAPI(), b.GatewayAPI.Version)
	fit(compat.CertManager(), b.CertManager.Version)
	fit(compat.PostgreSQL(), b.Postgresql.Tag)
}

func isSHA256(v string) bool {
	return len(v) == 64 && strings.Trim(v, "0123456789abcdefABCDEF") == ""
}

func isDigest(v string) bool {
	hex, ok := strings.CutPrefix(v, "sha256:")
	return ok && isSHA256(hex)
}

func TestEveryPinHasAVersionAndADigest(t *testing.T) {
	b := get(t)
	if b.Schema != 1 {
		t.Errorf("schema %d", b.Schema)
	}
	if !strings.HasPrefix(b.K3s.Version, "v") || !strings.Contains(b.K3s.Version, "+k3s") {
		t.Errorf("k3s version %s", b.K3s.Version)
	}
	if !strings.Contains(b.K3s.Installer.URL, strings.ReplaceAll(b.K3s.Version, "+", "%2B")) {
		t.Errorf("k3s installer %s", b.K3s.Installer.URL)
	}
	if !strings.Contains(b.GatewayAPI.URL, "/"+b.GatewayAPI.Version+"/") {
		t.Errorf("gateway api %s", b.GatewayAPI.URL)
	}
	if !strings.HasSuffix(b.CertManager.Chart.URL, "cert-manager-"+b.CertManager.Version+".tgz") {
		t.Errorf("cert-manager chart %s", b.CertManager.Chart.URL)
	}
	for _, sum := range []string{b.K3s.Installer.SHA256, b.GatewayAPI.SHA256, b.CertManager.Chart.SHA256} {
		if !isSHA256(sum) {
			t.Errorf("sha256 %s", sum)
		}
	}
	components := make([]string, 0, len(b.CertManager.Images))
	for c, d := range b.CertManager.Images {
		components = append(components, c)
		if !isDigest(d) {
			t.Errorf("%s digest %s", c, d)
		}
	}
	slices.Sort(components)
	if want := []string{"acmesolver", "cainjector", "controller", "startupapicheck", "webhook"}; !slices.Equal(components, want) {
		t.Errorf("components %v, want %v", components, want)
	}
	if !isDigest(b.Postgresql.Digest) {
		t.Errorf("postgresql digest %s", b.Postgresql.Digest)
	}
	for _, url := range []string{b.K3s.Installer.URL, b.GatewayAPI.URL, b.CertManager.Chart.URL} {
		if !strings.HasPrefix(url, "https://") {
			t.Errorf("url %s", url)
		}
	}
}

func TestTheChartsPostgresqlIsTheLockedOne(t *testing.T) {
	raw, err := os.ReadFile(root + "charts/kuben/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		Postgresql struct {
			Image struct {
				Repository string `json:"repository"`
				Tag        string `json:"tag"`
				Digest     string `json:"digest"`
			} `json:"image"`
		} `json:"postgresql"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	b := get(t).Postgresql
	image := values.Postgresql.Image
	if image.Repository != b.Image || image.Tag != b.Tag || image.Digest != b.Digest {
		t.Errorf("chart postgresql %+v, lock %+v", image, b)
	}
}

func TestTheSummaryListsEveryPin(t *testing.T) {
	b := get(t)
	s := b.Summary()
	for _, want := range []string{"bundle lock (schema 1)", "k3s              " + b.K3s.Version, "    acmesolver       sha256:", "PostgreSQL       docker.io/library/postgres:17-alpine@sha256:"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary lacks %q:\n%s", want, s)
		}
	}
}
