package discovery

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"

	"github.com/Teamtem-dev/kuben/internal/core/capacity"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/kube/registry"
)

// The API groups whose presence is a capability.
const (
	GatewayGroup     = "gateway.networking.k8s.io"
	CertManagerGroup = "cert-manager.io"
	MetricsGroup     = "metrics.k8s.io"
)

const (
	gatewayCRD    = "gateways.gateway.networking.k8s.io"
	bundleVersion = "gateway.networking.k8s.io/bundle-version"
	channel       = "gateway.networking.k8s.io/channel"
	// DefaultClassAnnotation marks the default StorageClass.
	DefaultClassAnnotation = "storageclass.kubernetes.io/is-default-class"
	// ProbeTimeout bounds each call to the API server.
	ProbeTimeout = 10 * time.Second
)

// The resources discovery reads dynamically (functions, not variables:
// there is no package-level mutable state).
func crdResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
}

func gatewayClassResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: GatewayGroup, Version: "v1", Resource: "gatewayclasses"}
}

func issuerResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: CertManagerGroup, Version: "v1", Resource: "clusterissuers"}
}

// policyEnforcers is the DaemonSets of CNIs that enforce NetworkPolicies,
// in order of precedence, and the name reported.
func policyEnforcers() [6][2]string {
	return [6][2]string{
		{"cilium", "cilium"},
		{"calico-node", "calico"},
		{"canal", "canal"},
		{"antrea-agent", "antrea"},
		{"kube-router", "kube-router"},
		{"weave-net", "weave"},
	}
}

// member is obj[key] when it is a T, else T's zero value: an absent or
// mistyped member of an unstructured object reads as nothing.
func member[T any](obj map[string]any, key string) T {
	v, ok := obj[key].(T)
	if !ok {
		var zero T
		return zero
	}
	return v
}

// ConditionState is one status condition of an object.
type ConditionState struct {
	// True is whether its status is `True`.
	True    bool
	Message opt.Val[string]
}

// Condition is the condition conditionType of obj's `status.conditions`
// (an unstructured object); false when it has none.
func Condition(obj map[string]any, conditionType string) (ConditionState, bool) {
	conditions := member[[]any](member[map[string]any](obj, "status"), "conditions")
	for _, c := range conditions {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if t, ok := m["type"].(string); !ok || t != conditionType {
			continue
		}
		out := ConditionState{True: member[string](m, "status") == "True"}
		if msg, ok := m["message"].(string); ok {
			out.Message = opt.Some(msg)
		}
		return out, true
	}
	return ConditionState{}, false
}

// readinessOf is the readiness of obj (unstructured) by its condition
// conditionType; only a reason for not being ready is kept as its message.
func readinessOf(obj map[string]any, conditionType string) Readiness {
	cond, _ := Condition(obj, conditionType)
	r := Readiness{Ready: cond.True, Name: member[string](member[map[string]any](obj, "metadata"), "name")}
	if controller, ok := member[map[string]any](obj, "spec")["controllerName"].(string); ok {
		r.Controller = opt.Some(controller)
	}
	if msg, ok := cond.Message.Get(); ok && !cond.True && msg != "" {
		r.Message = opt.Some(msg)
	}
	return r
}

// probe is ctx bounded by ProbeTimeout.
func probe(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, ProbeTimeout)
}

// listReady is the readiness of every object of resource, sorted by name.
func listReady(ctx context.Context, c registry.Cluster, resource schema.GroupVersionResource, conditionType string) ([]Readiness, error) {
	ctx, cancel := probe(ctx)
	defer cancel()
	list, err := c.Dynamic.Resource(resource).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	if list == nil {
		return nil, fmt.Errorf("listing %s: no list returned", resource.Resource)
	}
	out := make([]Readiness, 0, len(list.Items))
	for _, item := range list.Items {
		out = append(out, readinessOf(item.Object, conditionType))
	}
	slices.SortFunc(out, func(a, b Readiness) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// gatewayAPI reads the Gateway CRD's annotations and the kinds served by
// versions (group/version strings).
func gatewayAPI(ctx context.Context, logger *slog.Logger, c registry.Cluster, disc discovery.DiscoveryInterfaceWithContext,
	versions []string, unknown *[]string,
) GatewayAPI {
	var api GatewayAPI
	crd, err := func() (map[string]string, error) {
		ctx, cancel := probe(ctx)
		defer cancel()
		obj, err := c.Dynamic.Resource(crdResource()).Get(ctx, gatewayCRD, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		return obj.GetAnnotations(), nil
	}()
	switch {
	case err == nil:
		if v, ok := crd[bundleVersion]; ok {
			api.BundleVersion = opt.Some(v)
		}
		if v, ok := crd[channel]; ok {
			api.Channel = opt.Some(v)
		}
	case apierrors.IsNotFound(err):
	default:
		logger.Debug("cannot read the Gateway CRD", "error", err)
		*unknown = append(*unknown, ProbeGatewayAPIVersion)
	}
	for _, version := range versions {
		list, err := func() (*metav1.APIResourceList, error) {
			ctx, cancel := probe(ctx)
			defer cancel()
			return disc.ServerResourcesForGroupVersionWithContext(ctx, version)
		}()
		if err != nil {
			logger.Debug("cannot list Gateway API resources", "error", err, "version", version)
			*unknown = append(*unknown, probeResourcesPrefix+version)
			continue
		}
		if list == nil {
			continue
		}
		for _, r := range list.APIResources {
			if !strings.Contains(r.Name, "/") {
				api.Kinds = append(api.Kinds, r.Kind)
			}
		}
	}
	api.Kinds = kindSet(api.Kinds)
	return api
}

// defaultClass is the default StorageClass among classes.
func defaultClass(classes []storagev1.StorageClass) opt.Val[string] {
	for _, c := range classes {
		if c.Annotations[DefaultClassAnnotation] == "true" {
			return opt.Some(c.Name)
		}
	}
	return opt.None[string]()
}

// policyEnforcer is what enforces NetworkPolicies: a known CNI's
// DaemonSet, or k3s, whose embedded controller does unless it was turned
// off.
func policyEnforcer(daemonsets, kubeletVersions []string) opt.Val[string] {
	for _, e := range policyEnforcers() {
		if slices.Contains(daemonsets, e[0]) {
			return opt.Some(e[1])
		}
	}
	if slices.ContainsFunc(kubeletVersions, func(v string) bool { return strings.Contains(v, "+k3s") }) {
		return opt.Some("k3s")
	}
	return opt.None[string]()
}

// schedulable reports whether n is ready and takes pods: not cordoned and
// without a NoSchedule or NoExecute taint.
func schedulable(n corev1.Node) bool {
	tainted := slices.ContainsFunc(n.Spec.Taints, func(t corev1.Taint) bool {
		return t.Effect == corev1.TaintEffectNoSchedule || t.Effect == corev1.TaintEffectNoExecute
	})
	ready := slices.ContainsFunc(n.Status.Conditions, func(c corev1.NodeCondition) bool {
		return c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue
	})
	return ready && !n.Spec.Unschedulable && !tainted
}

// largestNode is the largest allocatable CPU and the largest allocatable
// memory among the schedulable nodes: each an upper bound for one pod.
// Absent when no node is known to take pods.
func largestNode(nodes []corev1.Node) opt.Val[NodeSize] {
	var out opt.Val[NodeSize]
	for _, n := range nodes {
		if !schedulable(n) {
			continue
		}
		cpu, ok := n.Status.Allocatable[corev1.ResourceCPU]
		if !ok {
			continue
		}
		memory, ok := n.Status.Allocatable[corev1.ResourceMemory]
		if !ok {
			continue
		}
		size := NodeSize{}
		if size.CPUMillis, ok = capacity.CPUMillis(cpu.String()); !ok {
			continue
		}
		if size.MemoryBytes, ok = capacity.Bytes(memory.String()); !ok {
			continue
		}
		if prev, ok := out.Get(); ok {
			size = NodeSize{CPUMillis: max(prev.CPUMillis, size.CPUMillis), MemoryBytes: max(prev.MemoryBytes, size.MemoryBytes)}
		}
		out = opt.Some(size)
	}
	return out
}

// workloadFacts reads the version, storage and NetworkPolicy enforcement.
func workloadFacts(ctx context.Context, c registry.Cluster, disc discovery.DiscoveryInterfaceWithContext, facts *ClusterFacts) {
	pctx, cancel := probe(ctx)
	v, err := disc.ServerVersionWithContext(pctx)
	cancel()
	if err == nil && v != nil {
		facts.KubernetesVersion = opt.Some(v.GitVersion)
	} else {
		facts.Unknown = append(facts.Unknown, ProbeVersion)
	}

	pctx, cancel = probe(ctx)
	classes, err := c.Typed.StorageV1().StorageClasses().List(pctx, metav1.ListOptions{})
	cancel()
	if err == nil {
		facts.DefaultStorageClass = defaultClass(classes.Items)
	} else {
		facts.Unknown = append(facts.Unknown, ProbeStorageClasses)
	}

	pctx, cancel = probe(ctx)
	daemonsets, dsErr := c.Typed.AppsV1().DaemonSets(metav1.NamespaceAll).List(pctx, metav1.ListOptions{})
	cancel()
	pctx, cancel = probe(ctx)
	nodes, nodeErr := c.Typed.CoreV1().Nodes().List(pctx, metav1.ListOptions{})
	cancel()
	if dsErr != nil || nodeErr != nil {
		facts.Unknown = append(facts.Unknown, ProbeNetworkPolicy)
		return
	}
	facts.LargestNode = largestNode(nodes.Items)
	facts.NetworkPolicy = policyEnforcer(daemonSetNames(daemonsets.Items), kubeletVersions(nodes.Items))
}

func daemonSetNames(items []appsv1.DaemonSet) []string {
	out := make([]string, 0, len(items))
	for _, d := range items {
		out = append(out, d.Name)
	}
	return out
}

func kubeletVersions(nodes []corev1.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Status.NodeInfo.KubeletVersion)
	}
	return out
}

// Discover is what cluster c can do. It never fails: failed probes are
// listed in [ClusterFacts.Unknown].
func Discover(ctx context.Context, logger *slog.Logger, c registry.Cluster) ClusterFacts {
	var facts ClusterFacts
	disc := discovery.ToDiscoveryInterfaceWithContext(c.Typed.Discovery())
	if disc == nil {
		logger.Warn("no discovery client; cluster capabilities are unknown")
		facts.Unknown = append(facts.Unknown, ProbeAPIGroups)
		return facts
	}
	groups, err := func() (*metav1.APIGroupList, error) {
		ctx, cancel := probe(ctx)
		defer cancel()
		return disc.ServerGroupsWithContext(ctx)
	}()
	if err != nil || groups == nil {
		logger.Warn("cannot list API groups; cluster capabilities are unknown", "error", err)
		facts.Unknown = append(facts.Unknown, ProbeAPIGroups)
		return facts
	}
	served := func(name string) ([]string, bool) {
		i := slices.IndexFunc(groups.Groups, func(g metav1.APIGroup) bool { return g.Name == name })
		if i < 0 {
			return nil, false
		}
		versions := make([]string, 0, len(groups.Groups[i].Versions))
		for _, v := range groups.Groups[i].Versions {
			versions = append(versions, v.GroupVersion)
		}
		return versions, true
	}
	if versions, ok := served(GatewayGroup); ok {
		facts.GatewayAPI = opt.Some(gatewayAPI(ctx, logger, c, disc, versions, &facts.Unknown))
		classes, err := listReady(ctx, c, gatewayClassResource(), "Accepted")
		if err == nil {
			facts.GatewayClasses = classes
		} else {
			logger.Debug("cannot list GatewayClasses", "error", err)
			facts.Unknown = append(facts.Unknown, ProbeGatewayClasses)
		}
	}
	if _, ok := served(CertManagerGroup); ok {
		facts.CertManager = true
		issuers, err := listReady(ctx, c, issuerResource(), "Ready")
		if err == nil {
			facts.ClusterIssuers = issuers
		} else {
			logger.Debug("cannot list ClusterIssuers", "error", err)
			facts.Unknown = append(facts.Unknown, ProbeClusterIssuers)
		}
	}
	_, facts.MetricsAPI = served(MetricsGroup)
	workloadFacts(ctx, c, disc, &facts)
	return facts
}
