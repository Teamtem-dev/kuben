package build_test

import (
	"errors"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/build"
)

// Ported from rescan.rs.

func TestRescanJobsScanOneDigestInIsolation(t *testing.T) {
	if first, again := build.RescanJobName(stepDigest), build.RescanJobName(stepDigest); first != again {
		t.Error("not deterministic")
	}
	if build.RescanJobName(stepDigest) == build.RescanJobName("sha256:1") || len(build.RescanJobName(stepDigest)) > 63 {
		t.Errorf("name %s", build.RescanJobName(stepDigest))
	}
	// The first 8 bytes of the digest's SHA-256, as Rust wrote them.
	if got := build.RescanJobName("sha256:1"); got != "kscan-51ae7fa00c92525c" {
		t.Errorf("name %s", got)
	}
	job := check(build.RescanJob(settings(), "registry.local/acme/shop", stepDigest)).must(t)
	pod := job.Spec.Template.Spec
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("the service-account token is mounted")
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Error("the Job retries")
	}
	if len(pod.Containers) != 1 || !hasEnv(pod.Containers[0], "KUBEN_DIGEST", stepDigest) {
		t.Errorf("containers %+v", pod.Containers)
	}
	if !slices.ContainsFunc(pod.Volumes, func(v corev1.Volume) bool { return v.Name == "push" }) {
		t.Error("no push credentials")
	}
	if len(pod.InitContainers) != 0 {
		t.Error("no source, no build")
	}
	off := settings()
	off.ScannerImage = opt.None[string]()
	if _, err := build.RescanJob(off, "r", stepDigest); !errors.Is(err, build.ErrScanningOff) {
		t.Errorf("without a scanner: %v", err)
	}
}
