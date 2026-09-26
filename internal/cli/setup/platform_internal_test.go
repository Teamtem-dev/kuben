package setup

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	"github.com/Teamtem-dev/kuben/internal/bundlelock"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/kube/projection"
	"github.com/Teamtem-dev/kuben/internal/kube/render"
)

// repoRoot is the repository root, relative to this package's directory.
const repoRoot = "../../../"

func TestTheEmbeddedAgentManifestIsTheCharts(t *testing.T) {
	want, err := os.ReadFile(repoRoot + "charts/kuben/files/agent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if agentManifest != string(want) {
		t.Fatal("internal/cli/setup/agent.yaml differs from charts/kuben/files/agent.yaml: copy it over")
	}
}

func TestK3sRunsWithReservationsAndTheChosenDatastore(t *testing.T) {
	single := k3sExec(DatastoreSqlite)
	if !strings.HasPrefix(single, "server ") || !strings.Contains(single, "system-reserved=cpu=100m,memory=256Mi") ||
		!strings.Contains(single, "eviction-hard=memory.available<100Mi") || strings.Contains(single, "--cluster-init") {
		t.Errorf("sqlite: %s", single)
	}
	if !strings.HasSuffix(k3sExec(DatastoreEtcd), "--cluster-init") {
		t.Errorf("etcd: %s", k3sExec(DatastoreEtcd))
	}
	var d Datastore
	if d.String() != "sqlite" || d.debugName() != "Sqlite" || DatastoreEtcd.debugName() != "Etcd" {
		t.Error("datastore names")
	}
	if err := d.Set("etcd"); err != nil || d != DatastoreEtcd {
		t.Errorf("set etcd: %v", err)
	}
	if err := d.Set("postgres"); err == nil {
		t.Error("postgres accepted")
	}
}

func TestTheConsoleRoutePointsTheGatewayAtThisServer(t *testing.T) {
	objects := consoleObjects("kuben.apps.example.com", "203.0.113.7", 3000)
	kinds := []string{}
	for _, o := range objects {
		kinds = append(kinds, stringAt(o, "kind"))
	}
	if diff := cmp.Diff([]string{"Namespace", "Service", "EndpointSlice", "HTTPRoute"}, kinds); diff != "" {
		t.Fatalf("kinds (-want +got):\n%s", diff)
	}
	if got := stringAt(objects[0], "metadata", "labels", managedBy); got != "kuben" {
		t.Errorf("the Gateway admits it: %s", got)
	}
	slice := objects[2]
	if got := stringAt(slice, "metadata", "labels", "kubernetes.io/service-name"); got != "kuben-console" {
		t.Errorf("service name %s", got)
	}
	if got := stringAt(slice, "metadata", "labels", "endpointslice.kubernetes.io/managed-by"); got != fieldManager {
		t.Errorf("slice manager %s", got)
	}
	if got := stringAt(slice, "addressType"); got != "IPv4" {
		t.Errorf("family %s", got)
	}
	endpoints, _ := slice["endpoints"].([]any) //nolint:errcheck // checked below
	if len(endpoints) != 1 || member(endpoints[0].(map[string]any), "addresses").([]any)[0] != "203.0.113.7" {
		t.Errorf("endpoints %v", endpoints)
	}
	ports, _ := slice["ports"].([]any) //nolint:errcheck // checked below
	if len(ports) != 1 || ports[0].(map[string]any)["port"] != int64(3000) {
		t.Errorf("ports %v", ports)
	}
	view := projection.RouteViewOf(object(objects[3]))
	if diff := cmp.Diff([]string{"kuben-system/kuben"}, view.Gateways); diff != "" {
		t.Errorf("gateways (-want +got):\n%s", diff)
	}
	if len(view.Domains) != 1 || view.Domains[0].Host != "kuben.apps.example.com" {
		t.Errorf("domains %v", view.Domains)
	}
	if diff := cmp.Diff([]string{render.HostListenerName("kuben.apps.example.com")}, view.Sections); diff != "" {
		t.Errorf("sections (-want +got):\n%s", diff)
	}
	if got := stringAt(consoleObjects("kuben.x", "2001:db8::7", 3000)[2], "addressType"); got != "IPv6" {
		t.Errorf("v6 family %s", got)
	}
}

func TestTheConfigOwnsOnlyWhatSetupDecides(t *testing.T) {
	plain := kubenConfig(Wanted{}, false)
	want := map[string]any{"gatewayClassName": "traefik", "gatewayPorts": map[string]any{"http": int64(8000), "https": int64(8443)}}
	if diff := cmp.Diff(want, plain["spec"]); diff != "" {
		t.Errorf("plain spec (-want +got):\n%s", diff)
	}
	full := kubenConfig(Wanted{Domain: opt.Some("apps.example.com"), AcmeEmail: opt.Some("ops@example.com")}, true)
	if stringAt(full, "spec", "clusterIssuer") != Issuer || stringAt(full, "spec", "baseDomain") != "apps.example.com" ||
		stringAt(full, "metadata", "labels", managedBy) != fieldManager {
		t.Errorf("full: %v", full)
	}
}

func TestTheIssuerSolvesThroughKubensGateway(t *testing.T) {
	issuer := clusterIssuer("ops@example.com", true)
	if !strings.Contains(stringAt(issuer, "spec", "acme", "server"), "staging") {
		t.Error("not staging")
	}
	solvers, _ := member(issuer, "spec", "acme", "solvers").([]any) //nolint:errcheck // checked below
	if len(solvers) != 1 {
		t.Fatalf("solvers %v", solvers)
	}
	parents, _ := member(solvers[0].(map[string]any), "http01", "gatewayHTTPRoute", "parentRefs").([]any) //nolint:errcheck // checked below
	if len(parents) != 1 {
		t.Fatalf("parents %v", parents)
	}
	parent := parents[0].(map[string]any)
	if parent["name"] != "kuben" || parent["namespace"] != GatewayNamespace || parent["sectionName"] != "http" {
		t.Errorf("parent %v", parent)
	}
}

func TestPinnedObjectsParseAndChecksumsAreHex(t *testing.T) {
	b, err := bundlelock.Get()
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []map[string]any{
		traefikConfig(),
		certManagerChart(b.CertManager, "H4sI"),
		namespaceObject(),
		clusterIssuer("a@b.c", false),
	} {
		obj := object(value)
		if obj.GetAPIVersion() == "" || obj.GetKind() == "" || obj.GetName() == "" {
			t.Errorf("not a Kubernetes object: %v", value)
		}
		if _, err := json.Marshal(obj); err != nil {
			t.Errorf("%s: %v", obj.GetKind(), err)
		}
	}
	var values map[string]any
	if err := yaml.Unmarshal([]byte(certManagerValues(b.CertManager.Images)), &values); err != nil {
		t.Fatalf("cert-manager values are not YAML: %v", err)
	}
	if got := stringAt(values, "image", "digest"); got != b.CertManager.Images["controller"] {
		t.Errorf("controller digest %s", got)
	}
	for _, component := range []string{"webhook", "cainjector", "acmesolver", "startupapicheck"} {
		if got := stringAt(values, component, "image", "digest"); got != b.CertManager.Images[component] {
			t.Errorf("%s digest %s", component, got)
		}
	}
	if member(values, "config", "enableGatewayAPI") != true {
		t.Error("Gateway API support is off")
	}
	if got := sha256Hex([]byte("abc")); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Errorf("sha256 %s", got)
	}
}

func TestTheAgentManifestIsTheChartsWithItsPlaceholdersFilled(t *testing.T) {
	objects, err := agentObjects("", "2.0.0")
	if err != nil {
		t.Fatal(err)
	}
	kinds := []string{}
	for _, o := range objects {
		kinds = append(kinds, stringAt(o, "kind"))
	}
	want := []string{"ServiceAccount", "ClusterRole", "ClusterRoleBinding", "Role", "RoleBinding", "Deployment"}
	if diff := cmp.Diff(want, kinds); diff != "" {
		t.Fatalf("kinds (-want +got):\n%s", diff)
	}
	text, err := json.Marshal(objects)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(text), "__") {
		t.Errorf("a placeholder is left: %s", text)
	}
	// The API server rejects what the typed objects cannot hold.
	for _, value := range objects {
		var typed any
		switch kind := stringAt(value, "kind"); kind {
		case "Deployment":
			typed = &appsv1.Deployment{}
		case "ServiceAccount":
			typed = &corev1.ServiceAccount{}
		case "ClusterRole":
			typed = &rbacv1.ClusterRole{}
		case "ClusterRoleBinding":
			typed = &rbacv1.ClusterRoleBinding{}
		case "Role":
			typed = &rbacv1.Role{}
		case "RoleBinding":
			typed = &rbacv1.RoleBinding{}
		default:
			t.Fatalf("unexpected kind %s", kind)
		}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(value, typed); err != nil {
			t.Errorf("%s is not a valid object: %v", stringAt(value, "kind"), err)
		}
	}
	var deployment map[string]any
	for _, o := range objects {
		if stringAt(o, "kind") == "Deployment" {
			deployment = o
		}
	}
	if stringAt(deployment, "metadata", "namespace") != GatewayNamespace {
		t.Error("the Deployment is not in kuben-system")
	}
	if got := stringAt(deployment, "spec", "template", "metadata", "labels", "app.kubernetes.io/name"); got != "kuben-agent" {
		t.Errorf("pod label %s", got)
	}
	containers, _ := member(deployment, "spec", "template", "spec", "containers").([]any) //nolint:errcheck // checked below
	if len(containers) == 0 {
		t.Fatal("no container")
	}
	container := containers[0].(map[string]any)
	if diff := cmp.Diff([]any{"/kuben-agent"}, container["command"]); diff != "" {
		t.Errorf("command (-want +got):\n%s", diff)
	}
	if image, _ := container["image"].(string); !strings.HasPrefix(image, "ghcr.io/teamtem-dev/kuben:") { //nolint:errcheck // checked
		t.Errorf("image %s", image)
	}
	for _, o := range objects {
		if stringAt(o, "kind") == "ClusterRole" && member(o, "metadata", "namespace") != nil {
			t.Error("the ClusterRole is cluster-scoped")
		}
	}
}
