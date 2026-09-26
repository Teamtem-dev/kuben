package v1alpha1_test

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"
	"sigs.k8s.io/yaml"

	"github.com/Teamtem-dev/kuben/api/v1alpha1"
)

// Ports the tests of crates/kuben-crd/src/v1alpha1/app.rs, and pins how
// Go zero values encode where the Rust types had non-zero defaults.

func TestAppSpecRoundtripsYAML(t *testing.T) {
	const doc = `
source:
  git:
    repo: https://github.com/acme/shop
    branch: main
runtime:
  processes:
    web:
      port: 8080
      replicas: { min: 2, max: 4 }
env:
  - { name: LOG_LEVEL, value: info }
domains:
  - { host: api.acme.com }
volumes:
  - { name: data, mountPath: /data }
`
	var spec v1alpha1.AppSpec
	if err := yaml.Unmarshal([]byte(doc), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Source.Git == nil || spec.Source.Git.Branch != "main" {
		t.Errorf("git source %+v", spec.Source.Git)
	}
	web := spec.Runtime.Processes["web"]
	if web.Replicas.Max != 4 {
		t.Errorf("replicas %+v", web.Replicas)
	}
	if spec.Domains[0].TLS != "auto" {
		t.Errorf("tls %q", spec.Domains[0].TLS)
	}
	if spec.Volumes[0].Size != "1Gi" {
		t.Errorf("size %q", spec.Volumes[0].Size)
	}
	if web.Protocol != v1alpha1.ProtocolHTTP {
		t.Errorf("protocol %q", web.Protocol)
	}
}

func TestScheduledTCPFieldsRoundtripAndStayCompact(t *testing.T) {
	const doc = `
source: { image: postgres:17-alpine }
runtime:
  processes:
    db: { port: 5432, protocol: tcp }
    backup: { schedule: '0 3 * * *', timeZone: Europe/Berlin }
`
	var spec v1alpha1.AppSpec
	if err := yaml.Unmarshal([]byte(doc), &spec); err != nil {
		t.Fatal(err)
	}
	if p := spec.Runtime.Processes["db"].Protocol; p != v1alpha1.ProtocolTCP {
		t.Errorf("protocol %q", p)
	}
	if s := spec.Runtime.Processes["backup"].Schedule; s == nil || *s != "0 3 * * *" {
		t.Errorf("schedule %v", s)
	}
	var encoded struct {
		Runtime struct {
			Processes map[string]map[string]any `json:"processes"`
		} `json:"runtime"`
	}
	if err := json.Unmarshal(mustJSON(t, spec), &encoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := encoded.Runtime.Processes["backup"]["protocol"]; ok {
		t.Error("the default protocol is serialized")
	}
	if encoded.Runtime.Processes["db"]["protocol"] != "tcp" {
		t.Errorf("db %v", encoded.Runtime.Processes["db"])
	}
}

// TestZeroValuesEncodeAsTheRustDefaults pins the wire form of Go values
// built in code rather than decoded: an empty enum is its Rust #[default]
// variant, and lists and maps the Rust types always wrote are [] and {},
// never null.
func TestZeroValuesEncodeAsTheRustDefaults(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  string
	}{
		{"process", v1alpha1.Process{}, `{"size":"","replicas":{"min":0,"max":0}}`},
		{"http process", v1alpha1.Process{Protocol: v1alpha1.ProtocolHTTP}, `{"size":"","replicas":{"min":0,"max":0}}`},
		{"tcp process", v1alpha1.Process{Protocol: v1alpha1.ProtocolTCP}, `{"size":"","replicas":{"min":0,"max":0},"protocol":"tcp"}`},
		{"runtime", v1alpha1.Runtime{}, `{"processes":{}}`},
		{"build", v1alpha1.Build{}, `{"strategy":"auto"}`},
		{"idle", v1alpha1.Idle{}, `{"mode":"off","after":""}`},
		{"environment", v1alpha1.EnvironmentSpec{}, `{"project":"","type":"standard","deletionPolicy":"Retain"}`},
		{"build run", v1alpha1.BuildRunSpec{}, `{"app":"","repo":"","gitRef":"","strategy":"auto","image":""}`},
		{"config", v1alpha1.KubenConfigSpec{}, `{"sizes":[]}`},
		{"size preset", v1alpha1.SizePreset{}, `{"name":"","cpuRequest":"","cpuLimit":null,"memoryRequest":"","memoryLimit":""}`},
		{"app status", v1alpha1.AppStatus{}, `{"conditions":[]}`},
		{"project status", v1alpha1.ProjectStatus{}, `{"environments":0,"conditions":[]}`},
		{"runtime status", v1alpha1.ApplicationRuntimeStatus{}, `{}`},
		{"task status", v1alpha1.ExecutionTaskStatus{}, `{}`},
		{"condition", v1alpha1.NewCondition(v1alpha1.ConditionReady, false, "Waiting"), `{"type":"Ready","status":"False","reason":"Waiting"}`},
		{"image source", v1alpha1.SourceFromImage("nginx"), `{"image":"nginx"}`},
		{"app", v1alpha1.App{}, `{"metadata":{},"spec":{"source":{},"runtime":{"processes":{}}}}`},
	}
	for _, c := range cases {
		if diff := cmp.Diff(c.want, string(mustJSON(t, c.value))); diff != "" {
			t.Errorf("%s (-want +got):\n%s", c.name, diff)
		}
	}
}

// TestEnumsWithoutDefaultRefuseTheZeroValue: ExecutionKind and Outcome had
// no Rust default, so an unset one cannot be encoded.
func TestEnumsWithoutDefaultRefuseTheZeroValue(t *testing.T) {
	for _, v := range []any{v1alpha1.ExecutionTaskSpec{}, v1alpha1.Receipt{}, v1alpha1.Protocol("udp")} {
		if data, err := json.Marshal(v); err == nil {
			t.Errorf("%T encoded as %s", v, data)
		}
	}
}

func TestParseEnums(t *testing.T) {
	if p, err := v1alpha1.ParseProtocol("tcp"); err != nil || p != v1alpha1.ProtocolTCP {
		t.Errorf("tcp: %q %v", p, err)
	}
	if _, err := v1alpha1.ParseProtocol("TCP"); err == nil {
		t.Error("TCP parsed: serde enums are case-sensitive")
	}
	if s, err := v1alpha1.ParseBuildStrategy("railpack"); err != nil || s != v1alpha1.BuildStrategyRailpack {
		t.Errorf("railpack: %q %v", s, err)
	}
	if m, err := v1alpha1.ParseIdleMode("throttle"); err != nil || m != v1alpha1.IdleModeThrottle {
		t.Errorf("throttle: %q %v", m, err)
	}
	if e, err := v1alpha1.ParseEnvironmentType("preview"); err != nil || e != v1alpha1.EnvironmentTypePreview {
		t.Errorf("preview: %q %v", e, err)
	}
	if d, err := v1alpha1.ParseDeletionPolicy("Delete"); err != nil || d != v1alpha1.DeletionPolicyDelete {
		t.Errorf("Delete: %q %v", d, err)
	}
	if k, err := v1alpha1.ParseExecutionKind("PlatformChange"); err != nil || k != v1alpha1.ExecutionKindPlatformChange {
		t.Errorf("PlatformChange: %q %v", k, err)
	}
	if o, err := v1alpha1.ParseOutcome("TimedOut"); err != nil || o != v1alpha1.OutcomeTimedOut {
		t.Errorf("TimedOut: %q %v", o, err)
	}
	if !v1alpha1.Protocol("").IsHTTP() || v1alpha1.ProtocolTCP.IsHTTP() {
		t.Error("IsHTTP")
	}
}

// TestDefaultsAreFreshValues: decoding two configs must not share the
// default size list.
func TestDefaultsAreFreshValues(t *testing.T) {
	var a, b v1alpha1.KubenConfigSpec
	if err := json.Unmarshal([]byte(`{}`), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{}`), &b); err != nil {
		t.Fatal(err)
	}
	a.Sizes[0].Name = "changed"
	if b.Sizes[0].Name != "nano" || v1alpha1.DefaultSizes()[0].Name != "nano" {
		t.Error("default sizes are shared")
	}
	if diff := cmp.Diff(v1alpha1.DefaultSizes(), b.Sizes); diff != "" {
		t.Error(diff)
	}
}
