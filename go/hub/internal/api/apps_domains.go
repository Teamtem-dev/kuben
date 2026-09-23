package api

// DNS check of an app's hostnames against the gateway (scenario 9,
// routes/apps/domains.rs).

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/gen"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/perm"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/controller"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/registry"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/render"
	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

// appHosts returns the public hostnames of the app: explicit domains first,
// then the generated one (if base domain and environment are set).
func appHosts(a appScope, p render.Platform) []string {
	var domains []string
	if spec, ok := desiredSpec(a.app); ok {
		for _, d := range spec.Domains {
			domains = append(domains, d.Host)
		}
	}
	return render.HostnamesFor(a.app.Slug, opt.Some(a.env.resourceName()), domains, p)
}

// dnsVerdict compares the host's resolved addresses with the gateway's expected addresses:
// ok, mismatch, unresolved, or unknown.
func dnsVerdict(resolved, expected []string) (string, string) {
	if len(resolved) == 0 {
		return "unresolved", "no DNS record: create an A/AAAA record (or CNAME) pointing at the gateway"
	}
	if len(expected) == 0 {
		return "unknown", fmt.Sprintf("resolves to %s; the gateway reports no address to compare with", strings.Join(resolved, ", "))
	}
	for _, ip := range resolved {
		if slices.Contains(expected, ip) {
			return "ok", "points at the gateway"
		}
	}
	return "mismatch", fmt.Sprintf("points at %s but the gateway is %s", strings.Join(resolved, ", "), strings.Join(expected, ", "))
}

// CheckAppDomains checks that every hostname of the app points at the gateway.
func (s *Server) CheckAppDomains(ctx context.Context, params gen.CheckAppDomainsParams) (gen.CheckAppDomainsRes, error) {
	acc, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	a, err := s.findApp(ctx, acc, params.Project, params.Environment, params.App)
	if err != nil {
		return nil, err
	}
	if _, err := acc.Require(perm.AppRead, a.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerr already
	}
	cluster, err := s.cluster()
	if err != nil {
		return nil, err
	}

	platform := s.readPlatform(ctx, cluster)
	expected := s.readGatewayAddresses(ctx, cluster, platform)
	hosts := appHosts(a, platform)

	checks := make([]gen.DomainCheck, len(hosts))
	for i, host := range hosts {
		resolvedRaw, _ := s.deps.Resolver.LookupHost(ctx, host)
		var resolved []string
		for _, r := range resolvedRaw {
			if ip := net.ParseIP(r); ip != nil {
				resolved = append(resolved, ip.String())
			}
		}
		slices.Sort(resolved)
		resolved = slices.Compact(resolved)

		status, message := dnsVerdict(resolved, expected)
		if resolved == nil {
			resolved = []string{}
		}
		expectedCopy := slices.Clone(expected)
		if expectedCopy == nil {
			expectedCopy = []string{}
		}
		checks[i] = gen.DomainCheck{
			Addresses: resolved,
			Expected:  expectedCopy,
			Host:      host,
			Message:   message,
			Status:    status,
		}
	}

	res := gen.CheckAppDomainsOKApplicationJSON(checks)
	return &res, nil
}

func (s *Server) readPlatform(ctx context.Context, cluster registry.Cluster) render.Platform {
	gvr := v1alpha1.SchemeGroupVersion.WithResource("kubenconfigs")
	obj, err := cluster.Dynamic.Resource(gvr).Get(ctx, controller.KubenConfigName, metav1.GetOptions{})
	if err != nil {
		return render.DefaultPlatform()
	}
	spec, ok := obj.Object["spec"].(map[string]any)
	if !ok {
		return render.DefaultPlatform()
	}
	data, err := json.Marshal(spec)
	if err != nil {
		return render.DefaultPlatform()
	}
	var configSpec v1alpha1.KubenConfigSpec
	if err := json.Unmarshal(data, &configSpec); err != nil {
		return render.DefaultPlatform()
	}
	return render.PlatformFromSpec(configSpec)
}

func (s *Server) readGatewayAddresses(ctx context.Context, cluster registry.Cluster, platform render.Platform) []string {
	gw, ok := platform.Gateway.Get()
	if !ok {
		return []string{}
	}
	gvr := schema.GroupVersionResource{
		Group:    "gateway.networking.k8s.io",
		Version:  "v1",
		Resource: "gateways",
	}
	gwObj, err := cluster.Dynamic.Resource(gvr).Namespace(gw.Namespace).Get(ctx, gw.Name, metav1.GetOptions{})
	if err != nil {
		return []string{}
	}
	status, ok := gwObj.Object["status"].(map[string]any)
	if !ok {
		return []string{}
	}
	rawAddrs, ok := status["addresses"].([]any)
	if !ok {
		return []string{}
	}
	var expected []string
	for _, a := range rawAddrs {
		am, ok := a.(map[string]any)
		if !ok {
			continue
		}
		val, ok := am["value"].(string)
		if !ok || val == "" {
			continue
		}
		if ip := net.ParseIP(val); ip != nil {
			expected = append(expected, ip.String())
		} else {
			resolved, _ := s.deps.Resolver.LookupHost(ctx, val)
			for _, r := range resolved {
				if ip := net.ParseIP(r); ip != nil {
					expected = append(expected, ip.String())
				}
			}
		}
	}
	slices.Sort(expected)
	return slices.Compact(expected)
}
