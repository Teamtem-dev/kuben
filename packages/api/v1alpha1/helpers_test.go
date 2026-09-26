package v1alpha1_test

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	"github.com/Teamtem-dev/kuben/packages/api/v1alpha1"
)

// manifestPath is the frozen CRD manifest, relative to this package.
const manifestPath = "../../../charts/kuben/crds/kuben.dev_all.yaml"

// kind ties a CRD of the manifest to its Go types.
type kind struct {
	kind, plural string
	object       func() runtime.Object
	list         func() runtime.Object
}

// kinds lists the resources in the manifest's install order.
var kinds = []kind{
	{v1alpha1.KubenConfigKind, v1alpha1.KubenConfigResource, func() runtime.Object { return &v1alpha1.KubenConfig{} }, func() runtime.Object { return &v1alpha1.KubenConfigList{} }},
	{v1alpha1.ProjectKind, v1alpha1.ProjectResource, func() runtime.Object { return &v1alpha1.Project{} }, func() runtime.Object { return &v1alpha1.ProjectList{} }},
	{v1alpha1.EnvironmentKind, v1alpha1.EnvironmentResource, func() runtime.Object { return &v1alpha1.Environment{} }, func() runtime.Object { return &v1alpha1.EnvironmentList{} }},
	{v1alpha1.AppKind, v1alpha1.AppResource, func() runtime.Object { return &v1alpha1.App{} }, func() runtime.Object { return &v1alpha1.AppList{} }},
	{v1alpha1.ReleaseKind, v1alpha1.ReleaseResource, func() runtime.Object { return &v1alpha1.Release{} }, func() runtime.Object { return &v1alpha1.ReleaseList{} }},
	{v1alpha1.BuildRunKind, v1alpha1.BuildRunResource, func() runtime.Object { return &v1alpha1.BuildRun{} }, func() runtime.Object { return &v1alpha1.BuildRunList{} }},
	{v1alpha1.ApplicationRuntimeKind, v1alpha1.ApplicationRuntimeResource, func() runtime.Object { return &v1alpha1.ApplicationRuntime{} }, func() runtime.Object { return &v1alpha1.ApplicationRuntimeList{} }},
	{v1alpha1.ExecutionTaskKind, v1alpha1.ExecutionTaskResource, func() runtime.Object { return &v1alpha1.ExecutionTask{} }, func() runtime.Object { return &v1alpha1.ExecutionTaskList{} }},
}

// kindNamed returns the entry of kinds whose kind lowercases to name.
func kindNamed(t *testing.T, name string) kind {
	t.Helper()
	for _, k := range kinds {
		if strings.ToLower(k.kind) == name {
			return k
		}
	}
	t.Fatalf("no kind %q", name)
	return kind{}
}

// manifest reads the frozen CRD manifest the way kubectl and Helm read it:
// through sigs.k8s.io/yaml (YAML 1.1 scalars).
func manifest(t *testing.T) []apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var crds []apiextensionsv1.CustomResourceDefinition
	for _, doc := range bytes.Split(data, []byte("\n---\n")) {
		doc = bytes.TrimPrefix(bytes.TrimSpace(doc), []byte("---"))
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.UnmarshalStrict(doc, &crd); err != nil {
			t.Fatalf("manifest document: %v", err)
		}
		crds = append(crds, crd)
	}
	return crds
}

// crdOf returns the manifest's CRD of a kind.
func crdOf(t *testing.T, kindName string) apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	for _, crd := range manifest(t) {
		if crd.Spec.Names.Kind == kindName {
			return crd
		}
	}
	t.Fatalf("no CRD for %s in the manifest", kindName)
	return apiextensionsv1.CustomResourceDefinition{}
}

// rootSchema returns the openAPIV3Schema of the CRD's only version.
func rootSchema(t *testing.T, crd apiextensionsv1.CustomResourceDefinition) *apiextensionsv1.JSONSchemaProps {
	t.Helper()
	if len(crd.Spec.Versions) != 1 {
		t.Fatalf("%s: %d versions, want 1", crd.Name, len(crd.Spec.Versions))
	}
	v := crd.Spec.Versions[0]
	if v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
		t.Fatalf("%s: no schema", crd.Name)
	}
	return v.Schema.OpenAPIV3Schema
}

// toMap decodes JSON text into a generic value.
func toMap(t *testing.T, data []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	return v
}

// mustJSON encodes v.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode %T: %v", v, err)
	}
	return data
}

// sourceMarkers has the +markers of the package's Go source: per type,
// and per type and JSON field name.
type sourceMarkers struct {
	types  map[string][]string
	fields map[string]map[string][]string
}

// has reports whether the field carries the marker (a prefix match).
func (m sourceMarkers) has(typeName, field, marker string) bool {
	for _, line := range m.fields[typeName][field] {
		if strings.HasPrefix(line, marker) {
			return true
		}
	}
	return false
}

// typeMarker returns the value after prefix of the type's first marker
// starting with it.
func (m sourceMarkers) typeMarker(typeName, prefix string) (string, bool) {
	for _, line := range m.types[typeName] {
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			return rest, true
		}
	}
	return "", false
}

// parseMarkers reads the markers from the package's non-test Go files.
func parseMarkers(t *testing.T) sourceMarkers {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	out := sourceMarkers{types: map[string][]string{}, fields: map[string]map[string][]string{}}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				out.types[ts.Name.Name] = markers(gen.Doc)
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				fields := map[string][]string{}
				for _, field := range st.Fields.List {
					if field.Tag == nil {
						continue
					}
					tag, err := strconv.Unquote(field.Tag.Value)
					if err != nil {
						t.Fatal(err)
					}
					jsonName, _, _ := strings.Cut(reflect.StructTag(tag).Get("json"), ",")
					fields[jsonName] = markers(field.Doc)
				}
				out.fields[ts.Name.Name] = fields
			}
		}
	}
	return out
}

// markers returns the lines of a comment group that start with "+".
func markers(doc *ast.CommentGroup) []string {
	if doc == nil {
		return nil
	}
	var out []string
	for _, c := range doc.List {
		line := strings.TrimSpace(strings.TrimPrefix(c.Text, "//"))
		if strings.HasPrefix(line, "+") {
			out = append(out, line)
		}
	}
	return out
}
