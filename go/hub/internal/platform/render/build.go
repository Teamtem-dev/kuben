package render

import (
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/wire"
	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

// The builder (controller::resources, the App half): an App's spec becomes
// the Kubernetes objects that run it. Objects are JSON maps shaped exactly
// as k8s-openapi serialized its types (unset fields absent), because their
// canonical text is hashed and stored.

// Names and annotations the builder writes.
const (
	// RestartedAt is the pod-template annotation bumped by "restart" to
	// roll pods without a spec change.
	RestartedAt = "kuben.dev/restarted-at"
	// Retain on a PVC: retained when the App is deleted (no
	// ownerReference).
	Retain = "kuben.dev/retain"
	// ServicePort is the port every app Service listens on; the gateway
	// routes here.
	ServicePort = 80
	// DomainsAnnotation on an app's HTTPRoute: its domains and how each is
	// secured (M2.3), read by the gateway controller and the exposure view
	// whichever path (App controller or agent) wrote the route.
	DomainsAnnotation = "kuben.dev/domains"
	// GrantSuffix is the name suffix of the ReferenceGrant that lets the
	// Gateway read an app's own certificate Secrets.
	GrantSuffix = "-tls"
)

// object is a Kubernetes object (or a part of one) as JSON.
type object = map[string]any

// OwnerReference is the owner the builder puts on the children of an App.
type OwnerReference struct {
	APIVersion         string
	Kind               string
	Name               string
	UID                string
	Controller         bool
	BlockOwnerDeletion bool
}

func (o OwnerReference) json() object {
	return object{
		"apiVersion":         o.APIVersion,
		"kind":               o.Kind,
		"name":               o.Name,
		"uid":                o.UID,
		"controller":         o.Controller,
		"blockOwnerDeletion": o.BlockOwnerDeletion,
	}
}

// Desired is everything an App's spec makes, before it is applied. The
// renderer freezes the same objects into a RenderPlan.
type Desired struct {
	Deployments []object
	Autoscalers []object
	CronJobs    []object
	Volumes     []object
	Service     opt.Val[object]
	Route       opt.Val[object]
	// Grant lets the Gateway read the app's own certificate Secrets.
	Grant opt.Val[object]
	// ExposesHTTP: the web process is HTTP and should be reachable through
	// the gateway.
	ExposesHTTP bool
}

// Build is every object app makes on p (controller::app::build).
func Build(app *v1alpha1.App, p Platform, owner OwnerReference) (Desired, error) {
	if err := Validate(app); err != nil {
		return Desired{}, err
	}
	deployments, err := deployments(app, p, owner)
	if err != nil {
		return Desired{}, err
	}
	crons, err := cronJobs(app, p, owner)
	if err != nil {
		return Desired{}, err
	}
	service, err := service(app, owner)
	if err != nil {
		return Desired{}, err
	}
	route, err := httpRoute(app, p, owner)
	if err != nil {
		return Desired{}, err
	}
	grant, err := referenceGrant(app, p, owner)
	if err != nil {
		return Desired{}, err
	}
	exposes, err := routesHTTP(app)
	if err != nil {
		return Desired{}, err
	}
	return Desired{
		Deployments: deployments,
		Autoscalers: autoscalers(app, owner),
		CronJobs:    crons,
		Volumes:     persistentVolumeClaims(app),
		Service:     service,
		Route:       route,
		Grant:       grant,
		ExposesHTTP: exposes,
	}, nil
}

// namedProcess is a process with its name; processes are visited in name
// order, as the Rust BTreeMap iterated them.
type namedProcess struct {
	name    string
	process v1alpha1.Process
}

func processes(app *v1alpha1.App) []namedProcess {
	names := make([]string, 0, len(app.Spec.Runtime.Processes))
	for name := range app.Spec.Runtime.Processes {
		names = append(names, name)
	}
	slices.Sort(names)
	out := make([]namedProcess, 0, len(names))
	for _, name := range names {
		out = append(out, namedProcess{name, app.Spec.Runtime.Processes[name]})
	}
	return out
}

// longRunning is every process without a schedule.
func longRunning(app *v1alpha1.App) []namedProcess {
	var out []namedProcess
	for _, np := range processes(app) {
		if np.process.Schedule == nil {
			out = append(out, np)
		}
	}
	return out
}

// webProcess is the single long-running process that exposes a port (the
// one routed to), if any.
func webProcess(app *v1alpha1.App) (opt.Val[namedProcess], error) {
	var exposed []namedProcess
	for _, np := range longRunning(app) {
		if np.process.Port != nil {
			exposed = append(exposed, np)
		}
	}
	switch len(exposed) {
	case 0:
		return opt.None[namedProcess](), nil
	case 1:
		return opt.Some(exposed[0]), nil
	}
	names := make([]string, 0, len(exposed))
	for _, np := range exposed {
		names = append(names, np.name)
	}
	return opt.None[namedProcess](), errMultiplePorts(names)
}

// Image is the image to run. Git sources have none until a build produced
// one.
func Image(app *v1alpha1.App) (string, error) {
	src := app.Spec.Source
	switch {
	case src.Image != nil && src.Git == nil:
		return *src.Image, nil
	case src.Image == nil && src.Git != nil:
		return "", errAwaitingBuild()
	}
	return "", errInvalidSource()
}

// ValidSchedule is a syntax check for a CronJob schedule: five fields or an
// `@hourly`-style macro.
func ValidSchedule(expr string) bool {
	expr = strings.TrimSpace(expr)
	if strings.HasPrefix(expr, "@") {
		switch expr {
		case "@yearly", "@annually", "@monthly", "@weekly", "@daily", "@midnight", "@hourly":
			return true
		}
		return false
	}
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return false
	}
	for _, f := range fields {
		for _, c := range f {
			ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
				c == '*' || c == '/' || c == ',' || c == '-' || c == '?'
			if !ok {
				return false
			}
		}
	}
	return true
}

// Validate checks the cross-field rules the CRD schema cannot express.
// Every builder calls it first, so an invalid App never produces a
// half-applied set of objects.
func Validate(app *v1alpha1.App) error {
	all := processes(app)
	if len(all) == 0 {
		return errNoProcesses()
	}
	for _, np := range all {
		if schedule := np.process.Schedule; schedule != nil {
			if np.process.Port != nil {
				return errScheduledWithPort(np.name)
			}
			if !ValidSchedule(*schedule) {
				return errInvalidSchedule(np.name, *schedule)
			}
		}
	}
	for _, d := range app.Spec.Domains {
		if _, ok := ParseTLSMode(d.TLS); !ok {
			return errInvalidDomainTLS(d.Host, d.TLS)
		}
	}
	if len(app.Spec.Volumes) > 0 {
		single := len(all) == 1 && all[0].process.Replicas.Max <= 1
		if !single {
			return errVolumeNeedsSingleReplica()
		}
	}
	if _, err := webProcess(app); err != nil {
		return err
	}
	if _, err := Image(app); err != nil {
		return err
	}
	return nil
}

// strings of a map, as JSON.
func stringMap(m map[string]string) object {
	out := make(object, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func managedLabels() map[string]string {
	return map[string]string{v1alpha1.LabelManagedBy: v1alpha1.LabelManagerValue}
}

// AppLabels are the labels shared by everything an App owns (propagated
// org/project/environment).
func AppLabels(app *v1alpha1.App, process opt.Val[string]) map[string]string {
	l := managedLabels()
	for _, key := range []string{v1alpha1.LabelOrg, v1alpha1.LabelProject, v1alpha1.LabelEnvironment} {
		if v, ok := app.Labels[key]; ok {
			l[key] = v
		}
	}
	l[v1alpha1.LabelApp] = app.Name
	if p, ok := process.Get(); ok {
		l[v1alpha1.LabelProcess] = p
	}
	return l
}

func selector(app *v1alpha1.App, process string) object {
	return object{v1alpha1.LabelApp: app.Name, v1alpha1.LabelProcess: process}
}

// WorkloadName is the name of a process's workload: `<app>-<process>`.
func WorkloadName(app, process string) string { return app + "-" + process }

// PVCName is the name of an app volume's claim: `<app>-<volume>`.
func PVCName(app, volume string) string { return app + "-" + volume }

// metadata is the metadata of a child object: name, the app's namespace
// when it has one, labels, and optional annotations and owner.
func metadata(app *v1alpha1.App, name string, labels map[string]string) object {
	m := object{"name": name, "labels": stringMap(labels)}
	if app.Namespace != "" {
		m["namespace"] = app.Namespace
	}
	return m
}

func owned(m object, owner OwnerReference) object {
	m["ownerReferences"] = []any{owner.json()}
	return m
}

func protocolName(p v1alpha1.Protocol) string {
	if p.IsHTTP() {
		return "http"
	}
	return "tcp"
}

func envVars(app *v1alpha1.App, port opt.Val[uint16]) []any {
	vars := make([]any, 0, len(app.Spec.Env)+1)
	hasPort := false
	for _, e := range app.Spec.Env {
		v := object{"name": e.Name}
		ref := e.FromSecret
		if ref == nil {
			ref = e.FromService
		}
		if ref == nil && e.Value != nil {
			v["value"] = *e.Value
		}
		// Service bindings are published as Secrets named after the service.
		if ref != nil {
			v["valueFrom"] = object{"secretKeyRef": object{"name": ref.Name, "key": ref.Key}}
		}
		hasPort = hasPort || e.Name == "PORT"
		vars = append(vars, v)
	}
	// Heroku/buildpack convention: tell the process which port to bind.
	if p, ok := port.Get(); ok && !hasPort {
		vars = append(vars, object{"name": "PORT", "value": strconv.FormatUint(uint64(p), 10)})
	}
	return vars
}

func resources(preset v1alpha1.SizePreset) object {
	limits := object{"memory": preset.MemoryLimit}
	if preset.CPULimit != nil {
		limits["cpu"] = *preset.CPULimit
	}
	return object{
		"requests": object{"cpu": preset.CPURequest, "memory": preset.MemoryRequest},
		"limits":   limits,
	}
}

// probes is (readiness, startup, liveness). Readiness gates traffic;
// startup gives slow boots up to 5 minutes before the other probes start;
// liveness only runs with an explicit health path (a TCP liveness check
// would restart apps that are merely busy).
func probes(app *v1alpha1.App, port uint16) (readiness, startup object, liveness opt.Val[object]) {
	health := app.Spec.Runtime.HealthCheck
	base := func() object {
		p := object{"timeoutSeconds": int64(3)}
		if health != nil {
			target := port
			if health.Port != nil {
				target = *health.Port
			}
			p["httpGet"] = object{"path": health.Path, "port": int64(target)}
		} else {
			p["tcpSocket"] = object{"port": int64(port)}
		}
		return p
	}
	readiness = base()
	readiness["periodSeconds"] = int64(10)
	readiness["failureThreshold"] = int64(3)
	startup = base()
	startup["periodSeconds"] = int64(5)
	startup["failureThreshold"] = int64(60)
	if health != nil {
		l := base()
		l["periodSeconds"] = int64(20)
		l["timeoutSeconds"] = int64(5)
		l["failureThreshold"] = int64(3)
		liveness = opt.Some(l)
	}
	return readiness, startup, liveness
}

func hardened() object {
	return object{
		"allowPrivilegeEscalation": false,
		"capabilities":             object{"drop": []any{"NET_RAW"}},
		"seccompProfile":           object{"type": "RuntimeDefault"},
	}
}

func stringList(items []string) []any {
	out := make([]any, 0, len(items))
	for _, s := range items {
		out = append(out, s)
	}
	return out
}

// container is the one container of a process. serve: long-running (gets
// probes).
func container(app *v1alpha1.App, np namedProcess, image string, preset v1alpha1.SizePreset, serve bool) object {
	p := np.process
	port := opt.FromPtr(p.Port)
	c := object{
		"name":            np.name,
		"image":           image,
		"env":             envVars(app, port),
		"resources":       resources(preset),
		"securityContext": hardened(),
	}
	if len(p.Command) > 0 {
		c["command"] = stringList(p.Command)
	}
	if n, ok := port.Get(); ok {
		c["ports"] = []any{object{"name": protocolName(p.Protocol), "containerPort": int64(n), "protocol": "TCP"}}
		if serve {
			readiness, startup, liveness := probes(app, n)
			c["readinessProbe"] = readiness
			c["startupProbe"] = startup
			if l, ok := liveness.Get(); ok {
				c["livenessProbe"] = l
			}
		}
	}
	if len(app.Spec.Volumes) > 0 {
		mounts := make([]any, 0, len(app.Spec.Volumes))
		for _, v := range app.Spec.Volumes {
			mounts = append(mounts, object{"name": v.Name, "mountPath": v.MountPath})
		}
		c["volumeMounts"] = mounts
	}
	return c
}

func podSpec(app *v1alpha1.App, c object, restartPolicy opt.Val[string]) object {
	security := object{"seccompProfile": object{"type": "RuntimeDefault"}}
	if g := app.Spec.Runtime.FSGroup; g != nil {
		security["fsGroup"] = *g
		security["fsGroupChangePolicy"] = "OnRootMismatch"
	}
	spec := object{
		"containers":                    []any{c},
		"automountServiceAccountToken":  false,
		"enableServiceLinks":            false,
		"securityContext":               security,
		"terminationGracePeriodSeconds": int64(30),
	}
	if len(app.Spec.Volumes) > 0 {
		volumes := make([]any, 0, len(app.Spec.Volumes))
		for _, v := range app.Spec.Volumes {
			volumes = append(volumes, object{
				"name":                  v.Name,
				"persistentVolumeClaim": object{"claimName": PVCName(app.Name, v.Name)},
			})
		}
		spec["volumes"] = volumes
	}
	if r, ok := restartPolicy.Get(); ok {
		spec["restartPolicy"] = r
	}
	if len(app.Spec.ImagePullSecrets) > 0 {
		refs := make([]any, 0, len(app.Spec.ImagePullSecrets))
		for _, name := range app.Spec.ImagePullSecrets {
			refs = append(refs, object{"name": name})
		}
		spec["imagePullSecrets"] = refs
	}
	return spec
}

func preset(p Platform, np namedProcess) (v1alpha1.SizePreset, error) {
	s, ok := p.Size(np.process.Size)
	if !ok {
		return v1alpha1.SizePreset{}, errUnknownSize(np.name, np.process.Size)
	}
	return s, nil
}

// autoscaled: autoscaling is on when max > min; then the HPA owns
// replicas.
func autoscaled(p v1alpha1.Process) bool {
	return p.Replicas.Max > p.Replicas.Min && p.Schedule == nil
}

func clampInt32(v uint32) int64 {
	return int64(min(v, math.MaxInt32))
}

// deployments is one Deployment per long-running process.
func deployments(app *v1alpha1.App, p Platform, owner OwnerReference) ([]object, error) {
	if err := Validate(app); err != nil {
		return nil, err
	}
	image, err := Image(app)
	if err != nil {
		return nil, err
	}
	restartedAt, restarted := app.Annotations[RestartedAt]
	// ReadWriteOnce volumes cannot be attached to the old and new pod at once.
	strategy := func() object {
		if len(app.Spec.Volumes) == 0 {
			return object{
				"type":          "RollingUpdate",
				"rollingUpdate": object{"maxSurge": int64(1), "maxUnavailable": int64(0)},
			}
		}
		return object{"type": "Recreate"}
	}
	var out []object
	for _, np := range longRunning(app) {
		size, err := preset(p, np)
		if err != nil {
			return nil, err
		}
		c := container(app, np, image, size, true)
		name := opt.Some(np.name)
		template := object{"labels": stringMap(AppLabels(app, name))}
		if restarted {
			template["annotations"] = object{RestartedAt: restartedAt}
		}
		spec := object{
			"revisionHistoryLimit":    int64(5),
			"progressDeadlineSeconds": int64(600),
			"selector":                object{"matchLabels": selector(app, np.name)},
			"strategy":                strategy(),
			"template":                object{"metadata": template, "spec": podSpec(app, c, opt.None[string]())},
		}
		if !autoscaled(np.process) {
			spec["replicas"] = clampInt32(np.process.Replicas.Min)
		}
		out = append(out, object{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata":   owned(metadata(app, WorkloadName(app.Name, np.name), AppLabels(app, name)), owner),
			"spec":       spec,
		})
	}
	return out, nil
}

// cronJobs is one CronJob per scheduled process (scenario 7). Overlapping
// runs are skipped (Forbid), a run that could not start within 5 minutes
// is dropped, and finished Jobs are garbage-collected after a day.
func cronJobs(app *v1alpha1.App, p Platform, owner OwnerReference) ([]object, error) {
	if err := Validate(app); err != nil {
		return nil, err
	}
	var scheduled []namedProcess
	for _, np := range processes(app) {
		if np.process.Schedule != nil {
			scheduled = append(scheduled, np)
		}
	}
	if len(scheduled) == 0 {
		return nil, nil
	}
	image, err := Image(app)
	if err != nil {
		return nil, err
	}
	out := make([]object, 0, len(scheduled))
	for _, np := range scheduled {
		size, err := preset(p, np)
		if err != nil {
			return nil, err
		}
		c := container(app, np, image, size, false)
		labels := func() object { return stringMap(AppLabels(app, opt.Some(np.name))) }
		spec := object{
			"schedule":                   opt.FromPtr(np.process.Schedule).Or(""),
			"concurrencyPolicy":          "Forbid",
			"startingDeadlineSeconds":    int64(300),
			"successfulJobsHistoryLimit": int64(3),
			"failedJobsHistoryLimit":     int64(3),
			"jobTemplate": object{
				"metadata": object{"labels": labels()},
				"spec": object{
					"backoffLimit":            int64(1),
					"activeDeadlineSeconds":   int64(3600),
					"ttlSecondsAfterFinished": int64(86_400),
					"template": object{
						"metadata": object{"labels": labels()},
						"spec":     podSpec(app, c, opt.Some("Never")),
					},
				},
			},
		}
		if tz := np.process.TimeZone; tz != nil {
			spec["timeZone"] = *tz
		}
		out = append(out, object{
			"apiVersion": "batch/v1",
			"kind":       "CronJob",
			"metadata":   owned(metadata(app, WorkloadName(app.Name, np.name), AppLabels(app, opt.Some(np.name))), owner),
			"spec":       spec,
		})
	}
	return out, nil
}

// persistentVolumeClaims are the PVCs of the app's volumes (scenario 6).
// Deliberately without an ownerReference: deleting an App never deletes
// its data; the API removes them only on an explicit delete_volumes=true.
func persistentVolumeClaims(app *v1alpha1.App) []object {
	out := make([]object, 0, len(app.Spec.Volumes))
	for _, v := range app.Spec.Volumes {
		meta := metadata(app, PVCName(app.Name, v.Name), AppLabels(app, opt.None[string]()))
		meta["annotations"] = object{Retain: "true"}
		spec := object{
			"accessModes": []any{"ReadWriteOnce"},
			"resources":   object{"requests": object{"storage": v.Size}},
		}
		if v.StorageClass != nil {
			spec["storageClassName"] = *v.StorageClass
		}
		out = append(out, object{
			"apiVersion": "v1",
			"kind":       "PersistentVolumeClaim",
			"metadata":   meta,
			"spec":       spec,
		})
	}
	return out
}

// autoscalers are the HPAs of autoscaled processes (target: 80 % CPU of
// the request).
func autoscalers(app *v1alpha1.App, owner OwnerReference) []object {
	var out []object
	for _, np := range processes(app) {
		if !autoscaled(np.process) {
			continue
		}
		name := WorkloadName(app.Name, np.name)
		out = append(out, object{
			"apiVersion": "autoscaling/v2",
			"kind":       "HorizontalPodAutoscaler",
			"metadata":   owned(metadata(app, name, AppLabels(app, opt.Some(np.name))), owner),
			"spec": object{
				"scaleTargetRef": object{"apiVersion": "apps/v1", "kind": "Deployment", "name": name},
				"minReplicas":    clampInt32(max(np.process.Replicas.Min, 1)),
				"maxReplicas":    clampInt32(np.process.Replicas.Max),
				"metrics": []any{object{
					"type": "Resource",
					"resource": object{
						"name":   "cpu",
						"target": object{"type": "Utilization", "averageUtilization": int64(80)},
					},
				}},
			},
		})
	}
	return out
}

// service is the ClusterIP Service in front of the web process. HTTP: :80
// → container port (the gateway routes here). TCP: the real port,
// cluster-internal only.
func service(app *v1alpha1.App, owner OwnerReference) (opt.Val[object], error) {
	web, err := webProcess(app)
	if err != nil {
		return opt.None[object](), err
	}
	np, ok := web.Get()
	if !ok {
		return opt.None[object](), nil
	}
	target := int64(8080)
	if np.process.Port != nil {
		target = int64(*np.process.Port)
	}
	http := np.process.Protocol.IsHTTP()
	port := object{"name": protocolName(np.process.Protocol), "port": target, "targetPort": target, "protocol": "TCP"}
	if http {
		port["port"] = int64(ServicePort)
		port["appProtocol"] = "http"
	}
	return opt.Some(object{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata":   owned(metadata(app, app.Name, AppLabels(app, opt.None[string]())), owner),
		"spec": object{
			"type":     "ClusterIP",
			"selector": selector(app, np.name),
			"ports":    []any{port},
		},
	}), nil
}

// routesHTTP: the app's web process is served over HTTP through the
// gateway.
func routesHTTP(app *v1alpha1.App) (bool, error) {
	web, err := webProcess(app)
	if err != nil {
		return false, err
	}
	np, ok := web.Get()
	return ok && np.process.Protocol.IsHTTP(), nil
}

// httpRoute is the Gateway API HTTPRoute of the app; none without a
// gateway, an HTTP web process or a host. With TLS the route attaches only
// to the HTTPS listeners of its hosts (scenario 9); plain HTTP is answered
// by the platform redirect.
func httpRoute(app *v1alpha1.App, p Platform, owner OwnerReference) (opt.Val[object], error) {
	gateway, ok := p.Gateway.Get()
	if !ok {
		return opt.None[object](), nil
	}
	if http, err := routesHTTP(app); err != nil || !http {
		return opt.None[object](), err
	}
	claims := DomainClaims(app, p)
	if len(claims) == 0 {
		return opt.None[object](), nil
	}
	var parents []any
	if p.TLS {
		sections := make([]string, 0, len(claims))
		for _, c := range claims {
			sections = append(sections, SectionFor(c, p))
		}
		slices.Sort(sections)
		for _, s := range slices.Compact(sections) {
			parents = append(parents, object{"name": gateway.Name, "namespace": gateway.Namespace, "sectionName": s})
		}
	} else {
		parents = []any{object{"name": gateway.Name, "namespace": gateway.Namespace}}
	}
	hosts := make([]any, 0, len(claims))
	for _, c := range claims {
		hosts = append(hosts, c.Host)
	}
	// serde_json::to_string(&claims): members in declaration order, which
	// is also the sorted order, so the canonical text is the same bytes.
	domains, err := wire.CanonicalValue(claims)
	if err != nil {
		domains = ""
	}
	meta := owned(metadata(app, app.Name, AppLabels(app, opt.None[string]())), owner)
	meta["annotations"] = object{DomainsAnnotation: domains}
	return opt.Some(object{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "HTTPRoute",
		"metadata":   meta,
		"spec": object{
			"parentRefs": parents,
			"hostnames":  hosts,
			"rules": []any{object{
				"matches":     []any{object{"path": object{"type": "PathPrefix", "value": "/"}}},
				"backendRefs": []any{object{"name": app.Name, "port": int64(ServicePort)}},
			}},
		},
	}), nil
}

// referenceGrant lets the Gateway read the Secrets of the app's own
// certificates (`tls: <secret>`), in the app's namespace. None when no
// domain brings its own certificate or TLS is off.
func referenceGrant(app *v1alpha1.App, p Platform, owner OwnerReference) (opt.Val[object], error) {
	gateway, ok := p.Gateway.Get()
	if !ok {
		return opt.None[object](), nil
	}
	if !p.TLS {
		return opt.None[object](), nil
	}
	if http, err := routesHTTP(app); err != nil || !http {
		return opt.None[object](), err
	}
	var secrets []string
	for _, c := range DomainClaims(app, p) {
		if m := c.Mode(); m.Kind == TLSSecret {
			secrets = append(secrets, m.Secret)
		}
	}
	if len(secrets) == 0 {
		return opt.None[object](), nil
	}
	slices.Sort(secrets)
	to := make([]any, 0, len(secrets))
	for _, name := range slices.Compact(secrets) {
		to = append(to, object{"group": "", "kind": "Secret", "name": name})
	}
	return opt.Some(object{
		"apiVersion": "gateway.networking.k8s.io/v1beta1",
		"kind":       "ReferenceGrant",
		"metadata":   owned(metadata(app, app.Name+GrantSuffix, AppLabels(app, opt.None[string]())), owner),
		"spec": object{
			"from": []any{object{"group": "gateway.networking.k8s.io", "kind": "Gateway", "namespace": gateway.Namespace}},
			"to":   to,
		},
	}), nil
}
