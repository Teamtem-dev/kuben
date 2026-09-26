package render_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/jsonx"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/render"
	"github.com/Teamtem-dev/kuben/packages/api/v1alpha1"
)

// The tests of controller::resources for the builder half the renderer
// carries (the environment objects, demand and job_from_cron follow with
// the controllers).

func owner() render.OwnerReference {
	return render.OwnerReference{
		APIVersion: "kuben.dev/v1alpha1", Kind: "App", Name: "api", UID: "uid-1",
		Controller: true, BlockOwnerDeletion: true,
	}
}

func resourcesApp(t *testing.T) *v1alpha1.App {
	t.Helper()
	return app(t, `{
		"source": { "image": "ghcr.io/acme/api:1.2.3" },
		"runtime": { "processes": {
			"web": { "port": 3000, "size": "small", "replicas": { "min": 2, "max": 2 } },
			"worker": { "command": ["bin/worker"], "size": "nano" }
		}, "healthCheck": { "path": "/healthz" } },
		"env": [
			{ "name": "LOG_LEVEL", "value": "info" },
			{ "name": "DATABASE_URL", "fromSecret": { "name": "db", "key": "url" } }
		],
		"domains": [{ "host": "API.acme.com" }]
	}`)
}

func platform(t *testing.T) render.Platform {
	t.Helper()
	return render.PlatformFromSpec(configSpec(t))
}

func build(t *testing.T, a *v1alpha1.App, p render.Platform) render.Desired {
	t.Helper()
	d, err := render.Build(a, p, owner())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return d
}

// at walks JSON maps (by key) and arrays (by index).
func at(t *testing.T, v any, path ...string) any {
	t.Helper()
	for _, step := range path {
		switch x := v.(type) {
		case map[string]any:
			v = x[step]
		case []any:
			i, err := strconv.Atoi(step)
			if err != nil || i >= len(x) {
				t.Fatalf("no %s in %v", step, x)
			}
			v = x[i]
		default:
			t.Fatalf("cannot step %q into %v", step, v)
		}
	}
	return v
}

func named(t *testing.T, objects []map[string]any, name string) map[string]any {
	t.Helper()
	for _, o := range objects {
		if at(t, o, "metadata", "name") == name {
			return o
		}
	}
	t.Fatalf("no object %s", name)
	return nil
}

func reason(err error) render.BuildReason {
	var b *render.BuildError
	if errors.As(err, &b) {
		return b.Reason
	}
	return ""
}

func TestDeploymentsAreHardenedAndResourced(t *testing.T) {
	d := build(t, resourcesApp(t), render.DefaultPlatform())
	if len(d.Deployments) != 2 {
		t.Fatalf("deployments: %d", len(d.Deployments))
	}
	web := named(t, d.Deployments, "api-web")
	spec := at(t, web, "spec")
	pod := at(t, spec, "template", "spec")
	c := at(t, pod, "containers", "0")
	checks := []struct {
		got, want any
		what      string
	}{
		{at(t, spec, "replicas"), int64(2), "replicas"},
		{at(t, pod, "automountServiceAccountToken"), false, "no token"},
		{at(t, c, "image"), "ghcr.io/acme/api:1.2.3", "image"},
		{at(t, c, "securityContext", "allowPrivilegeEscalation"), false, "no escalation"},
		{at(t, c, "env", "2"), map[string]any{"name": "PORT", "value": "3000"}, "PORT"},
		{at(t, c, "env", "1"), map[string]any{"name": "DATABASE_URL", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "db", "key": "url"}}}, "secret values are never inlined"},
		{at(t, c, "readinessProbe", "httpGet", "path"), "/healthz", "http probe"},
		{at(t, c, "startupProbe", "failureThreshold"), int64(60), "slow boots get 5 minutes"},
		{at(t, c, "livenessProbe") != nil, true, "health path → liveness"},
		{at(t, spec, "progressDeadlineSeconds"), int64(600), "deadline"},
		{at(t, spec, "strategy", "type"), "RollingUpdate", "strategy"},
		{at(t, c, "resources", "requests", "memory"), "128Mi", "requests"},
		{at(t, web, "metadata", "ownerReferences", "0", "uid"), "uid-1", "owner"},
	}
	for _, check := range checks {
		if diff := cmp.Diff(check.want, check.got); diff != "" {
			t.Errorf("%s (-want +got):\n%s", check.what, diff)
		}
	}
	wc := at(t, named(t, d.Deployments, "api-worker"), "spec", "template", "spec", "containers", "0").(map[string]any)
	if _, ports := wc["ports"]; ports {
		t.Error("a worker exposes no port")
	}
	if _, probe := wc["readinessProbe"]; probe {
		t.Error("a worker has no probe")
	}
	if diff := cmp.Diff([]any{"bin/worker"}, wc["command"]); diff != "" {
		t.Errorf("command: %s", diff)
	}
}

func TestAutoscaledProcessLeavesReplicasToTheHPA(t *testing.T) {
	d := build(t, app(t, `{
		"source": { "image": "nginx:1.27" },
		"runtime": { "processes": { "web": { "port": 80, "replicas": { "min": 2, "max": 6 } } } }
	}`), render.DefaultPlatform())
	if _, ok := at(t, d.Deployments[0], "spec").(map[string]any)["replicas"]; ok {
		t.Fatal("the HPA owns replicas")
	}
	spec := at(t, d.Autoscalers[0], "spec")
	if at(t, spec, "minReplicas") != int64(2) || at(t, spec, "maxReplicas") != int64(6) {
		t.Fatalf("hpa: %v", spec)
	}
}

func TestRestartAnnotationReachesThePodTemplate(t *testing.T) {
	a := resourcesApp(t)
	a.Annotations = map[string]string{render.RestartedAt: "2026-09-11T00:00:00Z"}
	d := build(t, a, render.DefaultPlatform())
	if got := at(t, d.Deployments[0], "spec", "template", "metadata", "annotations", render.RestartedAt); got != "2026-09-11T00:00:00Z" {
		t.Fatalf("annotation: %v", got)
	}
}

func TestInvalidAppsAreRejectedWithAReason(t *testing.T) {
	cases := []struct {
		spec string
		want render.BuildReason
	}{
		{`{ "source": { "image": "x" }, "runtime": { "processes": { "a": { "port": 1 }, "b": { "port": 2 } } } }`, render.ReasonMultiplePorts},
		{`{ "source": { "git": { "repo": "https://github.com/acme/api" } }, "runtime": { "processes": { "web": { "port": 8080 } } } }`, render.ReasonAwaitingBuild},
		{`{ "source": { "image": "x" }, "runtime": { "processes": { "web": { "size": "galactic" } } } }`, render.ReasonUnknownSize},
	}
	for _, c := range cases {
		_, err := render.Build(app(t, c.spec), render.DefaultPlatform(), owner())
		if got := reason(err); got != c.want {
			t.Errorf("%s: got %q (%v), want %q", c.spec, got, err, c.want)
		}
	}
	_, err := render.Build(app(t, cases[0].spec), render.DefaultPlatform(), owner())
	if want := "only one process may expose a port, found: a, b"; err == nil || err.Error() != want {
		t.Errorf("message: %v", err)
	}
}

func TestServiceAndRouteFollowTheWebProcess(t *testing.T) {
	a := resourcesApp(t)
	d := build(t, a, platform(t))
	svc, ok := d.Service.Get()
	if !ok {
		t.Fatal("service")
	}
	port := at(t, svc, "spec", "ports", "0")
	if at(t, port, "port") != int64(80) || at(t, port, "targetPort") != int64(3000) {
		t.Fatalf("port: %v", port)
	}
	p := platform(t)
	if diff := cmp.Diff([]string{"api.acme.com", "api-shop-prod.apps.example.com"}, render.Hostnames(a, p)); diff != "" {
		t.Fatal(diff)
	}
	if got := render.URL(a, p).Or(""); got != "https://api.acme.com" {
		t.Fatalf("url: %s", got)
	}
	route, ok := d.Route.Get()
	if !ok || at(t, route, "spec", "parentRefs", "0", "namespace") != "kuben-system" ||
		at(t, route, "spec", "rules", "0", "backendRefs", "0", "port") != int64(80) {
		t.Fatalf("route: %v", route)
	}
	if build(t, a, render.DefaultPlatform()).Route.IsSome() {
		t.Fatal("no gateway → no route")
	}
}

func TestVolumesAreRetainedMountedAndForceRecreate(t *testing.T) {
	d := build(t, app(t, `{
		"source": { "image": "postgres:17-alpine" },
		"runtime": { "processes": { "db": { "port": 5432, "protocol": "tcp" } } },
		"volumes": [{ "name": "data", "mountPath": "/var/lib/postgresql/data", "size": "5Gi" }]
	}`), render.DefaultPlatform())
	if len(d.Volumes) != 1 {
		t.Fatalf("pvcs: %d", len(d.Volumes))
	}
	pvc := d.Volumes[0]
	meta := at(t, pvc, "metadata").(map[string]any)
	if meta["name"] != "api-data" || at(t, meta, "annotations", render.Retain) != "true" {
		t.Fatalf("pvc: %v", meta)
	}
	if _, ok := meta["ownerReferences"]; ok {
		t.Fatal("data outlives the app")
	}
	if at(t, pvc, "spec", "resources", "requests", "storage") != "5Gi" {
		t.Fatal("size")
	}
	spec := at(t, d.Deployments[0], "spec")
	if at(t, spec, "strategy", "type") != "Recreate" {
		t.Fatal("Recreate")
	}
	pod := at(t, spec, "template", "spec")
	if at(t, pod, "volumes", "0", "persistentVolumeClaim", "claimName") != "api-data" ||
		at(t, pod, "containers", "0", "volumeMounts", "0", "mountPath") != "/var/lib/postgresql/data" {
		t.Fatalf("pod: %v", pod)
	}
	if at(t, pod, "containers", "0", "livenessProbe") != nil {
		t.Fatal("no health path → no liveness")
	}
	scaled := app(t, `{
		"source": { "image": "x" },
		"runtime": { "processes": { "web": { "port": 80, "replicas": { "min": 1, "max": 3 } } } },
		"volumes": [{ "name": "data", "mountPath": "/data" }]
	}`)
	if got := reason(render.Validate(scaled)); got != render.ReasonVolumeNeedsSingleReplica {
		t.Fatalf("got %q", got)
	}
}

func TestTCPProcessesGetARealPortAndNoRoute(t *testing.T) {
	d := build(t, app(t, `{
		"source": { "image": "redis:7-alpine" },
		"runtime": { "processes": { "redis": { "port": 6379, "protocol": "tcp" } } }
	}`), platform(t))
	svc, _ := d.Service.Get()
	port := at(t, svc, "spec", "ports", "0").(map[string]any)
	if port["port"] != int64(6379) || port["name"] != "tcp" {
		t.Fatalf("port: %v", port)
	}
	if _, ok := port["appProtocol"]; ok {
		t.Fatal("no app protocol")
	}
	if d.Route.IsSome() {
		t.Fatal("tcp is never public")
	}
}

func TestScheduledProcessesBecomeCronJobs(t *testing.T) {
	d := build(t, app(t, `{
		"source": { "image": "ghcr.io/acme/api:1.2.3" },
		"runtime": { "processes": {
			"web": { "port": 3000 },
			"report": { "command": ["bin/report"], "schedule": "0 3 * * *", "timeZone": "Europe/Berlin" }
		} }
	}`), render.DefaultPlatform())
	if len(d.Deployments) != 1 {
		t.Fatal("scheduled processes get no Deployment")
	}
	cron := d.CronJobs[0]
	spec := at(t, cron, "spec")
	if at(t, cron, "metadata", "name") != "api-report" || at(t, spec, "schedule") != "0 3 * * *" ||
		at(t, spec, "timeZone") != "Europe/Berlin" || at(t, spec, "concurrencyPolicy") != "Forbid" {
		t.Fatalf("cron: %v", cron)
	}
	pod := at(t, spec, "jobTemplate", "spec", "template", "spec")
	if at(t, pod, "restartPolicy") != "Never" || at(t, pod, "containers", "0", "readinessProbe") != nil {
		t.Fatalf("pod: %v", pod)
	}
	for _, ok := range []string{"*/5 * * * *", "@daily"} {
		if !render.ValidSchedule(ok) {
			t.Errorf("%q is valid", ok)
		}
	}
	for _, bad := range []string{"every day", "* * * *", "@often"} {
		if render.ValidSchedule(bad) {
			t.Errorf("%q is not valid", bad)
		}
	}
	err := render.Validate(app(t, `{
		"source": { "image": "x" },
		"runtime": { "processes": { "job": { "port": 80, "schedule": "@hourly" } } }
	}`))
	if err == nil {
		t.Fatal("a scheduled process with a port is refused")
	}
	if reason(err) != render.ReasonScheduledWithPort || err.Error() != "scheduled process `job` cannot expose a port" {
		t.Fatalf("got %v", err)
	}
}

func TestTLSRoutesAttachToHostListeners(t *testing.T) {
	route, _ := build(t, resourcesApp(t), platform(t)).Route.Get()
	var sections []string
	for _, r := range at(t, route, "spec", "parentRefs").([]any) {
		sections = append(sections, at(t, r, "sectionName").(string))
	}
	want := []string{render.HostListenerName("api.acme.com"), render.HostListenerName("api-shop-prod.apps.example.com")}
	slices.Sort(want)
	if diff := cmp.Diff(want, sections); diff != "" {
		t.Fatal(diff)
	}
}

func TestPlatformDefaultsIncludeSizePresets(t *testing.T) {
	p := render.DefaultPlatform()
	if _, ok := p.Size("small"); !ok {
		t.Fatal("small")
	}
	if p.Gateway.IsSome() || p.TLS {
		t.Fatal("no gateway, no TLS")
	}
}

func TestDomainsCarryTheirTLSModeIntoTheRouteAndAGrant(t *testing.T) {
	a := app(t, `{
		"source": { "image": "ghcr.io/acme/api@sha256:1111111111111111111111111111111111111111111111111111111111111111" },
		"runtime": { "processes": { "web": { "port": 3000 } } },
		"domains": [
			{ "host": "Shop.acme.com" },
			{ "host": "legacy.acme.com", "tls": "none" },
			{ "host": "own.acme.com", "tls": "acme-cert" }
		]
	}`)
	if err := render.Validate(a); err != nil {
		t.Fatal(err)
	}
	p := platform(t)
	claims := render.DomainClaims(a, p)
	want := []render.DomainClaim{
		{Host: "shop.acme.com", TLS: "auto"},
		{Host: "legacy.acme.com", TLS: "none"},
		{Host: "own.acme.com", TLS: "acme-cert"},
		{Host: "api-shop-prod.apps.example.com", TLS: "auto"},
	}
	if diff := cmp.Diff(want, claims); diff != "" {
		t.Fatal(diff)
	}
	if got := render.URL(a, p).Or(""); got != "https://shop.acme.com" {
		t.Fatal(got)
	}
	d := build(t, a, p)
	route, _ := d.Route.Get()
	var fromAnnotation []render.DomainClaim
	if err := json.Unmarshal([]byte(at(t, route, "metadata", "annotations", render.DomainsAnnotation).(string)), &fromAnnotation); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(claims, fromAnnotation); diff != "" {
		t.Fatal(diff)
	}
	sections := map[string]bool{}
	for _, r := range at(t, route, "spec", "parentRefs").([]any) {
		sections[at(t, r, "sectionName").(string)] = true
	}
	if !sections[render.PlainListenerName("legacy.acme.com")] || !sections[render.HostListenerName("own.acme.com")] || len(sections) != 4 {
		t.Fatalf("sections: %v", sections)
	}
	grant, ok := d.Grant.Get()
	if !ok || at(t, grant, "kind") != "ReferenceGrant" || at(t, grant, "metadata", "name") != "api-tls" ||
		at(t, grant, "spec", "from", "0", "namespace") != "kuben-system" {
		t.Fatalf("grant: %v", grant)
	}
	if diff := cmp.Diff([]any{map[string]any{"group": "", "kind": "Secret", "name": "acme-cert"}}, at(t, grant, "spec", "to")); diff != "" {
		t.Fatal(diff)
	}

	// TLS unavailable: no grant, no listener names, plain URLs.
	off := platform(t)
	off.TLS = false
	d = build(t, a, off)
	if d.Grant.IsSome() {
		t.Fatal("no grant without TLS")
	}
	plain, _ := d.Route.Get()
	if _, ok := at(t, plain, "spec", "parentRefs", "0").(map[string]any)["sectionName"]; ok {
		t.Fatal("no listener names without TLS")
	}
	if got := render.URL(a, off).Or(""); got != "http://shop.acme.com" {
		t.Fatal(got)
	}
}

func TestADomainTLSThatIsNoSecretNameIsRefused(t *testing.T) {
	for text, want := range map[string]render.TLSMode{
		"":           {Kind: render.TLSAuto},
		"none":       {Kind: render.TLSPlain},
		"certs.acme": {Kind: render.TLSSecret, Secret: "certs.acme"},
	} {
		if got, ok := render.ParseTLSMode(text); !ok || got != want {
			t.Errorf("%q: got %v %v", text, got, ok)
		}
	}
	for _, bad := range []string{"Bad_Name", "-x", "a..b", "UPPER"} {
		if _, ok := render.ParseTLSMode(bad); ok {
			t.Errorf("%q is refused", bad)
		}
	}
	err := render.Validate(app(t, `{
		"source": { "image": "nginx@sha256:1111111111111111111111111111111111111111111111111111111111111111" },
		"runtime": { "processes": { "web": { "port": 80 } } },
		"domains": [{ "host": "a.acme.com", "tls": "Bad_Name" }]
	}`))
	if reason(err) != render.ReasonInvalidDomainTLS {
		t.Fatalf("got %v", err)
	}
}

// The listener and secret names are stored in clusters: pinned.
func TestListenerNamesArePinned(t *testing.T) {
	if got := render.HostListenerName("api.acme.com"); got != "h-20550d4a0e0f" {
		t.Fatal(got)
	}
	if got := render.HostListenerName("api-shop-prod.apps.example.com"); got != "h-7ddbda499ebe" {
		t.Fatal(got)
	}
	if render.HostSecretName("x")[len(render.TLSSecretPrefix):] != render.HostListenerName("x")[2:] {
		t.Fatal("one hash for the listener and its certificate")
	}
}

// The capability snapshot is stored with every plan (canonical JSON), and
// a size preset's cpuLimit is written as null, as serde did.
func TestTheCapabilitySnapshotJSONIsTheRustOne(t *testing.T) {
	caps := capabilities(t)
	text, err := jsonx.CanonicalValue(caps)
	if err != nil {
		t.Fatal(err)
	}
	preset := func(name, cpu, mem, limit string) string {
		return `{"cpuLimit":null,"cpuRequest":"` + cpu + `","memoryLimit":"` + limit + `","memoryRequest":"` + mem + `","name":"` + name + `"}`
	}
	want := `{"baseDomain":"apps.example.com","clusterIssuer":"letsencrypt","gateway":"kuben-system/kuben","sizes":[` +
		preset("nano", "50m", "64Mi", "128Mi") + "," + preset("small", "100m", "128Mi", "256Mi") + "," +
		preset("medium", "250m", "512Mi", "1Gi") + "," + preset("large", "1", "2Gi", "4Gi") + `]}`
	if text != want {
		t.Fatalf("got  %s\nwant %s", text, want)
	}
	caps.Gateway = opt.None[string]()
	caps.Sizes = nil
	if text, _ := jsonx.CanonicalValue(caps); text != `{"baseDomain":"apps.example.com","clusterIssuer":"letsencrypt","sizes":[]}` {
		t.Fatal(text)
	}
}
