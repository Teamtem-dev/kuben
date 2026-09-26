package render_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/jsonx"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/render"
	"github.com/Teamtem-dev/kuben/packages/api/v1alpha1"
)

// cmpOpts compares optionals and plans by their fields.
func cmpOpts() cmp.Option {
	return cmp.AllowUnexported(opt.Val[string]{}, render.Plan{})
}

func app(t *testing.T, spec string) *v1alpha1.App {
	t.Helper()
	a := &v1alpha1.App{}
	a.Name = "api"
	a.Namespace = "kb-shop-prod"
	a.Labels = map[string]string{
		v1alpha1.LabelProject:     "shop",
		v1alpha1.LabelEnvironment: "shop-prod",
	}
	if err := json.Unmarshal([]byte(spec), &a.Spec); err != nil {
		t.Fatalf("app spec: %v", err)
	}
	return a
}

func webApp(t *testing.T) *v1alpha1.App {
	t.Helper()
	return app(t, `{
		"source": { "image": "ghcr.io/acme/api@sha256:1111111111111111111111111111111111111111111111111111111111111111" },
		"runtime": { "processes": {
			"web": { "port": 3000, "size": "small", "replicas": { "min": 2, "max": 4 } },
			"worker": { "command": ["bin/worker"], "size": "nano" }
		}, "healthCheck": { "path": "/healthz" } },
		"env": [
			{ "name": "LOG_LEVEL", "value": "info" },
			{ "name": "DATABASE_URL", "fromSecret": { "name": "db", "key": "url" } }
		],
		"domains": [{ "host": "api.acme.com" }]
	}`)
}

func configSpec(t *testing.T) v1alpha1.KubenConfigSpec {
	t.Helper()
	var spec v1alpha1.KubenConfigSpec
	err := json.Unmarshal([]byte(`{
		"baseDomain": "apps.example.com",
		"gateway": "kuben-system/kuben",
		"clusterIssuer": "letsencrypt"
	}`), &spec)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return spec
}

func capabilities(t *testing.T) render.Capabilities {
	t.Helper()
	return render.CapabilitiesOf(render.PlatformFromSpec(configSpec(t)))
}

func mustRender(t *testing.T, a *v1alpha1.App, caps render.Capabilities) render.Plan {
	t.Helper()
	plan, err := render.Render(a, caps)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return plan
}

// pretty is the Rust test's helper: the canonical resources read back and
// printed by serde_json::to_string_pretty (two-space indent, `": "`,
// sorted keys, serde's string escaping).
func pretty(t *testing.T, plan render.Plan) string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(plan.ResourcesJSON()))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("canonical JSON: %v", err)
	}
	var b strings.Builder
	writePretty(t, &b, v, 0)
	return b.String()
}

func writePretty(t *testing.T, b *strings.Builder, v any, depth int) {
	t.Helper()
	indent := func(d int) { b.WriteString(strings.Repeat("  ", d)) }
	switch x := v.(type) {
	case []any:
		if len(x) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[\n")
		for i, item := range x {
			if i > 0 {
				b.WriteString(",\n")
			}
			indent(depth + 1)
			writePretty(t, b, item, depth+1)
		}
		b.WriteString("\n")
		indent(depth)
		b.WriteString("]")
	case map[string]any:
		if len(x) == 0 {
			b.WriteString("{}")
			return
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		b.WriteString("{\n")
		for i, k := range keys {
			if i > 0 {
				b.WriteString(",\n")
			}
			indent(depth + 1)
			writePretty(t, b, k, depth+1)
			b.WriteString(": ")
			writePretty(t, b, x[k], depth+1)
		}
		b.WriteString("\n")
		indent(depth)
		b.WriteString("}")
	default:
		// Scalars print as serde prints them, which is what the canonical
		// text of the scalar is.
		text, err := jsonx.CanonicalValue(x)
		if err != nil {
			t.Fatalf("scalar: %v", err)
		}
		b.WriteString(text)
	}
}

// snapshot is the body of the Rust insta snapshot `name`: the copy in
// testdata, which must equal the Rust file while the Rust tree exists.
func snapshot(t *testing.T, name string) string {
	t.Helper()
	file := "kuben_platform__render__tests__" + name + ".snap"
	local, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		t.Fatalf("snapshot copy: %v", err)
	}
	rust, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "..", "crates", "kuben-platform", "src", "render", "snapshots", file))
	switch {
	case err == nil && !bytes.Equal(rust, local):
		t.Fatalf("testdata/%s differs from the Rust snapshot: copy it again", file)
	case err != nil && !errors.Is(err, os.ErrNotExist):
		t.Fatalf("Rust snapshot: %v", err)
	case err != nil:
		t.Logf("the Rust tree is gone; comparing with testdata/%s", file)
	}
	// insta: a `---` header block, then the body; trailing newlines do not
	// count.
	parts := strings.SplitN(string(local), "---\n", 3)
	if len(parts) != 3 {
		t.Fatalf("snapshot %s has no header", file)
	}
	return strings.TrimRight(parts[2], "\n")
}

func assertSnapshot(t *testing.T, name, got string) {
	t.Helper()
	if want := snapshot(t, name); got != want {
		t.Fatalf("snapshot %s differs (-want +got):\n%s", name, cmp.Diff(want, got))
	}
}

// facts mirrors discovery::ClusterFacts for the ClusterIssuer probe.
type facts struct {
	issuers map[string]bool // name → Ready
	unknown bool            // the probe failed
}

func (f facts) IssuerUsableOrUnknown(name string) bool {
	ready, found := f.issuers[name]
	if found {
		return ready
	}
	return f.unknown
}

func gatedCapabilities(t *testing.T, f opt.Val[render.IssuerFacts]) render.Capabilities {
	t.Helper()
	return render.CapabilitiesOf(render.PlatformFromSpec(configSpec(t)).Gated(f))
}

func TestTheCapabilitySnapshotRoundTrips(t *testing.T) {
	caps := capabilities(t)
	platform := caps.Platform()
	if diff := cmp.Diff(caps, render.CapabilitiesOf(platform), cmpOpts()); diff != "" {
		t.Fatalf("round trip (-want +got):\n%s", diff)
	}
	if !platform.TLS {
		t.Fatal("a cluster issuer means TLS routes")
	}
	data, err := json.Marshal(caps)
	if err != nil {
		t.Fatal(err)
	}
	var back render.Capabilities
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(caps, back, cmpOpts()); diff != "" {
		t.Fatalf("JSON round trip (-want +got):\n%s", diff)
	}
}

func TestAnIssuerThatIsNotReadyBlocksOnlyTLS(t *testing.T) {
	issuer := func(ready bool) opt.Val[render.IssuerFacts] {
		return opt.Some[render.IssuerFacts](facts{issuers: map[string]bool{"letsencrypt": ready}})
	}
	blocked := gatedCapabilities(t, issuer(false))
	if !blocked.TLSUnavailable || blocked.Platform().TLS {
		t.Fatalf("blocked: %+v", blocked)
	}
	data, err := json.Marshal(blocked)
	if err != nil {
		t.Fatal(err)
	}
	var members map[string]any
	if err := json.Unmarshal(data, &members); err != nil {
		t.Fatal(err)
	}
	if members["tlsUnavailable"] != true {
		t.Fatalf("tlsUnavailable missing: %s", data)
	}
	var back render.Capabilities
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(blocked, back, cmpOpts()); diff != "" {
		t.Fatalf("JSON round trip (-want +got):\n%s", diff)
	}
	plan := mustRender(t, webApp(t), blocked)
	resources, err := plan.Resources()
	if err != nil {
		t.Fatal(err)
	}
	var route map[string]any
	for _, o := range resources.([]any) {
		if m := o.(map[string]any); m["kind"] == "HTTPRoute" {
			route = m
		}
	}
	if route == nil {
		t.Fatal("the route is still rendered")
	}
	parent := route["spec"].(map[string]any)["parentRefs"].([]any)[0].(map[string]any)
	if _, ok := parent["sectionName"]; ok {
		t.Fatal("plain HTTP: the route attaches to the gateway, not to an HTTPS listener")
	}

	ready := gatedCapabilities(t, issuer(true))
	if diff := cmp.Diff(capabilities(t), ready, cmpOpts()); diff != "" {
		t.Fatalf("a Ready issuer changes nothing (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(capabilities(t), gatedCapabilities(t, opt.None[render.IssuerFacts]()), cmpOpts()); diff != "" {
		t.Fatalf("unknown facts keep the configured intent (-want +got):\n%s", diff)
	}
	data, err = json.Marshal(ready)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("tlsUnavailable")) {
		t.Fatalf("older snapshots stay byte-identical: %s", data)
	}
}

func TestThePlanHoldsWhatTheAppControllerBuildsWithoutOwners(t *testing.T) {
	plan := mustRender(t, webApp(t), capabilities(t))
	var inventory [][2]string
	for _, i := range plan.Inventory {
		inventory = append(inventory, [2]string{i.Kind, i.Name})
	}
	want := [][2]string{
		{"Deployment", "api-web"},
		{"Deployment", "api-worker"},
		{"HorizontalPodAutoscaler", "api-web"},
		{"Service", "api"},
		{"HTTPRoute", "api"},
	}
	if diff := cmp.Diff(want, inventory, cmpOpts()); diff != "" {
		t.Fatalf("inventory (-want +got):\n%s", diff)
	}
	if strings.Contains(plan.ResourcesJSON(), "ownerReferences") || strings.Contains(plan.ResourcesJSON(), `"status"`) {
		t.Fatal("owners and status are left out")
	}
	if plan.RendererVersion() != render.RendererVersion {
		t.Fatal("renderer version")
	}
	envelope := plan.Envelope("plan-1")
	if envelope.Digest != plan.Digest || envelope.Resources != plan.ResourcesJSON() || envelope.ID != "plan-1" {
		t.Fatalf("envelope: %+v", envelope)
	}
}

func TestRenderingIsDeterministicAndContentAddressed(t *testing.T) {
	first := mustRender(t, webApp(t), capabilities(t))
	again := mustRender(t, webApp(t), capabilities(t))
	if diff := cmp.Diff(first, again, cmpOpts()); diff != "" {
		t.Fatalf("not deterministic (-first +again):\n%s", diff)
	}
	if first.Digest != jsonx.SHA256(first.ResourcesJSON()) {
		t.Fatal("the digest is the hash of the resources")
	}

	changed := webApp(t)
	debug := "debug"
	changed.Spec.Env[0].Value = &debug
	other := mustRender(t, changed, capabilities(t))
	if other.Digest == first.Digest {
		t.Fatal("another env value")
	}
	if other.InventoryHash != first.InventoryHash {
		t.Fatal("the same objects")
	}

	caps := capabilities(t)
	caps.Gateway = opt.None[string]()
	unrouted := mustRender(t, webApp(t), caps)
	if unrouted.Digest == first.Digest {
		t.Fatal("another capability snapshot")
	}
	if unrouted.InventoryHash == first.InventoryHash {
		t.Fatal("no route without a gateway")
	}
}

func TestCanonicalJSONSortsKeysAtEveryLevel(t *testing.T) {
	got, err := jsonx.Canonical([]byte(`{ "b": [ { "y": 1, "x": null } ], "a": "\"q\"" }`))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"a":"\"q\"","b":[{"x":null,"y":1}]}`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestAPlanLargerThanTheEnvelopeIsRefused(t *testing.T) {
	big := webApp(t)
	huge := strings.Repeat("x", v1alpha1.MaxEnvelopeBytes)
	big.Spec.Env[0].Value = &huge
	_, err := render.Render(big, capabilities(t))
	var tooLarge *render.TooLargeError
	if !errors.As(err, &tooLarge) || tooLarge.Bytes <= tooLarge.Max {
		t.Fatalf("expected TooLarge, got %v", err)
	}
}

func TestASpecTheControllerRefusesIsRefused(t *testing.T) {
	broken := app(t, `{
		"source": { "image": "ghcr.io/acme/api:1.2.3" },
		"runtime": { "processes": { "web": { "port": 3000, "size": "no-such-size" } } }
	}`)
	_, err := render.Render(broken, capabilities(t))
	var build *render.BuildError
	if !errors.As(err, &build) {
		t.Fatalf("expected a build error, got %v", err)
	}
}

func TestGoldenWebApp(t *testing.T) {
	plan := mustRender(t, webApp(t), capabilities(t))
	assertSnapshot(t, "golden_web_app", pretty(t, plan))
}

func TestGoldenCronJobAndVolume(t *testing.T) {
	cron := app(t, `{
		"source": { "image": "busybox@sha256:2222222222222222222222222222222222222222222222222222222222222222" },
		"runtime": { "processes": {
			"tick": { "command": ["sh", "-c", "echo tick"], "schedule": "*/30 * * * *", "size": "nano" }
		} }
	}`)
	store := app(t, `{
		"source": { "image": "nginx@sha256:3333333333333333333333333333333333333333333333333333333333333333" },
		"runtime": { "processes": { "web": { "port": 8080, "size": "small" } } },
		"volumes": [{ "name": "data", "mountPath": "/data", "size": "5Gi" }]
	}`)
	// The CRD defaults: the size presets, no gateway and no TLS.
	caps := render.CapabilitiesOf(render.DefaultPlatform())
	assertSnapshot(t, "cron_job", pretty(t, mustRender(t, cron, caps)))
	assertSnapshot(t, "volume", pretty(t, mustRender(t, store, caps)))
}
