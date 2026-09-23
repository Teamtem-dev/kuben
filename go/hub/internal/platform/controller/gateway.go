package controller

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/discovery"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/projection"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/render"
	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

// Gateway listeners and automatic HTTPS (controller::gateway, scenario 9,
// M2.2). The listener and certificate names are render's (domains.go).
//
// Kuben writes the listeners of one Gateway, KubenConfig's spec.gateway,
// and only when that Gateway is Kuben's: the one it creates itself when
// spec.gatewayClassName is set, or an existing one an operator dedicated to
// it with the label kuben.dev/gateway-owner=kuben. Any other Gateway is left
// untouched and reported on KubenConfig's Gateway condition: Kuben never
// overwrites a resource it does not own (ADR-031).
//
// With TLS (a Ready ClusterIssuer, Platform.Gated):
//
//   - `http` (:80): only the platform redirect route and cert-manager's
//     HTTP-01 solver routes attach here (`allowedRoutes: Same`), so tenants
//     can never serve plain HTTP;
//   - `https` (:443, `*.<baseDomain>`): only when wildcardTlsSecret is set;
//     covers every generated `<app>-<env>.<baseDomain>` host;
//   - `h-<hash>` (:443): one listener per other hostname, admitting routes
//     only from the namespace that owns the host (first come, first
//     served), so one tenant cannot hijack another tenant's domain.
//
// Without TLS, `http` admits the routes of Kuben's namespaces and is the
// only listener: plain HTTP keeps working while TLS is unavailable.
//
// The cert-manager.io/cluster-issuer annotation makes cert-manager's
// gateway-shim issue one certificate per listener into `kuben-tls-<hash>`.

// Names and limits of the gateway controller.
const (
	// RedirectRoute is the platform route that answers plain HTTP with a
	// redirect to HTTPS.
	RedirectRoute = "kuben-https-redirect"
	// IssuerAnnotation makes cert-manager's gateway-shim order the
	// listeners' certificates.
	IssuerAnnotation = "cert-manager.io/cluster-issuer"
	// GatewayCondition is the condition type on KubenConfig: can apps be
	// exposed through the Gateway?
	GatewayCondition = "Gateway"
	// MaxListeners is below the Gateway API's 64 listeners, leaving
	// headroom for hand-made ones.
	MaxListeners = 60
)

// ListenerPlan is the desired listeners plus what could not be served.
type ListenerPlan struct {
	Listeners []map[string]any
	// Conflicts are hosts claimed by more than one namespace (the first
	// one keeps it).
	Conflicts []string
	// Skipped are hosts without a listener because MaxListeners was
	// reached.
	Skipped []string
}

// HostClaim is a routed domain with the namespace of its route.
type HostClaim struct {
	Claim     render.DomainClaim
	Namespace string
}

func certificateRefs(secret string) map[string]any {
	return map[string]any{
		"mode":            "Terminate",
		"certificateRefs": []any{map[string]any{"kind": "Secret", "name": secret}},
	}
}

// kubenNamespaces admits routes from the namespaces Kuben manages.
func kubenNamespaces() map[string]any {
	return map[string]any{"namespaces": map[string]any{
		"from":     "Selector",
		"selector": map[string]any{"matchLabels": map[string]any{v1alpha1.LabelManagedBy: v1alpha1.LabelManagerValue}},
	}}
}

// owner is who keeps a host: the first namespace in sorted order.
type owner struct {
	namespace string
	mode      render.TLSMode
}

// owners is the owner of each host, and the conflicting claims.
func owners(hosts []HostClaim) (map[string]owner, []string, []string) {
	sorted := slices.Clone(hosts)
	slices.SortFunc(sorted, func(a, b HostClaim) int {
		return cmp.Or(cmp.Compare(a.Claim.Host, b.Claim.Host), cmp.Compare(a.Claim.TLS, b.Claim.TLS),
			cmp.Compare(a.Namespace, b.Namespace))
	})
	byHost := map[string]owner{}
	var order, conflicts []string
	for _, h := range sorted {
		existing, ok := byHost[h.Claim.Host]
		switch {
		case !ok:
			byHost[h.Claim.Host] = owner{h.Namespace, h.Claim.Mode()}
			order = append(order, h.Claim.Host)
		case existing.namespace != h.Namespace:
			conflicts = append(conflicts, fmt.Sprintf("%s: owned by %s, also requested by %s",
				h.Claim.Host, existing.namespace, h.Namespace))
		}
	}
	return byHost, order, conflicts
}

// PlanListeners is the listeners for hosts, every routed domain with its
// namespace. Deterministic: the same input always yields the same
// listeners in the same order.
func PlanListeners(hosts []HostClaim, p render.Platform) ListenerPlan {
	httpRoutes := kubenNamespaces()
	if p.TLS {
		httpRoutes = map[string]any{"namespaces": map[string]any{"from": "Same"}}
	}
	listeners := []map[string]any{{
		"name":          render.HTTPListener,
		"protocol":      "HTTP",
		"port":          p.GatewayPorts.HTTP,
		"allowedRoutes": httpRoutes,
	}}
	if !p.TLS {
		return ListenerPlan{Listeners: listeners}
	}
	byHost, order, conflicts := owners(hosts)
	base, hasBase := p.BaseDomain.Get()
	wildcard, hasWildcard := p.WildcardTLSSecret.Get()
	if hasBase && hasWildcard {
		listeners = append(listeners, map[string]any{
			"name":          render.WildcardListener,
			"protocol":      "HTTPS",
			"port":          p.GatewayPorts.HTTPS,
			"hostname":      "*." + base,
			"tls":           certificateRefs(wildcard),
			"allowedRoutes": kubenNamespaces(),
		})
	}
	var skipped []string
	for _, host := range order {
		o := byHost[host]
		if o.mode.Kind == render.TLSAuto && render.SectionForHost(host, p) == render.WildcardListener {
			continue
		}
		if len(listeners) >= MaxListeners {
			skipped = append(skipped, host)
			continue
		}
		listeners = append(listeners, hostListener(host, o, p))
	}
	return ListenerPlan{Listeners: listeners, Conflicts: conflicts, Skipped: skipped}
}

// hostListener is the listener of one host, admitting only its owner's
// routes.
func hostListener(host string, o owner, p render.Platform) map[string]any {
	onlyOwner := map[string]any{"namespaces": map[string]any{
		"from":     "Selector",
		"selector": map[string]any{"matchLabels": map[string]any{"kubernetes.io/metadata.name": o.namespace}},
	}}
	switch o.mode.Kind {
	case render.TLSPlain:
		return map[string]any{
			"name":          render.PlainListenerName(host),
			"protocol":      "HTTP",
			"port":          p.GatewayPorts.HTTP,
			"hostname":      host,
			"allowedRoutes": onlyOwner,
		}
	case render.TLSSecret:
		// The app's own certificate, readable through its ReferenceGrant.
		return httpsListener(host, map[string]any{
			"mode": "Terminate",
			"certificateRefs": []any{map[string]any{
				"kind": "Secret", "name": o.mode.Secret, "namespace": o.namespace,
			}},
		}, onlyOwner, p)
	case render.TLSAuto:
	}
	return httpsListener(host, certificateRefs(render.HostSecretName(host)), onlyOwner, p)
}

func httpsListener(host string, tls, allowed map[string]any, p render.Platform) map[string]any {
	return map[string]any{
		"name":          render.HostListenerName(host),
		"protocol":      "HTTPS",
		"port":          p.GatewayPorts.HTTPS,
		"hostname":      host,
		"tls":           tls,
		"allowedRoutes": allowed,
	}
}

// GatewayPatch is the server-side-apply body of the Gateway: Kuben's field
// manager owns only the listeners it lists, the issuer annotation while
// TLS is on, and, on the Gateway it creates, the class and the ownership
// labels.
func GatewayPatch(gateway render.GatewayRef, p render.Platform, plan ListenerPlan) map[string]any {
	listeners := make([]any, 0, len(plan.Listeners))
	for _, l := range plan.Listeners {
		listeners = append(listeners, l)
	}
	metadata := map[string]any{"name": gateway.Name, "namespace": gateway.Namespace}
	spec := map[string]any{"listeners": listeners}
	if class, ok := p.GatewayClass.Get(); ok {
		metadata["labels"] = map[string]any{
			v1alpha1.LabelManagedBy:    v1alpha1.LabelManagerValue,
			v1alpha1.LabelGatewayOwner: v1alpha1.LabelManagerValue,
		}
		spec["gatewayClassName"] = class
	}
	if issuer, ok := p.ClusterIssuer.Get(); ok && p.TLS {
		metadata["annotations"] = map[string]any{IssuerAnnotation: issuer}
	}
	return map[string]any{
		"apiVersion": gatewayGroup + "/v1",
		"kind":       "Gateway",
		"metadata":   metadata,
		"spec":       spec,
	}
}

// RedirectRouteFor is the platform route answering every plain-HTTP
// request with a 301 to HTTPS. cert-manager's solver routes carry an exact
// hostname and therefore win.
func RedirectRouteFor(gateway render.GatewayRef) map[string]any {
	return map[string]any{
		"apiVersion": gatewayGroup + "/v1",
		"kind":       "HTTPRoute",
		"metadata": map[string]any{
			"name":      RedirectRoute,
			"namespace": gateway.Namespace,
			"labels":    map[string]any{v1alpha1.LabelManagedBy: v1alpha1.LabelManagerValue},
		},
		"spec": map[string]any{
			"parentRefs": []any{map[string]any{
				"name": gateway.Name, "namespace": gateway.Namespace, "sectionName": render.HTTPListener,
			}},
			"rules": []any{map[string]any{"filters": []any{map[string]any{
				"type":            "RequestRedirect",
				"requestRedirect": map[string]any{"scheme": "https", "statusCode": 301},
			}}}},
		},
	}
}

// Ownership is who a Gateway belongs to.
type Ownership string

// The owners of a Gateway.
const (
	// OwnedByKuben: created by Kuben or dedicated to it with
	// v1alpha1.LabelGatewayOwner.
	OwnedByKuben Ownership = "kuben"
	// OwnedByOthers: someone else's; never written.
	OwnedByOthers Ownership = "foreign"
	// GatewayAbsent: no such Gateway.
	GatewayAbsent Ownership = "absent"
)

// OwnershipOf is who the live Gateway belongs to.
func OwnershipOf(live opt.Val[*unstructured.Unstructured]) Ownership {
	g, ok := live.Get()
	if !ok || g == nil {
		return GatewayAbsent
	}
	if g.GetLabels()[v1alpha1.LabelGatewayOwner] == v1alpha1.LabelManagerValue {
		return OwnedByKuben
	}
	return OwnedByOthers
}

// Verdict is the Gateway condition: its status, reason and message.
type Verdict struct {
	OK      bool
	Reason  string
	Message string
}

// Refusal is why Kuben must not write gateway, if it must not.
func Refusal(gateway render.GatewayRef, p render.Platform, owner Ownership) opt.Val[Verdict] {
	name := gateway.Namespace + "/" + gateway.Name
	switch owner {
	case OwnedByOthers:
		return opt.Some(Verdict{false, "GatewayNotOwned", fmt.Sprintf(
			"Gateway %s is not dedicated to Kuben; label it %s=%s or name another Gateway",
			name, v1alpha1.LabelGatewayOwner, v1alpha1.LabelManagerValue)})
	case GatewayAbsent:
		if p.GatewayClass.IsNone() {
			return opt.Some(Verdict{false, "GatewayMissing", fmt.Sprintf(
				"Gateway %s does not exist; create it, or set spec.gatewayClassName for Kuben to create it", name)})
		}
	case OwnedByKuben:
	}
	return opt.None[Verdict]()
}

// Readiness is the readiness of a Gateway Kuben writes, from its
// Programmed condition.
func Readiness(live *unstructured.Unstructured, p render.Platform, facts opt.Val[discovery.ClusterFacts]) Verdict {
	programmed, found := discovery.Condition(live.Object, "Programmed")
	switch {
	case found && programmed.True:
		var addresses []string
		list, _, _ := unstructured.NestedSlice(live.Object, "status", "addresses")
		for _, a := range list {
			if m, ok := a.(map[string]any); ok {
				if v, ok := m["value"].(string); ok {
					addresses = append(addresses, v)
				}
			}
		}
		message := "programmed, no address yet"
		if len(addresses) > 0 {
			message = "programmed at " + strings.Join(addresses, ", ")
		}
		if p.ClusterIssuer.IsSome() && !p.TLS {
			message += "; HTTPS is off until the ClusterIssuer is Ready"
		}
		return Verdict{true, "Programmed", message}
	case found:
		return Verdict{false, "NotProgrammed", programmed.Message.Or("")}
	}
	pending := Verdict{false, "Pending", "waiting for the gateway controller"}
	class, hasClass := p.GatewayClass.Get()
	f, hasFacts := facts.Get()
	if !hasClass || !hasFacts {
		return pending
	}
	switch a := f.GatewayClass(class).(type) {
	case discovery.Missing:
		return Verdict{false, "GatewayClassMissing", fmt.Sprintf("GatewayClass %s does not exist", class)}
	case discovery.NotReady:
		return Verdict{false, "GatewayClassNotAccepted", fmt.Sprintf("GatewayClass %s is not accepted: %s", class, a.Message)}
	case discovery.Ready, discovery.Unknown:
	}
	return pending
}

// RoutedHosts is every domain routed to gateway, with its route's
// namespace. Routes of both delivery paths carry their domains (M2.3).
func RoutedHosts(routes []*projection.RouteView, gateway render.GatewayRef) []HostClaim {
	key := gateway.Namespace + "/" + gateway.Name
	var out []HostClaim
	for _, r := range routes {
		if r == nil || !slices.Contains(r.Gateways, key) {
			continue
		}
		for _, d := range r.Domains {
			out = append(out, HostClaim{Claim: d, Namespace: r.Namespace})
		}
	}
	return out
}
