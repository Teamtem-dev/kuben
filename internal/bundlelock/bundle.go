// Package bundle is the bundle lock (crates/kuben/src/bundle.rs, M2.17):
// what a release installs besides Kuben, pinned by version and digest.
// `bundle.lock.json` at the repository root is the one place these pins
// live; this package embeds a byte-identical copy (a test holds them
// together), `kuben setup` installs from it, the chart's PostgreSQL follows
// it, and each release publishes it under its signed checksums.
package bundle

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Lock is the lock file, as released.
//
//go:embed bundle.lock.json
var Lock string

// Pinned is a download and its SHA-256.
type Pinned struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// K3s is the pinned k3s release and its installer script.
type K3s struct {
	Version   string `json:"version"`
	Installer Pinned `json:"installer"`
}

// GatewayAPI is the pinned Gateway API release.
type GatewayAPI struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
}

// CertManager is the pinned cert-manager chart and its image digests.
type CertManager struct {
	Version string `json:"version"`
	Chart   Pinned `json:"chart"`
	// Images is the image digest per component (`controller`, `webhook`, …).
	Images map[string]string `json:"images"`
}

// Postgresql is the pinned PostgreSQL image of the chart.
type Postgresql struct {
	Image  string `json:"image"`
	Tag    string `json:"tag"`
	Digest string `json:"digest"`
}

// Bundle is the parsed lock.
type Bundle struct {
	Schema      uint32      `json:"schema"`
	K3s         K3s         `json:"k3s"`
	GatewayAPI  GatewayAPI  `json:"gatewayApi"`
	CertManager CertManager `json:"certManager"`
	Postgresql  Postgresql  `json:"postgresql"`
}

// Get parses the embedded lock. The file is part of the source and a test
// parses it, so an error here means a broken build.
func Get() (Bundle, error) {
	var b Bundle
	if err := json.Unmarshal([]byte(Lock), &b); err != nil {
		return Bundle{}, fmt.Errorf("bundle.lock.json does not parse: %w", err)
	}
	return b, nil
}

// Summary is one line per pin, for `kuben version --bundle`.
func (b Bundle) Summary() string {
	components := make([]string, 0, len(b.CertManager.Images))
	for c := range b.CertManager.Images {
		components = append(components, c)
	}
	slices.Sort(components)
	images := make([]string, 0, len(components))
	for _, c := range components {
		images = append(images, fmt.Sprintf("    %-16s %s", c, b.CertManager.Images[c]))
	}
	return fmt.Sprintf("bundle lock (schema %d)\n"+
		"k3s              %s (installer sha256 %s)\n"+
		"Gateway API      %s (sha256 %s)\n"+
		"cert-manager     %s (chart sha256 %s)\n%s\n"+
		"PostgreSQL       %s:%s@%s (Helm chart)",
		b.Schema,
		b.K3s.Version, b.K3s.Installer.SHA256,
		b.GatewayAPI.Version, b.GatewayAPI.SHA256,
		b.CertManager.Version, b.CertManager.Chart.SHA256,
		strings.Join(images, "\n"),
		b.Postgresql.Image, b.Postgresql.Tag, b.Postgresql.Digest)
}
