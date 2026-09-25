package main

import (
	"encoding/json"
	"testing"
)

func TestOnlyBareObjectsChange(t *testing.T) {
	in := `{"components":{"schemas":{
	  "Free":{"type":"object"},
	  "Nullable":{"type":["object","null"]},
	  "Typed":{"type":"object","properties":{"a":{"type":"string"}}},
	  "Map":{"type":"object","additionalProperties":{"type":"string"}},
	  "Closed":{"type":"object","properties":{},"additionalProperties":false},
	  "OneOf":{"type":"object","oneOf":[{"type":"object"}]},
	  "Str":{"type":"string"}}}}`
	out, n, err := Normalize([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 { // Free, Nullable and the bare object inside OneOf
		t.Fatalf("changed %d", n)
	}
	var got map[string]map[string]map[string]map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	s := got["components"]["schemas"]
	for name, want := range map[string]bool{"Free": true, "Nullable": true, "Typed": false, "Str": false} {
		_, has := s[name]["additionalProperties"]
		if has != want {
			t.Errorf("%s: additionalProperties present %v, want %v", name, has, want)
		}
	}
	if s["Closed"]["additionalProperties"] != false {
		t.Error("an explicit false stays")
	}
}
