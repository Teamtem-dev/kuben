package discovery_test

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/jsonx"
	"github.com/Teamtem-dev/kuben/internal/kube/discovery"
)

// Ported from discovery.rs, plus the pinned wire form.

func readyNode(cpu, memory string, ready bool, spec corev1.NodeSpec) corev1.Node {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n"},
		Spec:       spec,
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(cpu),
				corev1.ResourceMemory: resource.MustParse(memory),
			},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}},
		},
	}
}

func TestOnlySchedulableNodesBoundAPod(t *testing.T) {
	none := corev1.NodeSpec{}
	nodes := []corev1.Node{
		readyNode("4", "8Gi", true, none),
		readyNode("3500m", "16Gi", true, none),
		readyNode("64", "512Gi", false, none),
		readyNode("32", "128Gi", true, corev1.NodeSpec{Unschedulable: true}),
		readyNode("16", "64Gi", true, corev1.NodeSpec{Taints: []corev1.Taint{
			{Key: "node-role.kubernetes.io/control-plane", Effect: corev1.TaintEffectNoSchedule},
		}}),
		readyNode("8", "32Gi", true, corev1.NodeSpec{Taints: []corev1.Taint{
			{Key: "gpu", Effect: corev1.TaintEffectPreferNoSchedule},
		}}),
	}
	want := opt.Some(discovery.NodeSize{CPUMillis: 8000, MemoryBytes: 32 << 30})
	if got := discovery.LargestNode(nodes); got != want {
		t.Fatalf("largest: %+v", got)
	}
	if got := discovery.LargestNode(nodes[2:4]); got.IsSome() {
		t.Fatalf("no schedulable node: %+v", got)
	}
	facts := discovery.ClusterFacts{LargestNode: discovery.LargestNode(nodes[:1])}
	if c, ok := facts.NodeCapacity().Get(); !ok || c.CPUMillis != 4000 {
		t.Fatalf("capacity: %+v, %v", c, ok)
	}
	var back struct {
		LargestNode struct {
			MemoryBytes uint64 `json:"memoryBytes"`
		} `json:"largestNode"`
	}
	text, err := json.Marshal(facts)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(text, &back); err != nil || back.LargestNode.MemoryBytes != 8<<30 {
		t.Fatalf("largestNode.memoryBytes: %s, %v", text, err)
	}
}

func object(name string, rest map[string]any) map[string]any {
	obj := map[string]any{"apiVersion": "x/v1", "kind": "X", "metadata": map[string]any{"name": name}}
	for k, v := range rest {
		obj[k] = v
	}
	return obj
}

func conditions(c ...map[string]any) map[string]any {
	list := make([]any, 0, len(c))
	for _, x := range c {
		list = append(list, x)
	}
	return map[string]any{"conditions": list}
}

func TestReadinessReadsTheConditionAndKeepsOnlyUsefulMessages(t *testing.T) {
	accepted := object("traefik", map[string]any{
		"spec":   map[string]any{"controllerName": "traefik.io/gateway-controller"},
		"status": conditions(map[string]any{"type": "Accepted", "status": "True", "message": "ok"}),
	})
	want := discovery.Readiness{Name: "traefik", Controller: opt.Some("traefik.io/gateway-controller"), Ready: true}
	if got := discovery.ReadinessOf(accepted, "Accepted"); got != want {
		t.Fatalf("accepted: %+v", got)
	}
	failing := object("letsencrypt", map[string]any{
		"status": conditions(map[string]any{"type": "Ready", "status": "False", "message": "account not registered"}),
	})
	r := discovery.ReadinessOf(failing, "Ready")
	if r.Ready || r.Message != opt.Some("account not registered") {
		t.Fatalf("failing: %+v", r)
	}
	if discovery.ReadinessOf(object("new", nil), "Ready").Ready {
		t.Fatal("no status yet")
	}
	c, ok := discovery.Condition(failing, "Ready")
	if !ok || c.True || c.Message != opt.Some("account not registered") {
		t.Fatalf("condition: %+v, %v", c, ok)
	}
	if _, ok := discovery.Condition(failing, "Programmed"); ok {
		t.Fatal("no such condition")
	}
}

func TestAvailabilitySeparatesMissingFromUnknown(t *testing.T) {
	facts := discovery.ClusterFacts{
		CertManager: true,
		ClusterIssuers: []discovery.Readiness{
			{Name: "le", Ready: true},
			{Name: "broken", Message: opt.Some("no account")},
		},
	}
	cases := []struct {
		name string
		got  discovery.Availability
		want discovery.Availability
	}{
		{"ready", facts.Issuer("le"), discovery.Ready{}},
		{"not ready", facts.Issuer("broken"), discovery.NotReady{Message: "no account"}},
		{"missing issuer", facts.Issuer("other"), discovery.Missing{}},
		{"missing class", facts.GatewayClass("traefik"), discovery.Missing{}},
		{"blind", discovery.ClusterFacts{CertManager: true, Unknown: []string{discovery.ProbeClusterIssuers}}.Issuer("le"), discovery.Unknown{}},
		{"offline", discovery.ClusterFacts{Unknown: []string{discovery.ProbeAPIGroups}}.GatewayClass("traefik"), discovery.Unknown{}},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: %#v, want %#v", c.name, c.got, c.want)
		}
	}
	if !discovery.UsableOrUnknown(discovery.Unknown{}) || !discovery.UsableOrUnknown(discovery.Ready{}) {
		t.Fatal("ready and unknown are usable or unknown")
	}
	if discovery.UsableOrUnknown(discovery.Missing{}) || discovery.UsableOrUnknown(discovery.NotReady{}) {
		t.Fatal("missing and not ready are not")
	}
}

func TestFactsRoundtripCompactlyAndSummarize(t *testing.T) {
	facts := discovery.ClusterFacts{
		GatewayAPI: opt.Some(discovery.GatewayAPI{
			BundleVersion: opt.Some("v1.5.1"),
			Channel:       opt.Some("standard"),
			Kinds:         []string{"GRPCRoute", "HTTPRoute"},
		}),
		GatewayClasses: []discovery.Readiness{{Name: "traefik", Ready: true}},
		MetricsAPI:     true,
	}
	text, err := json.Marshal(facts)
	if err != nil {
		t.Fatal(err)
	}
	generic, err := jsonx.DecodeAny(text)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := generic.(map[string]any)
	if !ok {
		t.Fatalf("not an object: %s", text)
	}
	if _, ok := m["unknown"]; ok {
		t.Fatal("no unknown member when nothing is unknown")
	}
	if g, ok := m["gatewayApi"].(map[string]any); !ok || g["channel"] != "standard" {
		t.Fatalf("gatewayApi.channel: %s", text)
	}
	var back discovery.ClusterFacts
	if err := json.Unmarshal(text, &back); err != nil || !back.Equal(facts) {
		t.Fatalf("back: %+v, %v", back, err)
	}
	if !facts.Serves("GRPCRoute") || facts.Serves("TLSRoute") {
		t.Fatal("serves")
	}
	want := "Gateway API v1.5.1 (standard); gateway classes: traefik; cert-manager: no; " +
		"cluster issuers: none ready; metrics: yes"
	if got := facts.Summary(); got != want {
		t.Fatalf("summary:\n%s\n%s", got, want)
	}
	var empty discovery.ClusterFacts
	if err := json.Unmarshal([]byte(`{}`), &empty); err != nil || !empty.Equal(discovery.ClusterFacts{}) {
		t.Fatalf("older or partial records still read: %+v, %v", empty, err)
	}
	unknown := discovery.ClusterFacts{Unknown: []string{"version", "networkPolicy"}}
	if got := unknown.Summary(); got != "no Gateway API; gateway classes: none ready; cert-manager: no; "+
		"cluster issuers: none ready; metrics: no; unknown: version, networkPolicy" {
		t.Fatalf("summary with unknowns: %s", got)
	}
}

func class(name string, isDefault bool) storagev1.StorageClass {
	c := storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if isDefault {
		c.Annotations = map[string]string{discovery.DefaultClassAnnotation: "true"}
	}
	return c
}

func TestStorageAndPolicyEnforcementAreReadFromTheCluster(t *testing.T) {
	if got := discovery.DefaultClass([]storagev1.StorageClass{class("slow", false), class("local-path", true)}); got != opt.Some("local-path") {
		t.Fatalf("default: %+v", got)
	}
	if got := discovery.DefaultClass([]storagev1.StorageClass{class("slow", false)}); got.IsSome() {
		t.Fatalf("no default: %+v", got)
	}
	cases := []struct {
		daemonsets, versions []string
		want                 opt.Val[string]
	}{
		{[]string{"kube-proxy", "cilium"}, nil, opt.Some("cilium")},
		{[]string{"svclb-traefik"}, []string{"v1.36.4+k3s1"}, opt.Some("k3s")},
		{[]string{"kube-flannel-ds"}, []string{"v1.34.1"}, opt.None[string]()},
		{[]string{"weave-net", "calico-node"}, []string{"v1.36.4+k3s1"}, opt.Some("calico")},
	}
	for _, c := range cases {
		if got := discovery.PolicyEnforcer(c.daemonsets, c.versions); got != c.want {
			t.Errorf("%v %v: %+v, want %+v", c.daemonsets, c.versions, got, c.want)
		}
	}
}

// fullFacts sets every member.
func fullFacts() discovery.ClusterFacts {
	return discovery.ClusterFacts{
		GatewayAPI: opt.Some(discovery.GatewayAPI{
			BundleVersion: opt.Some("v1.5.1"),
			Channel:       opt.Some("standard"),
			Kinds:         []string{"HTTPRoute", "GRPCRoute", "HTTPRoute"},
		}),
		GatewayClasses:      []discovery.Readiness{{Name: "traefik", Controller: opt.Some("traefik.io/gateway-controller"), Ready: true}},
		CertManager:         true,
		ClusterIssuers:      []discovery.Readiness{{Name: "le", Message: opt.Some("no account")}},
		MetricsAPI:          true,
		KubernetesVersion:   opt.Some("v1.36.4+k3s1"),
		DefaultStorageClass: opt.Some("local-path"),
		NetworkPolicy:       opt.Some("k3s"),
		LargestNode:         opt.Some(discovery.NodeSize{CPUMillis: 4000, MemoryBytes: 8 << 30}),
		Unknown:             []string{"version"},
	}
}

// The JSON is stored and served: pinned byte for byte as serde_json wrote
// it (struct order), and as the store writes it (canonical).
func TestWireFormIsPinned(t *testing.T) {
	cases := []struct {
		name             string
		facts            discovery.ClusterFacts
		serde, canonical string
	}{
		{
			"empty",
			discovery.ClusterFacts{},
			`{"gatewayClasses":[],"certManager":false,"clusterIssuers":[],"metricsApi":false}`,
			`{"certManager":false,"clusterIssuers":[],"gatewayClasses":[],"metricsApi":false}`,
		},
		{
			"gateway api without kinds or annotations",
			discovery.ClusterFacts{GatewayAPI: opt.Some(discovery.GatewayAPI{}), Unknown: []string{}},
			`{"gatewayApi":{"kinds":[]},"gatewayClasses":[],"certManager":false,"clusterIssuers":[],"metricsApi":false}`,
			`{"certManager":false,"clusterIssuers":[],"gatewayApi":{"kinds":[]},"gatewayClasses":[],"metricsApi":false}`,
		},
		{
			"full",
			fullFacts(),
			`{"gatewayApi":{"bundleVersion":"v1.5.1","channel":"standard","kinds":["GRPCRoute","HTTPRoute"]},` +
				`"gatewayClasses":[{"name":"traefik","controller":"traefik.io/gateway-controller","ready":true}],` +
				`"certManager":true,"clusterIssuers":[{"name":"le","ready":false,"message":"no account"}],` +
				`"metricsApi":true,"kubernetesVersion":"v1.36.4+k3s1","defaultStorageClass":"local-path",` +
				`"networkPolicy":"k3s","largestNode":{"cpuMillis":4000,"memoryBytes":8589934592},"unknown":["version"]}`,
			`{"certManager":true,"clusterIssuers":[{"message":"no account","name":"le","ready":false}],` +
				`"defaultStorageClass":"local-path","gatewayApi":{"bundleVersion":"v1.5.1","channel":"standard",` +
				`"kinds":["GRPCRoute","HTTPRoute"]},"gatewayClasses":[{"controller":"traefik.io/gateway-controller",` +
				`"name":"traefik","ready":true}],"kubernetesVersion":"v1.36.4+k3s1","largestNode":{"cpuMillis":4000,` +
				`"memoryBytes":8589934592},"metricsApi":true,"networkPolicy":"k3s","unknown":["version"]}`,
		},
	}
	for _, c := range cases {
		text, err := json.Marshal(c.facts)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if diff := cmp.Diff(c.serde, string(text)); diff != "" {
			t.Errorf("%s serde form (-want +got):\n%s", c.name, diff)
		}
		canonical, err := jsonx.CanonicalValue(c.facts)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if diff := cmp.Diff(c.canonical, canonical); diff != "" {
			t.Errorf("%s canonical form (-want +got):\n%s", c.name, diff)
		}
		var back discovery.ClusterFacts
		if err := json.Unmarshal(text, &back); err != nil || !back.Equal(c.facts) {
			t.Errorf("%s round trip: %+v, %v", c.name, back, err)
		}
	}
}

// Decoding is as strict as serde: what the Rust type required must be
// there, the rest may be absent or null.
func TestDecodingIsAsStrictAsSerde(t *testing.T) {
	refused := []string{
		`{"gatewayClasses":[{"ready":true}]}`,
		`{"clusterIssuers":[{"name":"le"}]}`,
		`{"clusterIssuers":[{"name":null,"ready":true}]}`,
		`{"largestNode":{"cpuMillis":1}}`,
		`{"largestNode":{"cpuMillis":-1,"memoryBytes":1}}`,
		`{"certManager":"yes"}`,
	}
	for _, text := range refused {
		var f discovery.ClusterFacts
		if err := json.Unmarshal([]byte(text), &f); err == nil {
			t.Errorf("accepted %s", text)
		}
	}
	var f discovery.ClusterFacts
	text := `{"gatewayApi":{"bundleVersion":null,"kinds":["b","a","b"]},"networkPolicy":null,` +
		`"clusterIssuers":[{"name":"le","ready":true,"controller":null}],"somethingNewer":1}`
	if err := json.Unmarshal([]byte(text), &f); err != nil {
		t.Fatal(err)
	}
	want := discovery.ClusterFacts{
		GatewayAPI:     opt.Some(discovery.GatewayAPI{Kinds: []string{"a", "b"}}),
		ClusterIssuers: []discovery.Readiness{{Name: "le", Ready: true}},
	}
	if !f.Equal(want) {
		t.Fatalf("decoded: %+v", f)
	}
	if g, _ := f.GatewayAPI.Get(); !cmp.Equal(g.Kinds, []string{"a", "b"}) {
		t.Fatalf("kinds are a sorted set: %v", g.Kinds)
	}
}

func TestEqualIgnoresEmptyVersusAbsentAndKindOrder(t *testing.T) {
	a := fullFacts()
	b := fullFacts()
	g, _ := b.GatewayAPI.Get()
	g.Kinds = []string{"GRPCRoute", "HTTPRoute"}
	b.GatewayAPI = opt.Some(g)
	if !a.Equal(b) {
		t.Fatal("kinds are a set")
	}
	if !(discovery.ClusterFacts{GatewayClasses: []discovery.Readiness{}}).Equal(discovery.ClusterFacts{}) {
		t.Fatal("empty equals absent")
	}
	b.MetricsAPI = false
	if a.Equal(b) {
		t.Fatal("metrics differ")
	}
	c := fullFacts()
	c.GatewayAPI = opt.None[discovery.GatewayAPI]()
	if a.Equal(c) || c.Equal(a) {
		t.Fatal("gateway API differs")
	}
}

func TestFactsGateTheIssuer(t *testing.T) {
	facts := discovery.ClusterFacts{ClusterIssuers: []discovery.Readiness{{Name: "le", Ready: false}}}
	if facts.IssuerUsableOrUnknown("le") || facts.IssuerUsableOrUnknown("other") {
		t.Fatal("a not-ready or missing issuer is not usable")
	}
	facts.Unknown = []string{discovery.ProbeClusterIssuers}
	if !facts.IssuerUsableOrUnknown("other") {
		t.Fatal("unknown when the probe failed")
	}
}
