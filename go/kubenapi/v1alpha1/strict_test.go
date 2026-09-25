package v1alpha1_test

import (
	"testing"

	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

// serde refused a required member that is missing or null; encoding/json
// would have left its zero value.
func TestDecodeAppSpecRequiresWhatSerdeRequired(t *testing.T) {
	const runtime = `"runtime":{"processes":{"web":{"port":8080}}}`
	cases := []struct {
		json, err string
	}{
		{`{"source":{"image":"x"},` + runtime + `}`, ""},
		{`{"source":{"image":"x"}}`, "missing field `runtime`"},
		{`{"source":null,` + runtime + `}`, "missing field `source`"},
		{`{"source":{},"runtime":{}}`, "missing field `processes`"},
		{`{"source":{"git":{}},` + runtime + `}`, "missing field `repo`"},
		{`{"source":{"image":"x"},` + runtime + `,"env":[{"value":"v"}]}`, "missing field `name`"},
		{`{"source":{"image":"x"},` + runtime + `,"env":[{"name":"A","fromSecret":{"name":"db"}}]}`, "missing field `key`"},
		{`{"source":{"image":"x"},` + runtime + `,"domains":[{"tls":"auto"}]}`, "missing field `host`"},
		{`{"source":{"image":"x"},` + runtime + `,"volumes":[{"name":"data"}]}`, "missing field `mountPath`"},
		{`{"source":{"image":"x"},"runtime":{"processes":{},"healthCheck":{"port":80}}}`, "missing field `path`"},
		{`[]`, "json: cannot unmarshal array into Go value of type v1alpha1.AppSpec"},
	}
	for _, c := range cases {
		_, err := v1alpha1.DecodeAppSpec([]byte(c.json))
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != c.err {
			t.Errorf("%s: got %q, want %q", c.json, got, c.err)
		}
	}
	spec, err := v1alpha1.DecodeAppSpec([]byte(`{"source":{"image":"x"},` + runtime + `}`))
	if err != nil || spec.Runtime.Processes["web"].Size != v1alpha1.DefaultProcessSize {
		t.Fatalf("defaults apply: %+v %v", spec, err)
	}
}
