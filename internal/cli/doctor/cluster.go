package doctor

// The cluster's checks: reachable, its capabilities, what each feature
// needs, Kuben's permissions and, once installed, its exposure.

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"unicode"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sdiscovery "k8s.io/client-go/discovery"

	support "github.com/Teamtem-dev/kuben/internal/core/compat"
	"github.com/Teamtem-dev/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	pdoctor "github.com/Teamtem-dev/kuben/internal/doctor"
	"github.com/Teamtem-dev/kuben/internal/kube/discovery"
	"github.com/Teamtem-dev/kuben/internal/kube/registry"
)

// envoyGateway is Envoy Gateway, the documented choice when a cluster has
// no Gateway controller (ADR-031).
const envoyGateway = "helm install eg oci://docker.io/envoyproxy/gateway-helm -n envoy-gateway-system " +
	"--create-namespace, then install Kuben with --set platform.gatewayClassName=eg"

func checkCluster(ctx context.Context, r *Report, cfg config.Config, installed bool, logger *slog.Logger) {
	reg, err := registry.Connect(cfg.Kube)
	switch {
	case err != nil && !cfg.Kube.Required:
		r.Line(LevelWarn, "kubernetes", fmt.Sprintf(
			"no cluster found (setup mode). Install one (e.g. k3s) and set KUBECONFIG to its kubeconfig; details: %v", err))
		return
	case err != nil:
		r.Line(LevelFail, "kubernetes", err.Error())
		return
	}
	c := reg.Primary()
	apiserverLines(ctx, r, c)
	facts := discovery.Discover(ctx, logger, c)
	CheckCapabilities(r, facts)
	CheckFeatures(r, facts)
	checkPermissions(ctx, r, c)
	if installed {
		checkExposure(ctx, r, c, facts)
	}
}

// apiserverLines is the API server's version and how it fits the support
// envelope.
func apiserverLines(ctx context.Context, r *Report, c registry.Cluster) {
	disc := k8sdiscovery.ToDiscoveryInterfaceWithContext(c.Typed.Discovery())
	if disc == nil {
		r.Line(LevelFail, "kubernetes", "no discovery client")
		return
	}
	v, err := disc.ServerVersionWithContext(ctx)
	switch {
	case err != nil:
		r.Line(LevelFail, "kubernetes", err.Error())
	case v == nil:
		r.Line(LevelFail, "kubernetes", "the API server did not say its version")
	default:
		r.Line(LevelOK, "kubernetes", "apiserver "+v.GitVersion)
		envelopeLine(r, support.Kubernetes(), v.Major+"."+v.Minor)
	}
}

// envelopeLine is how version of a dependency fits the support envelope
// (M4.12): OK when supported, WARN when untested, FAIL when unsupported.
func envelopeLine(r *Report, rng support.VersionRange, version string) {
	minor, ok := support.ParseMinor(version)
	if !ok {
		r.Line(LevelWarn, "support envelope", fmt.Sprintf("cannot read the %s version %q", rng.Name, version))
		return
	}
	fit, text := rng.Describe(minor)
	level := LevelFail
	switch fit {
	case support.Supported:
		level = LevelOK
	case support.Untested:
		level = LevelWarn
	case support.Unsupported:
		level = LevelFail
	}
	r.Line(level, "support envelope", text)
}

// CheckCapabilities is what the cluster can do (ADR-031), as the
// controllers see it: unknown facts are warnings, never OK.
func CheckCapabilities(r *Report, facts discovery.ClusterFacts) {
	if len(facts.Unknown) > 0 {
		r.Line(LevelWarn, "capabilities", fmt.Sprintf("unknown (probes failed: %s)", strings.Join(facts.Unknown, ", ")))
	}
	if api, ok := facts.GatewayAPI.Get(); ok {
		r.Line(LevelOK, "gateway-api", fmt.Sprintf("%s (%s channel)",
			api.BundleVersion.Or("unknown version"), api.Channel.Or("unknown")))
		if version, ok := api.BundleVersion.Get(); ok {
			envelopeLine(r, support.GatewayAPI(), version)
		}
		readinessLine(r, "gateway-class", facts.GatewayClasses, "no GatewayClass is Accepted")
	} else {
		r.Line(LevelWarn, "gateway-api", "not installed — install the Gateway API CRDs (standard channel) to expose apps")
	}
	if facts.CertManager {
		r.Line(LevelOK, "cert-manager", "present")
		readinessLine(r, "cluster-issuer", facts.ClusterIssuers, "no ClusterIssuer is Ready")
	} else {
		r.Line(LevelWarn, "cert-manager", "not found — install cert-manager for automatic HTTPS")
	}
	if facts.MetricsAPI {
		r.Line(LevelOK, "metrics-server", "present")
	} else {
		r.Line(LevelWarn, "metrics-server", "not found — install metrics-server for autoscaling")
	}
}

// checkExposure is Kuben's Gateway, its issuer and the public ports, as
// the API's app Doctor judges them (M2.13). A missing piece blocks only
// exposure: a warning.
func checkExposure(ctx context.Context, r *Report, c registry.Cluster, facts discovery.ClusterFacts) {
	platform := pdoctor.ReadPlatform(ctx, c.Dynamic)
	if platform.Gateway.IsNone() {
		return // `feature: public routes` says what is missing.
	}
	gateway := pdoctor.ReadGateway(ctx, c.Dynamic, platform, net.DefaultResolver)
	checks := pdoctor.PlatformChecks(opt.Some(facts), platform, gateway)
	ports := []uint16{80}
	if platform.TLS {
		ports = []uint16{80, 443}
	}
	var dialer net.Dialer
	for _, port := range ports {
		checks = append(checks, pdoctor.PortCheck(port, pdoctor.ProbePort(ctx, &dialer, gateway.Addresses(), port)))
	}
	for _, check := range checks {
		ExposureLine(r, check)
	}
}

// ExposureLine prints one check of the app Doctor: unknown and failed
// checks only warn.
func ExposureLine(r *Report, check pdoctor.Check) {
	name := strings.TrimRightFunc(check.ID+" "+check.Subject, unicode.IsSpace)
	level, detail := LevelWarn, check.Detail
	switch check.Status {
	case pdoctor.StatusOK:
		level = LevelOK
	case pdoctor.StatusUnknown:
		detail = "unknown: " + check.Detail
	case pdoctor.StatusWarn, pdoctor.StatusFail:
	}
	if hint, ok := check.Hint.Get(); ok {
		detail += " — " + hint
	}
	r.Line(level, name, detail)
}

// CheckFeatures is what each feature needs, and whether this cluster has
// it: a missing capability blocks only its feature.
func CheckFeatures(r *Report, facts discovery.ClusterFacts) {
	accepted := anyReady(facts.GatewayClasses)
	switch {
	case facts.GatewayAPI.IsSome() && accepted:
		r.Line(LevelOK, "feature: public routes", "ready")
	case facts.GatewayAPI.IsSome():
		r.Line(LevelWarn, "feature: public routes",
			"no GatewayClass is accepted. Install a Gateway controller: "+envoyGateway)
	default:
		r.Line(LevelWarn, "feature: public routes",
			"needs the Gateway API CRDs (kubectl apply --server-side -f "+
				"https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.5.1/standard-install.yaml) "+
				"and a Gateway controller: "+envoyGateway)
	}
	issuer := anyReady(facts.ClusterIssuers)
	switch {
	case facts.CertManager && issuer:
		r.Line(LevelOK, "feature: HTTPS", "ready (set platform.clusterIssuer to a Ready issuer)")
	case facts.CertManager:
		r.Line(LevelWarn, "feature: HTTPS",
			"cert-manager has no Ready ClusterIssuer; apps are served over plain HTTP until one is")
	default:
		r.Line(LevelWarn, "feature: HTTPS",
			"needs cert-manager with Gateway API support (--set config.enableGatewayAPI=true) and a ClusterIssuer")
	}
	if class, ok := facts.DefaultStorageClass.Get(); ok {
		r.Line(LevelOK, "feature: volumes", "default StorageClass "+class)
	} else {
		r.Line(LevelWarn, "feature: volumes",
			"no default StorageClass: apps with volumes and the chart's PostgreSQL stay Pending")
	}
	if enforcer, ok := facts.NetworkPolicy.Get(); ok {
		r.Line(LevelOK, "feature: isolation", "NetworkPolicies enforced by "+enforcer)
	} else {
		r.Line(LevelWarn, "feature: isolation",
			"no known NetworkPolicy enforcer found (unknown, not proven absent): environments are not isolated "+
				"from each other on the network unless the CNI enforces policies")
	}
	if !facts.MetricsAPI {
		r.Line(LevelWarn, "feature: autoscaling", "needs metrics-server; apps run with fixed replicas until then")
	}
}

func anyReady(list []discovery.Readiness) bool {
	for _, x := range list {
		if x.Ready {
			return true
		}
	}
	return false
}

// needed is what the chart's ClusterRole grants Kuben: verb, group,
// resource.
func needed() [8][3]string {
	return [8][3]string{
		{"create", "", "namespaces"},
		{"patch", "apps", "deployments"},
		{"create", "", "secrets"},
		{"create", "gateway.networking.k8s.io", "httproutes"},
		{"create", "gateway.networking.k8s.io", "gateways"},
		{"list", "cert-manager.io", "clusterissuers"},
		{"create", "apiextensions.k8s.io", "customresourcedefinitions"},
		{"update", "coordination.k8s.io", "leases"},
	}
}

// checkPermissions is Kuben's own permissions: what the chart's
// ClusterRole grants, checked for whoever runs this.
func checkPermissions(ctx context.Context, r *Report, c registry.Cluster) {
	reviews := c.Typed.AuthorizationV1().SelfSubjectAccessReviews()
	var denied []string
	for _, n := range needed() {
		verb, group, resource := n[0], n[1], n[2]
		review := &authorizationv1.SelfSubjectAccessReview{
			Spec: authorizationv1.SelfSubjectAccessReviewSpec{
				ResourceAttributes: &authorizationv1.ResourceAttributes{Verb: verb, Group: group, Resource: resource},
			},
		}
		answer, err := reviews.Create(ctx, review, metav1.CreateOptions{})
		if err != nil {
			r.Line(LevelWarn, "permissions", fmt.Sprintf("cannot check them: %v", err))
			return
		}
		if answer == nil || !answer.Status.Allowed {
			denied = append(denied, verb+" "+resource)
		}
	}
	if len(denied) == 0 {
		r.Line(LevelOK, "permissions", "everything Kuben needs")
		return
	}
	r.Line(LevelFail, "permissions",
		fmt.Sprintf("denied: %s (the Helm chart's ClusterRole grants them)", strings.Join(denied, ", ")))
}

func readinessLine(r *Report, name string, list []discovery.Readiness, none string) {
	var ready []string
	for _, x := range list {
		if x.Ready {
			ready = append(ready, x.Name)
		}
	}
	if len(ready) > 0 {
		r.Line(LevelOK, name, strings.Join(ready, ", "))
	}
	for _, x := range list {
		if !x.Ready {
			r.Line(LevelWarn, name, fmt.Sprintf("%s not ready: %s", x.Name, x.Message.Or("no status yet")))
		}
	}
	if len(list) == 0 {
		r.Line(LevelWarn, name, none)
	}
}
