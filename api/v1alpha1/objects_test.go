package v1alpha1_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/runtime"
	kjson "k8s.io/apimachinery/pkg/runtime/serializer/json"

	"github.com/Teamtem-dev/kuben/api/v1alpha1"
)

// fixture is one testdata/objects/<kind>-<name>.in.json with what the Rust
// types made of it (see testdata/rust-oracle.rs.txt): the JSON they wrote
// back, or the reason they refused it.
type fixture struct {
	name     string
	kind     kind
	input    []byte
	rust     []byte
	rustErr  string
	refusing bool
}

func fixtures(t *testing.T) []fixture {
	t.Helper()
	inputs, err := filepath.Glob("testdata/objects/*.in.json")
	if err != nil || len(inputs) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	var out []fixture
	for _, path := range inputs {
		name := strings.TrimSuffix(filepath.Base(path), ".in.json")
		kindName, _, _ := strings.Cut(name, "-")
		f := fixture{name: name, kind: kindNamed(t, kindName)}
		if f.input, err = os.ReadFile(path); err != nil {
			t.Fatal(err)
		}
		base := strings.TrimSuffix(path, ".in.json")
		if msg, err := os.ReadFile(base + ".rust.err"); err == nil {
			f.refusing, f.rustErr = true, strings.TrimSpace(string(msg))
		} else if f.rust, err = os.ReadFile(base + ".rust.json"); err != nil {
			t.Fatalf("%s: neither .rust.json nor .rust.err: %v", name, err)
		}
		out = append(out, f)
	}
	return out
}

// TestObjectsEncodeAsTheRustTypesDid decodes every fixture with the Go
// type and expects what the Rust type wrote back: the same members, the
// same defaults filled in, the same members left out.
func TestObjectsEncodeAsTheRustTypesDid(t *testing.T) {
	for _, f := range fixtures(t) {
		t.Run(f.name, func(t *testing.T) {
			obj := f.kind.object()
			err := json.Unmarshal(f.input, obj)
			if f.refusing {
				if err == nil {
					t.Fatalf("decoded; Rust refused it: %s", f.rustErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if diff := cmp.Diff(toMap(t, f.rust), toMap(t, mustJSON(t, obj))); diff != "" {
				t.Errorf("encoding differs from Rust (-rust +go):\n%s", diff)
			}
		})
	}
}

// TestRustOutputIsAFixedPoint decodes what Rust wrote and expects the same
// JSON back, byte for byte for spec and status (member order included).
func TestRustOutputIsAFixedPoint(t *testing.T) {
	for _, f := range fixtures(t) {
		if f.refusing {
			continue
		}
		t.Run(f.name, func(t *testing.T) {
			obj := f.kind.object()
			if err := json.Unmarshal(f.rust, obj); err != nil {
				t.Fatalf("decode: %v", err)
			}
			got := mustJSON(t, obj)
			if diff := cmp.Diff(toMap(t, f.rust), toMap(t, got)); diff != "" {
				t.Errorf("not a fixed point (-rust +go):\n%s", diff)
			}
			want, have := members(t, f.rust), members(t, got)
			for _, name := range []string{"spec", "status"} {
				if !bytes.Equal(want[name], have[name]) {
					t.Errorf("%s bytes differ:\nrust %s\ngo   %s", name, want[name], have[name])
				}
			}
		})
	}
}

// TestObjectsSurviveTheKubernetesCodecs sends every fixture through what
// client-go uses: the scheme's JSON serializer (case-sensitive decoding)
// and the unstructured converter of the dynamic client, plus DeepCopy.
func TestObjectsSurviveTheKubernetesCodecs(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	serializer := kjson.NewSerializerWithOptions(kjson.DefaultMetaFactory, scheme, scheme, kjson.SerializerOptions{})
	for _, f := range fixtures(t) {
		if f.refusing {
			continue
		}
		t.Run(f.name, func(t *testing.T) {
			want := toMap(t, f.rust)
			obj, _, err := serializer.Decode(f.input, nil, nil)
			if err != nil {
				t.Fatalf("serializer decode: %v", err)
			}
			var buf bytes.Buffer
			if err := serializer.Encode(obj, &buf); err != nil {
				t.Fatalf("serializer encode: %v", err)
			}
			if diff := cmp.Diff(want, toMap(t, buf.Bytes())); diff != "" {
				t.Errorf("serializer (-rust +go):\n%s", diff)
			}
			if diff := cmp.Diff(want, toMap(t, mustJSON(t, obj.DeepCopyObject()))); diff != "" {
				t.Errorf("deep copy (-rust +go):\n%s", diff)
			}

			unstructured, ok := toMap(t, f.input).(map[string]any)
			if !ok {
				t.Fatal("fixture is not an object")
			}
			typed := f.kind.object()
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(unstructured, typed); err != nil {
				t.Fatalf("from unstructured: %v", err)
			}
			back, err := runtime.DefaultUnstructuredConverter.ToUnstructured(typed)
			if err != nil {
				t.Fatalf("to unstructured: %v", err)
			}
			if diff := cmp.Diff(want, toMap(t, mustJSON(t, back))); diff != "" {
				t.Errorf("unstructured (-rust +go):\n%s", diff)
			}
		})
	}
}

// members splits a JSON object into its members' compact text.
func members(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for name, value := range raw {
		var buf bytes.Buffer
		if err := json.Compact(&buf, value); err != nil {
			t.Fatal(err)
		}
		out[name] = buf.Bytes()
	}
	return out
}
