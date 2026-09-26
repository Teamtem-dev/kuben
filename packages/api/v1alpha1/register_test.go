package v1alpha1_test

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	"github.com/Teamtem-dev/kuben/packages/api/v1alpha1"
)

// Ports the tests of crates/kuben-crd/src/lib.rs, and pins the names,
// scopes and printer columns of the manifest against the Go constants and
// kubebuilder markers.

func TestCRDNamesFollowGroup(t *testing.T) {
	want := map[string]string{
		v1alpha1.AppKind:                "apps.kuben.dev",
		v1alpha1.ProjectKind:            "projects.kuben.dev",
		v1alpha1.EnvironmentKind:        "environments.kuben.dev",
		v1alpha1.ReleaseKind:            "releases.kuben.dev",
		v1alpha1.BuildRunKind:           "buildruns.kuben.dev",
		v1alpha1.KubenConfigKind:        "kubenconfigs.kuben.dev",
		v1alpha1.ApplicationRuntimeKind: "applicationruntimes.kuben.dev",
		v1alpha1.ExecutionTaskKind:      "executiontasks.kuben.dev",
	}
	for _, k := range kinds {
		if got := v1alpha1.Resource(k.plural).String(); got != want[k.kind] {
			t.Errorf("%s: %s, want %s", k.kind, got, want[k.kind])
		}
		if got := crdOf(t, k.kind).Name; got != want[k.kind] {
			t.Errorf("%s: manifest names it %s, want %s", k.kind, got, want[k.kind])
		}
	}
}

func TestAllCRDsHaveStructuralSchema(t *testing.T) {
	crds := manifest(t)
	if len(crds) != len(kinds) {
		t.Fatalf("%d CRDs in the manifest, want %d", len(crds), len(kinds))
	}
	for i, crd := range crds {
		if crd.Spec.Names.Kind != kinds[i].kind {
			t.Errorf("CRD %d is %s, want %s (install order)", i, crd.Spec.Names.Kind, kinds[i].kind)
		}
		rootSchema(t, crd)
		v := crd.Spec.Versions[0]
		if v.Subresources == nil || v.Subresources.Status == nil {
			t.Errorf("%s: no status subresource", crd.Name)
		}
		if !v.Served || !v.Storage || v.Name != v1alpha1.Version {
			t.Errorf("%s: version %s served=%t storage=%t", crd.Name, v.Name, v.Served, v.Storage)
		}
	}
}

// snapshotPath is the insta snapshot of the Rust app_crd_snapshot test.
const snapshotPath = "../../../crates/kuben-crd/src/snapshots/kuben_crd__tests__app_crd_snapshot.snap"

func TestAppCRDMatchesTheRustSnapshot(t *testing.T) {
	data, err := os.ReadFile(snapshotPath)
	if os.IsNotExist(err) {
		t.Skip("the Rust crate is gone; the manifest is pinned by the other tests")
	}
	if err != nil {
		t.Fatal(err)
	}
	// An insta snapshot is a front matter block between "---" lines, then
	// the value.
	parts := strings.SplitN(string(data), "---\n", 3)
	if len(parts) != 3 {
		t.Fatalf("unexpected snapshot layout")
	}
	var snapshot apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict([]byte(parts[2]), &snapshot); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(snapshot, crdOf(t, v1alpha1.AppKind)); diff != "" {
		t.Errorf("manifest App CRD differs from the Rust snapshot (-snapshot +manifest):\n%s", diff)
	}
}

func TestAddToSchemeRegistersEveryKind(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for _, k := range kinds {
		for _, obj := range []runtime.Object{k.object(), k.list()} {
			gvks, _, err := scheme.ObjectKinds(obj)
			if err != nil {
				t.Fatalf("%T: %v", obj, err)
			}
			if len(gvks) != 1 || gvks[0].GroupVersion() != v1alpha1.SchemeGroupVersion {
				t.Errorf("%T: %v", obj, gvks)
			}
		}
		gvk := v1alpha1.SchemeGroupVersion.WithKind(k.kind)
		if !scheme.Recognizes(gvk) || !scheme.Recognizes(gvk.GroupVersion().WithKind(k.kind+"List")) {
			t.Errorf("%s is not registered", k.kind)
		}
	}
	if v1alpha1.SchemeGroupVersion.String() != "kuben.dev/v1alpha1" || v1alpha1.FieldManager != "kuben" {
		t.Errorf("group version %s, field manager %s", v1alpha1.SchemeGroupVersion, v1alpha1.FieldManager)
	}
}

var (
	resourceMarker    = regexp.MustCompile(`^\+kubebuilder:resource:(.*)$`)
	printColumnMarker = regexp.MustCompile(`^\+kubebuilder:printcolumn:name="([^"]*)",type="([^"]*)",JSONPath="(.*)"$`)
)

func TestNamesScopesAndColumnsMatchTheManifest(t *testing.T) {
	src := parseMarkers(t)
	for _, k := range kinds {
		crd := crdOf(t, k.kind)
		names := crd.Spec.Names
		if crd.Spec.Group != v1alpha1.Group || names.Kind != k.kind || names.Plural != k.plural ||
			names.Singular != strings.ToLower(k.kind) || names.ListKind != "" && names.ListKind != k.kind+"List" {
			t.Errorf("%s: group %s names %+v", k.kind, crd.Spec.Group, names)
		}
		if !slices.Contains(src.types[k.kind], "+kubebuilder:object:root=true") ||
			!slices.Contains(src.types[k.kind+"List"], "+kubebuilder:object:root=true") ||
			!slices.Contains(src.types[k.kind], "+kubebuilder:subresource:status") {
			t.Errorf("%s: root or status marker missing", k.kind)
		}

		var resource map[string]string
		var columns []apiextensionsv1.CustomResourceColumnDefinition
		for _, line := range src.types[k.kind] {
			if m := resourceMarker.FindStringSubmatch(line); m != nil {
				resource = map[string]string{}
				for _, kv := range strings.Split(m[1], ",") {
					key, value, _ := strings.Cut(kv, "=")
					resource[key] = value
				}
			}
			if m := printColumnMarker.FindStringSubmatch(line); m != nil {
				columns = append(columns, apiextensionsv1.CustomResourceColumnDefinition{
					Name: m[1], Type: m[2], JSONPath: strings.ReplaceAll(m[3], `\"`, `"`),
				})
			}
		}
		want := map[string]string{"scope": string(crd.Spec.Scope), "shortName": strings.Join(names.ShortNames, ";")}
		got := map[string]string{"scope": resource["scope"], "shortName": resource["shortName"]}
		if path, ok := resource["path"]; ok && path != names.Plural {
			t.Errorf("%s: path marker %s, plural %s", k.kind, path, names.Plural)
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("%s: resource marker (-manifest +go):\n%s", k.kind, diff)
		}
		if diff := cmp.Diff(crd.Spec.Versions[0].AdditionalPrinterColumns, columns); diff != "" {
			t.Errorf("%s: printer columns (-manifest +go):\n%s", k.kind, diff)
		}
	}
}

func TestLabelsAndConstants(t *testing.T) {
	got := []string{
		v1alpha1.LabelManagedBy, v1alpha1.LabelManagerValue, v1alpha1.LabelOrg, v1alpha1.LabelProject,
		v1alpha1.LabelEnvironment, v1alpha1.LabelApp, v1alpha1.LabelProcess, v1alpha1.LabelGatewayOwner,
		v1alpha1.ManagedSelector, v1alpha1.ReceiptFinalizer,
	}
	want := []string{
		"app.kubernetes.io/managed-by", "kuben", "kuben.dev/org", "kuben.dev/project",
		"kuben.dev/environment", "kuben.dev/app", "kuben.dev/process", "kuben.dev/gateway-owner",
		"app.kubernetes.io/managed-by=kuben", "kuben.dev/receipt",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("(-rust +go):\n%s", diff)
	}
	if v1alpha1.MaxEnvelopeBytes != 131072 {
		t.Errorf("MaxEnvelopeBytes %d", v1alpha1.MaxEnvelopeBytes)
	}
}
