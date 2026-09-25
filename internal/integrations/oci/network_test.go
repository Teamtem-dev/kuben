package oci_test

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/integrations/oci"
)

// Image tags resolved against real registries (ADR-032, option A;
// crates/kuben-api/tests/oci.rs). They need the network, so they run only
// with KUBEN_TEST_NETWORK=1, as the Rust tests ran only with --ignored.

func needsNetwork(t *testing.T) {
	t.Helper()
	if os.Getenv("KUBEN_TEST_NETWORK") != "1" {
		t.Skipf("skipped %s: needs the network; set KUBEN_TEST_NETWORK=1 to run it", t.Name())
	}
}

func TestDockerHubTagsResolveToDigests(t *testing.T) {
	needsNetwork(t)
	var registry oci.Registry
	busybox, err := registry.ResolveAs(t.Context(), "busybox:1.36", opt.None[oci.Login]())
	if err != nil {
		t.Fatalf("busybox: %v", err)
	}
	if busybox.Repository != "docker.io/library/busybox" || !strings.HasPrefix(busybox.Digest.String(), "sha256:") ||
		busybox.Given != "busybox:1.36" {
		t.Errorf("busybox: %+v", busybox)
	}
	nginx, err := registry.ResolveAs(t.Context(), "nginxinc/nginx-unprivileged:1.27-alpine", opt.None[oci.Login]())
	if err != nil {
		t.Fatalf("nginx: %v", err)
	}
	if nginx.Repository != "docker.io/nginxinc/nginx-unprivileged" {
		t.Errorf("nginx: %+v", nginx)
	}
	_, err = registry.ResolveAs(t.Context(), "busybox:no-such-tag-for-kuben", opt.None[oci.Login]())
	var notFound oci.NotFound
	if !errors.As(err, &notFound) {
		t.Errorf("an unknown tag: %v", err)
	}
}

func TestGhcrTagsResolveWithAnAnonymousToken(t *testing.T) {
	needsNetwork(t)
	podinfo, err := oci.Registry{}.ResolveAs(t.Context(), "ghcr.io/stefanprodan/podinfo:6.7.0", opt.None[oci.Login]())
	if err != nil {
		t.Fatalf("podinfo: %v", err)
	}
	if podinfo.Repository != "ghcr.io/stefanprodan/podinfo" || !strings.HasPrefix(podinfo.Digest.String(), "sha256:") {
		t.Errorf("podinfo: %+v", podinfo)
	}
}
