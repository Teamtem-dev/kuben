package doctor_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/go/hub/internal/cli/doctor"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/config"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/discovery"
	pdoctor "github.com/Teamtem-dev/kuben/go/hub/internal/platform/doctor"
)

func TestAMissingFeatureWarnsAndNeverFailsTheCheck(t *testing.T) {
	var out bytes.Buffer
	r := doctor.NewReport(&out)
	doctor.CheckCapabilities(r, discovery.ClusterFacts{})
	doctor.CheckFeatures(r, discovery.ClusterFacts{})
	if r.Failed() {
		t.Error("optional capabilities only warn")
	}
	r.Line(doctor.LevelFail, "permissions", "denied: create namespaces")
	if !r.Failed() {
		t.Error("a failed line did not fail the report")
	}
	want := `[WARN] gateway-api: not installed — install the Gateway API CRDs (standard channel) to expose apps
[WARN] cert-manager: not found — install cert-manager for automatic HTTPS
[WARN] metrics-server: not found — install metrics-server for autoscaling
[WARN] feature: public routes: needs the Gateway API CRDs (kubectl apply --server-side -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.5.1/standard-install.yaml) and a Gateway controller: helm install eg oci://docker.io/envoyproxy/gateway-helm -n envoy-gateway-system --create-namespace, then install Kuben with --set platform.gatewayClassName=eg
[WARN] feature: HTTPS: needs cert-manager with Gateway API support (--set config.enableGatewayAPI=true) and a ClusterIssuer
[WARN] feature: volumes: no default StorageClass: apps with volumes and the chart's PostgreSQL stay Pending
[WARN] feature: isolation: no known NetworkPolicy enforcer found (unknown, not proven absent): environments are not isolated from each other on the network unless the CNI enforces policies
[WARN] feature: autoscaling: needs metrics-server; apps run with fixed replicas until then
[FAIL] permissions: denied: create namespaces
`
	if diff := cmp.Diff(want, out.String()); diff != "" {
		t.Errorf("lines (-want +got):\n%s", diff)
	}
}

func TestLinesAreRedacted(t *testing.T) {
	var out bytes.Buffer
	r := doctor.NewReport(&out)
	r.Line(doctor.LevelOK, "database", "postgres reachable, migrations applied (postgres://kuben:hunter2@db/kuben)")
	if got, want := out.String(), "[OK  ] database: postgres reachable, migrations applied (postgres://***@db/kuben)\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExposureChecksOnlyWarn(t *testing.T) {
	var out bytes.Buffer
	r := doctor.NewReport(&out)
	doctor.ExposureLine(r, pdoctor.Check{
		ID: "gateway", Subject: "", Status: pdoctor.StatusFail, Detail: "does not exist",
		Hint: opt.Some("fix it"),
	})
	doctor.ExposureLine(r, pdoctor.Check{ID: "port-80", Subject: "80", Status: pdoctor.StatusUnknown, Detail: "no address"})
	want := "[WARN] gateway: does not exist — fix it\n[WARN] port-80 80: unknown: no address\n"
	if diff := cmp.Diff(want, out.String()); diff != "" || r.Failed() {
		t.Errorf("failed %v, lines (-want +got):\n%s", r.Failed(), diff)
	}
}

func TestUnreadableKubeconfigIsAFailureNotAMissingCluster(t *testing.T) {
	file := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(file, []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(file, 0o600) })
	kube := config.KubeCfg{Kubeconfig: opt.Some(file)}
	// root reads files regardless of their mode
	f, err := os.Open(file)
	readableAnyway := err == nil
	if f != nil {
		_ = f.Close()
	}
	if _, found := doctor.UnreadableKubeconfig(kube); found == readableAnyway {
		t.Errorf("unreadable found %v, readable anyway %v", found, readableAnyway)
	}
	missing := config.KubeCfg{Kubeconfig: opt.Some("/nonexistent/kubeconfig")}
	if _, found := doctor.UnreadableKubeconfig(missing); found {
		t.Error("a missing file is the setup-mode case")
	}
}
