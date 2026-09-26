// Command genspec prepares the API contract for ogen without changing its
// meaning. JSON Schema lets an object without `additionalProperties` hold
// any member, but ogen generates an empty struct for a bare
// `{"type": "object"}` and drops what it holds (a deployment's config, an
// export, the Doctor's graph). genspec states the default explicitly —
// `additionalProperties: true` on such objects — so ogen keeps them as raw
// JSON. The contract file itself stays as it is.
//
//	scripts/go-tool.sh genspec <openapi.json> <output.json>
//
// It is a tool of the tools module (tools/go.mod) and runs from go:generate
// in apps/kuben/internal/httpapi.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"slices"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "genspec:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: genspec <openapi.json> <output.json>")
	}
	data, err := os.ReadFile(args[0]) //nolint:gosec // a build tool reading the path it was given
	if err != nil {
		return err //nolint:wrapcheck // the path is in the message
	}
	out, n, err := Normalize(data)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "genspec: %d free-form objects made explicit\n", n)
	return os.WriteFile(args[1], out, 0o644) //nolint:gosec,wrapcheck // a generated source file
}

// Normalize adds `additionalProperties: true` to every object schema that
// has no properties, no additionalProperties and no composition, and says
// how many it changed.
func Normalize(data []byte) ([]byte, int, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var spec map[string]any
	if err := dec.Decode(&spec); err != nil {
		return nil, 0, fmt.Errorf("read the contract: %w", err)
	}
	if spec == nil {
		return nil, 0, fmt.Errorf("read the contract: not an object")
	}
	n := walk(spec)
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(spec); err != nil {
		return nil, 0, fmt.Errorf("write the contract: %w", err)
	}
	return b.Bytes(), n, nil
}

func walk(node any) int {
	n := 0
	switch x := node.(type) {
	case map[string]any:
		if freeForm(x) {
			x["additionalProperties"] = true
			n++
		}
		for _, v := range x {
			n += walk(v)
		}
	case []any:
		for _, v := range x {
			n += walk(v)
		}
	}
	return n
}

func freeForm(schema map[string]any) bool {
	isObject := false
	switch t := schema["type"].(type) {
	case string:
		isObject = t == "object"
	case []any:
		isObject = slices.Contains(t, any("object"))
	}
	if !isObject {
		return false
	}
	for _, k := range []string{"properties", "additionalProperties", "patternProperties", "oneOf", "anyOf", "allOf", "$ref"} {
		if _, ok := schema[k]; ok {
			return false
		}
	}
	return true
}
