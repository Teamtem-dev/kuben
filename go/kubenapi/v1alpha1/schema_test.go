package v1alpha1_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// knownManifestDefects are disagreements between the frozen manifest and
// the Rust types that the structural walk finds, keyed by CRD kind and
// schema path. They are real: the test fails when one disappears, so the
// entry is removed together with the fix.
//
// The Rust generator wrote the IdleMode variant `off` unquoted. YAML 1.1
// readers (sigs.k8s.io/yaml, so kubectl and Helm) read it as the boolean
// false, which makes the default and the first enum member of idle.mode a
// boolean where the Rust type (and the Go one) has the string "off". The
// running binary applied the CRDs from the Rust types, not from this file,
// so only a Helm install of the chart's crds/ sees the boolean.
var knownManifestDefects = map[string][]string{
	"App": {
		"spec.runtime.processes.*.idle.mode: default false, want a string",
		"spec.runtime.processes.*.idle.mode: enum member false is not a string",
		"spec.runtime.processes.*.idle.mode: enum [throttle zero], Go [off throttle zero]",
	},
	"Release": {
		"spec.appSpec.runtime.processes.*.idle.mode: default false, want a string",
		"spec.appSpec.runtime.processes.*.idle.mode: enum member false is not a string",
		"spec.appSpec.runtime.processes.*.idle.mode: enum [throttle zero], Go [off throttle zero]",
	},
}

// rootFieldsOutsideTheSchema are the members every object has that the
// kube-rs generator left out of the root schema (the apiserver knows them).
var rootFieldsOutsideTheSchema = []string{"apiVersion", "kind", "metadata"}

func TestTheGoTypesAgreeWithTheManifest(t *testing.T) {
	src := parseMarkers(t)
	for _, k := range kinds {
		t.Run(k.kind, func(t *testing.T) {
			w := walker{markers: src}
			w.object("", rootSchema(t, crdOf(t, k.kind)), reflect.TypeOf(k.object()).Elem(), true)
			want := knownManifestDefects[k.kind]
			sort.Strings(w.problems)
			sort.Strings(want)
			if diff := cmp.Diff(want, w.problems); diff != "" {
				t.Errorf("schema disagreements (-known +found):\n%s", diff)
			}
		})
	}
}

// walker compares a structural schema with a Go type and collects every
// disagreement.
type walker struct {
	markers  sourceMarkers
	problems []string
}

func (w *walker) fail(path, format string, args ...any) {
	w.problems = append(w.problems, path+": "+fmt.Sprintf(format, args...))
}

// goField is a JSON member of a Go struct.
type goField struct {
	typ       reflect.Type
	omitempty bool
	owner     string
}

// jsonFields returns the JSON members of a struct type, with inline
// embedded structs flattened.
func jsonFields(typ reflect.Type) map[string]goField {
	out := map[string]goField{}
	for i := range typ.NumField() {
		f := typ.Field(i)
		tag := f.Tag.Get("json")
		name, opts, _ := strings.Cut(tag, ",")
		if name == "-" {
			continue
		}
		if f.Anonymous && name == "" {
			for n, g := range jsonFields(f.Type) {
				out[n] = g
			}
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[name] = goField{typ: f.Type, omitempty: slices.Contains(strings.Split(opts, ","), "omitempty"), owner: typ.Name()}
	}
	return out
}

func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

// object compares an object schema with a struct type.
func (w *walker) object(path string, s *apiextensionsv1.JSONSchemaProps, typ reflect.Type, root bool) {
	if typ.Kind() != reflect.Struct {
		w.fail(path, "schema object, Go %s", typ)
		return
	}
	fields := jsonFields(typ)
	if root {
		for _, name := range rootFieldsOutsideTheSchema {
			if _, ok := fields[name]; !ok {
				w.fail(path, "Go type has no %s", name)
			}
			delete(fields, name)
		}
	}
	var goNames, schemaNames, goRequired []string
	for name, f := range fields {
		goNames = append(goNames, name)
		if !f.omitempty && !w.markers.has(f.owner, name, "+optional") {
			goRequired = append(goRequired, name)
		}
	}
	for name := range s.Properties {
		schemaNames = append(schemaNames, name)
	}
	sort.Strings(goNames)
	sort.Strings(schemaNames)
	sort.Strings(goRequired)
	required := slices.Clone(s.Required)
	sort.Strings(required)
	if !slices.Equal(goNames, schemaNames) {
		w.fail(path, "properties %v, Go %v", schemaNames, goNames)
	}
	if !slices.Equal(goRequired, required) {
		w.fail(path, "required %v, Go %v", required, goRequired)
	}
	for _, name := range schemaNames {
		f, ok := fields[name]
		if !ok {
			continue
		}
		prop := s.Properties[name]
		w.value(join(path, name), &prop, f.typ)
	}
	w.defaults(path, s, typ)
}

// value compares any schema with a Go type.
func (w *walker) value(path string, s *apiextensionsv1.JSONSchemaProps, typ reflect.Type) {
	pointer := typ.Kind() == reflect.Pointer
	if pointer {
		typ = typ.Elem()
	}
	if s.Nullable != pointer {
		w.fail(path, "nullable %t, Go pointer %t", s.Nullable, pointer)
	}
	if len(s.Enum) > 0 {
		w.enum(path, s, typ)
	}
	switch s.Type {
	case "string":
		if typ.Kind() != reflect.String {
			w.fail(path, "string, Go %s", typ)
		}
	case "integer":
		w.integer(path, s, typ)
	case "boolean":
		if typ.Kind() != reflect.Bool {
			w.fail(path, "boolean, Go %s", typ)
		}
	case "array":
		if typ.Kind() != reflect.Slice || s.Items == nil || s.Items.Schema == nil {
			w.fail(path, "array, Go %s", typ)
			return
		}
		w.value(path+"[]", s.Items.Schema, typ.Elem())
	case "object":
		if s.AdditionalProperties != nil {
			if typ.Kind() != reflect.Map || typ.Key().Kind() != reflect.String || s.AdditionalProperties.Schema == nil {
				w.fail(path, "map, Go %s", typ)
				return
			}
			w.value(path+".*", s.AdditionalProperties.Schema, typ.Elem())
			return
		}
		w.object(path, s, typ, false)
	default:
		w.fail(path, "schema type %q", s.Type)
	}
}

// integerFormats maps Go integer kinds to the schema format the Rust
// generator wrote for the same width and signedness.
var integerFormats = map[reflect.Kind]string{
	reflect.Int32: "int32", reflect.Int64: "int64", reflect.Uint16: "uint16", reflect.Uint32: "uint32",
}

func (w *walker) integer(path string, s *apiextensionsv1.JSONSchemaProps, typ reflect.Type) {
	format, ok := integerFormats[typ.Kind()]
	if !ok {
		w.fail(path, "integer, Go %s", typ)
		return
	}
	if s.Format != "" && s.Format != format {
		w.fail(path, "format %s, Go %s", s.Format, typ)
	}
}

// enum checks that the Go type accepts exactly the manifest's members: the
// kubebuilder Enum marker lists the same strings, each of them decodes,
// and a string outside the set does not.
func (w *walker) enum(path string, s *apiextensionsv1.JSONSchemaProps, typ reflect.Type) {
	var members []string
	for _, raw := range s.Enum {
		var member string
		if err := json.Unmarshal(raw.Raw, &member); err != nil {
			w.fail(path, "enum member %s is not a string", raw.Raw)
			continue
		}
		members = append(members, member)
		if err := json.Unmarshal(raw.Raw, reflect.New(typ).Interface()); err != nil {
			w.fail(path, "enum member %s does not decode: %v", raw.Raw, err)
		}
	}
	marker, ok := w.markers.typeMarker(typ.Name(), "+kubebuilder:validation:Enum=")
	if !ok {
		w.fail(path, "Go %s has no Enum marker", typ)
		return
	}
	goMembers := strings.Split(marker, ";")
	sort.Strings(goMembers)
	sort.Strings(members)
	if !slices.Equal(goMembers, members) {
		w.fail(path, "enum %v, Go %v", members, goMembers)
	}
	if err := json.Unmarshal([]byte(`"no-such-member"`), reflect.New(typ).Interface()); err == nil {
		w.fail(path, "Go %s accepts an unknown member", typ)
	}
}

// defaults checks every default of an object's properties: the smallest
// object the schema accepts, decoded into the Go type and encoded again,
// carries the manifest's default value for each defaulted property.
func (w *walker) defaults(path string, s *apiextensionsv1.JSONSchemaProps, typ reflect.Type) {
	var defaulted []string
	for name, prop := range s.Properties {
		if prop.Default != nil {
			defaulted = append(defaulted, name)
		}
	}
	if len(defaulted) == 0 {
		return
	}
	sort.Strings(defaulted)
	input, err := json.Marshal(minimal(s))
	if err != nil {
		w.fail(path, "minimal object: %v", err)
		return
	}
	v := reflect.New(typ).Interface()
	if err := json.Unmarshal(input, v); err != nil {
		w.fail(path, "minimal object %s does not decode: %v", input, err)
		return
	}
	output, err := json.Marshal(v)
	if err != nil {
		w.fail(path, "encode: %v", err)
		return
	}
	var got map[string]any
	if err := json.Unmarshal(output, &got); err != nil {
		w.fail(path, "decode %s: %v", output, err)
		return
	}
	for _, name := range defaulted {
		var want any
		if err := json.Unmarshal(s.Properties[name].Default.Raw, &want); err != nil {
			w.fail(join(path, name), "default: %v", err)
			continue
		}
		if s.Properties[name].Type == "string" {
			if _, ok := want.(string); !ok {
				w.fail(join(path, name), "default %v, want a string", want)
				continue
			}
		}
		if diff := cmp.Diff(want, got[name]); diff != "" {
			w.fail(join(path, name), "decoding without it gives %v, the manifest's default is %v", got[name], want)
		}
	}
}

// minimal returns the smallest value a schema accepts: required members
// only, each of them minimal.
func minimal(s *apiextensionsv1.JSONSchemaProps) any {
	switch s.Type {
	case "object":
		out := map[string]any{}
		for _, name := range s.Required {
			prop := s.Properties[name]
			out[name] = minimal(&prop)
		}
		return out
	case "array":
		return []any{}
	case "integer":
		return 1
	case "boolean":
		return false
	default:
		if len(s.Enum) > 0 {
			var v any
			if json.Unmarshal(s.Enum[0].Raw, &v) == nil {
				return v
			}
		}
		if strings.HasPrefix(s.Pattern, "^sha256:") {
			return "sha256:" + strings.Repeat("0", 64)
		}
		return "x"
	}
}
