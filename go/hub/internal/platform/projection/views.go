package projection

import (
	"encoding/json"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/render"
	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

// View types (projection/views.rs). Small, comparable by value (so an
// unchanged object produces no delta) and serialized for the UI exactly as
// the Rust structs were: snake_case members in declaration order, an absent
// optional as null, a list always as an array. Every view carries its org
// so streams can be filtered per tenant. A view is never changed after it
// was stored: the projections hand out pointers to it.

// PodPhase is a pod's lifecycle phase.
type PodPhase string

// The pod phases.
const (
	PodPending   PodPhase = "pending"
	PodRunning   PodPhase = "running"
	PodSucceeded PodPhase = "succeeded"
	PodFailed    PodPhase = "failed"
	PodUnknown   PodPhase = "unknown"
)

// PodPhaseOf reads the Kubernetes phase; anything else is PodUnknown.
func PodPhaseOf(phase corev1.PodPhase) PodPhase {
	switch phase {
	case corev1.PodPending:
		return PodPending
	case corev1.PodRunning:
		return PodRunning
	case corev1.PodSucceeded:
		return PodSucceeded
	case corev1.PodFailed:
		return PodFailed
	case corev1.PodUnknown:
		return PodUnknown
	}
	return PodUnknown
}

// PodView is a pod of an app.
type PodView struct {
	// Key is `namespace/name`.
	Key       string          `json:"key"`
	Namespace string          `json:"namespace"`
	Name      string          `json:"name"`
	Org       opt.Val[string] `json:"org"`
	App       opt.Val[string] `json:"app"`
	Process   opt.Val[string] `json:"process"`
	Phase     PodPhase        `json:"phase"`
	Ready     bool            `json:"ready"`
	Restarts  int32           `json:"restarts"`
	// Reason is CrashLoopBackOff, OOMKilled, ImagePullBackOff, ...
	Reason    opt.Val[string] `json:"reason"`
	Node      opt.Val[string] `json:"node"`
	StartedAt opt.Val[string] `json:"started_at"`
}

// text is absent for "": the Go API types use "" where k8s-openapi had
// None.
func text(s string) opt.Val[string] {
	if s == "" {
		return opt.None[string]()
	}
	return opt.Some(s)
}

func labelOf(labels map[string]string, key string) opt.Val[string] {
	v, ok := labels[key]
	if !ok {
		return opt.None[string]()
	}
	return opt.Some(v)
}

// timestamp prints a Kubernetes time as jiff's Timestamp did: RFC 3339 in
// UTC, fractional seconds only when there are some.
func timestamp(t *metav1.Time) opt.Val[string] {
	if t == nil || t.IsZero() {
		return opt.None[string]()
	}
	return opt.Some(t.UTC().Format(time.RFC3339Nano))
}

// PodViewOf is the view of pod.
func PodViewOf(pod *corev1.Pod) PodView {
	statuses := pod.Status.ContainerStatuses
	ready := len(statuses) > 0
	var restarts int32
	reason := opt.None[string]()
	for _, c := range statuses {
		ready = ready && c.Ready
		restarts += c.RestartCount
		if reason.IsSome() {
			continue
		}
		if w := c.State.Waiting; w != nil && w.Reason != "" {
			reason = opt.Some(w.Reason)
		} else if term := c.State.Terminated; term != nil && term.Reason != "" {
			reason = opt.Some(term.Reason)
		}
	}
	if reason.IsNone() {
		reason = text(pod.Status.Reason)
	}
	return PodView{
		Key:       pod.Namespace + "/" + pod.Name,
		Namespace: pod.Namespace,
		Name:      pod.Name,
		Org:       labelOf(pod.Labels, v1alpha1.LabelOrg),
		App:       labelOf(pod.Labels, v1alpha1.LabelApp),
		Process:   labelOf(pod.Labels, v1alpha1.LabelProcess),
		Phase:     PodPhaseOf(pod.Status.Phase),
		Ready:     ready,
		Restarts:  restarts,
		Reason:    reason,
		Node:      text(pod.Spec.NodeName),
		StartedAt: timestamp(pod.Status.StartTime),
	}
}

func readyCondition(conditions v1alpha1.Conditions) (v1alpha1.Condition, bool) {
	for _, c := range conditions {
		if c.Type == v1alpha1.ConditionReady {
			return c, true
		}
	}
	return v1alpha1.Condition{}, false
}

// ProjectView is a project.
type ProjectView struct {
	Name         string          `json:"name"`
	UID          opt.Val[string] `json:"uid"`
	DisplayName  string          `json:"display_name"`
	Description  opt.Val[string] `json:"description"`
	Org          opt.Val[string] `json:"org"`
	Environments uint32          `json:"environments"`
	Ready        bool            `json:"ready"`
	Deleting     bool            `json:"deleting"`
	CreatedAt    opt.Val[string] `json:"created_at"`
}

// ProjectViewOf is the view of p.
func ProjectViewOf(p *v1alpha1.Project) ProjectView {
	v := ProjectView{
		Name:        p.Name,
		UID:         text(string(p.UID)),
		DisplayName: p.Spec.DisplayName,
		Description: opt.FromPtr(p.Spec.Description),
		Org:         labelOf(p.Labels, v1alpha1.LabelOrg),
		Deleting:    p.DeletionTimestamp != nil,
		CreatedAt:   timestamp(&p.CreationTimestamp),
	}
	if s := p.Status; s != nil {
		v.Environments = s.Environments
		c, ok := readyCondition(s.Conditions)
		v.Ready = ok && c.Status == "True"
	}
	return v
}

// EnvironmentView is an environment.
type EnvironmentView struct {
	Name    string          `json:"name"`
	UID     opt.Val[string] `json:"uid"`
	Project string          `json:"project"`
	Org     opt.Val[string] `json:"org"`
	// EnvType is standard, production or preview.
	EnvType   string `json:"env_type"`
	Namespace string `json:"namespace"`
	// Phase is Pending, Ready, Terminating or Degraded.
	Phase               opt.Val[string] `json:"phase"`
	Ready               bool            `json:"ready"`
	Message             opt.Val[string] `json:"message"`
	Deleting            bool            `json:"deleting"`
	DeletionScheduledAt opt.Val[string] `json:"deletion_scheduled_at"`
	CreatedAt           opt.Val[string] `json:"created_at"`
}

func envType(t v1alpha1.EnvironmentType) string {
	switch t {
	case v1alpha1.EnvironmentTypeStandard:
		return "standard"
	case v1alpha1.EnvironmentTypeProduction:
		return "production"
	case v1alpha1.EnvironmentTypePreview:
		return "preview"
	}
	// The zero value is the Rust #[default], standard.
	return "standard"
}

// EnvironmentViewOf is the view of e.
func EnvironmentViewOf(e *v1alpha1.Environment) EnvironmentView {
	v := EnvironmentView{
		Name:      e.Name,
		UID:       text(string(e.UID)),
		Project:   e.Spec.Project,
		Org:       labelOf(e.Labels, v1alpha1.LabelOrg),
		EnvType:   envType(e.Spec.Type),
		Namespace: render.NamespaceName(e.Name),
		Deleting:  e.DeletionTimestamp != nil,
		CreatedAt: timestamp(&e.CreationTimestamp),
	}
	if s := e.Status; s != nil {
		if s.Namespace != nil {
			v.Namespace = *s.Namespace
		}
		v.Phase = opt.FromPtr(s.Phase)
		v.DeletionScheduledAt = opt.FromPtr(s.DeletionScheduledAt)
		if c, ok := readyCondition(s.Conditions); ok {
			v.Ready = c.Status == "True"
			v.Message = opt.FromPtr(c.Message)
		}
	}
	return v
}

// ProcessView is a process of an app.
type ProcessView struct {
	Name        string          `json:"name"`
	Command     []string        `json:"command"`
	Port        opt.Val[uint16] `json:"port"`
	Size        string          `json:"size"`
	MinReplicas uint32          `json:"min_replicas"`
	MaxReplicas uint32          `json:"max_replicas"`
	// Schedule is the cron expression of a scheduled process.
	Schedule opt.Val[string] `json:"schedule"`
	// Protocol is http or tcp.
	Protocol string `json:"protocol"`
}

// VolumeView is a volume of an app.
type VolumeView struct {
	Name      string `json:"name"`
	MountPath string `json:"mount_path"`
	Size      string `json:"size"`
}

// EnvVarRef is an environment variable *reference*. Values never travel
// through the shared stream; the app detail endpoint returns them after an
// authorization check.
type EnvVarRef struct {
	Name string `json:"name"`
	// Secret is `secret/key` for secret-backed variables.
	Secret opt.Val[string] `json:"secret"`
}

// AppView is an app.
type AppView struct {
	// Key is `namespace/name`.
	Key         string          `json:"key"`
	Namespace   string          `json:"namespace"`
	Name        string          `json:"name"`
	UID         opt.Val[string] `json:"uid"`
	Org         opt.Val[string] `json:"org"`
	Project     opt.Val[string] `json:"project"`
	Environment opt.Val[string] `json:"environment"`
	Image       opt.Val[string] `json:"image"`
	GitRepo     opt.Val[string] `json:"git_repo"`
	URL         opt.Val[string] `json:"url"`
	Ready       bool            `json:"ready"`
	Reason      opt.Val[string] `json:"reason"`
	Message     opt.Val[string] `json:"message"`
	Processes   []ProcessView   `json:"processes"`
	Env         []EnvVarRef     `json:"env"`
	Domains     []string        `json:"domains"`
	Volumes     []VolumeView    `json:"volumes"`
	CreatedAt   opt.Val[string] `json:"created_at"`
}

// AppViewOf is the view of a.
func AppViewOf(a *v1alpha1.App) AppView {
	v := AppView{
		Key:         a.Namespace + "/" + a.Name,
		Namespace:   a.Namespace,
		Name:        a.Name,
		UID:         text(string(a.UID)),
		Org:         labelOf(a.Labels, v1alpha1.LabelOrg),
		Project:     labelOf(a.Labels, v1alpha1.LabelProject),
		Environment: labelOf(a.Labels, v1alpha1.LabelEnvironment),
		Image:       opt.FromPtr(a.Spec.Source.Image),
		Processes:   processViews(a.Spec.Runtime.Processes),
		Env:         make([]EnvVarRef, 0, len(a.Spec.Env)),
		Domains:     make([]string, 0, len(a.Spec.Domains)),
		Volumes:     make([]VolumeView, 0, len(a.Spec.Volumes)),
		CreatedAt:   timestamp(&a.CreationTimestamp),
	}
	if g := a.Spec.Source.Git; g != nil {
		v.GitRepo = opt.Some(g.Repo)
	}
	if s := a.Status; s != nil {
		v.URL = opt.FromPtr(s.URL)
		if c, ok := readyCondition(s.Conditions); ok {
			v.Ready = c.Status == "True"
			v.Reason = opt.FromPtr(c.Reason)
			v.Message = opt.FromPtr(c.Message)
		}
	}
	for _, e := range a.Spec.Env {
		ref := EnvVarRef{Name: e.Name}
		secret := e.FromSecret
		if secret == nil {
			secret = e.FromService
		}
		if secret != nil {
			ref.Secret = opt.Some(secret.Name + "/" + secret.Key)
		}
		v.Env = append(v.Env, ref)
	}
	for _, d := range a.Spec.Domains {
		v.Domains = append(v.Domains, d.Host)
	}
	for _, vol := range a.Spec.Volumes {
		v.Volumes = append(v.Volumes, VolumeView{Name: vol.Name, MountPath: vol.MountPath, Size: vol.Size})
	}
	return v
}

// processViews are the processes in name order, as the Rust BTreeMap
// iterated them.
func processViews(processes map[string]v1alpha1.Process) []ProcessView {
	names := make([]string, 0, len(processes))
	for name := range processes {
		names = append(names, name)
	}
	slices.Sort(names)
	out := make([]ProcessView, 0, len(names))
	for _, name := range names {
		p := processes[name]
		protocol := "tcp"
		if p.Protocol.IsHTTP() {
			protocol = "http"
		}
		out = append(out, ProcessView{
			Name:        name,
			Command:     append(make([]string, 0, len(p.Command)), p.Command...),
			Port:        opt.FromPtr(p.Port),
			Size:        p.Size,
			MinReplicas: p.Replicas.Min,
			MaxReplicas: p.Replicas.Max,
			Schedule:    opt.FromPtr(p.Schedule),
			Protocol:    protocol,
		})
	}
	return out
}

// RouteView is an app's HTTPRoute (M2.3): its domains and whether a
// Gateway took it. Routes of both delivery paths (App controller and
// agent) look alike.
type RouteView struct {
	// Key is `namespace/name`; the name is the app's.
	Key       string `json:"key"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// Gateways is `namespace/name` of each parent Gateway, sorted.
	Gateways []string `json:"gateways"`
	// Sections are the listener names the route attaches to
	// (`sectionName`), if any.
	Sections []string             `json:"sections"`
	Domains  []render.DomainClaim `json:"domains"`
	// Accepted: a Gateway accepted the route and resolved its references;
	// absent until a gateway controller answered.
	Accepted opt.Val[bool]   `json:"accepted"`
	Message  opt.Val[string] `json:"message"`
}

// member reads a JSON object member; absent for anything but an object.
func member(v any, key string) any {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	return m[key]
}

func str(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func array(v any) []any {
	a, ok := v.([]any)
	if !ok {
		return nil
	}
	return a
}

// routeStatus folds the Accepted and ResolvedRefs verdicts of every parent.
func routeStatus(obj map[string]any) (opt.Val[bool], opt.Val[string]) {
	accepted := opt.None[bool]()
	for _, parent := range array(member(member(obj, "status"), "parents")) {
		conditions, ok := member(parent, "conditions").([]any)
		if !ok {
			continue
		}
		for _, c := range conditions {
			if t := member(c, "type"); t != "Accepted" && t != "ResolvedRefs" {
				continue
			}
			if member(c, "status") == "True" {
				if accepted.IsNone() {
					accepted = opt.Some(true)
				}
				continue
			}
			message := opt.None[string]()
			if m, ok := str(member(c, "message")); ok && m != "" {
				message = opt.Some(m)
			} else if r, ok := str(member(c, "reason")); ok {
				message = opt.Some(r)
			}
			return opt.Some(false), message
		}
	}
	return accepted, opt.None[string]()
}

// domainClaims reads the route annotation as serde read Vec<DomainClaim>:
// an array of objects whose host and tls are strings; anything else is
// unreadable.
func domainClaims(annotation string) ([]render.DomainClaim, bool) {
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(annotation), &raw); err != nil || raw == nil {
		return nil, false
	}
	claims := make([]render.DomainClaim, 0, len(raw))
	for _, m := range raw {
		var c render.DomainClaim
		host, okHost := m["host"]
		tls, okTLS := m["tls"]
		if !okHost || !okTLS || json.Unmarshal(host, &c.Host) != nil || json.Unmarshal(tls, &c.TLS) != nil ||
			string(host) == "null" || string(tls) == "null" {
			return nil, false
		}
		claims = append(claims, c)
	}
	return claims, true
}

// RouteViewOf is the view of an HTTPRoute.
func RouteViewOf(route *unstructured.Unstructured) RouteView {
	namespace, name := route.GetNamespace(), route.GetName()
	spec := member(route.Object, "spec")
	parents := array(member(spec, "parentRefs"))
	gateways := []string{}
	sections := []string{}
	for _, p := range parents {
		if gw, ok := str(member(p, "name")); ok {
			ns, ok := str(member(p, "namespace"))
			if !ok {
				ns = namespace
			}
			gateways = append(gateways, ns+"/"+gw)
		}
		if s, ok := str(member(p, "sectionName")); ok {
			sections = append(sections, s)
		}
	}
	slices.Sort(gateways)
	gateways = slices.Compact(gateways)
	domains, ok := domainClaims(route.GetAnnotations()[render.DomainsAnnotation])
	if !ok {
		// Routes written before M2.3 carry only their hostnames.
		domains = []render.DomainClaim{}
		for _, h := range array(member(spec, "hostnames")) {
			if host, ok := str(h); ok {
				domains = append(domains, render.DomainClaim{Host: host, TLS: "auto"})
			}
		}
	}
	accepted, message := routeStatus(route.Object)
	return RouteView{
		Key:       namespace + "/" + name,
		Namespace: namespace,
		Name:      name,
		Gateways:  gateways,
		Sections:  sections,
		Domains:   domains,
		Accepted:  accepted,
		Message:   message,
	}
}

// CertificateView is a cert-manager Certificate Kuben's gateway ordered
// (`kuben-tls-*`).
type CertificateView struct {
	// Key is `namespace/name`.
	Key     string          `json:"key"`
	Ready   bool            `json:"ready"`
	Message opt.Val[string] `json:"message"`
	// NotAfter is status.notAfter.
	NotAfter opt.Val[string] `json:"not_after"`
}

// condition is discovery::condition: the status and message of the
// condition of type conditionType.
func condition(obj map[string]any, conditionType string) (ready bool, message opt.Val[string], found bool) {
	for _, c := range array(member(member(obj, "status"), "conditions")) {
		if member(c, "type") != conditionType {
			continue
		}
		if m, ok := str(member(c, "message")); ok {
			message = opt.Some(m)
		}
		return member(c, "status") == "True", message, true
	}
	return false, opt.None[string](), false
}

// CertificateViewOf is the view of a cert-manager Certificate.
func CertificateViewOf(cert *unstructured.Unstructured) CertificateView {
	ready, message, _ := condition(cert.Object, "Ready")
	if m, ok := message.Get(); !ok || ready || m == "" {
		message = opt.None[string]()
	}
	notAfter := opt.None[string]()
	if s, ok := str(member(member(cert.Object, "status"), "notAfter")); ok {
		notAfter = opt.Some(s)
	}
	return CertificateView{
		Key:      cert.GetNamespace() + "/" + cert.GetName(),
		Ready:    ready,
		Message:  message,
		NotAfter: notAfter,
	}
}

// HostExposure is how one hostname of an app is reached.
type HostExposure struct {
	Host string `json:"host"`
	// TLS is auto, none or secret.
	TLS string `json:"tls"`
	// CertificateReady: for auto hosts with a certificate of their own,
	// whether it is issued.
	CertificateReady   opt.Val[bool]   `json:"certificate_ready"`
	CertificateMessage opt.Val[string] `json:"certificate_message"`
}

// ExposureView is whether an app can be reached through the gateway (I09:
// separate from being applied and healthy).
type ExposureView struct {
	Accepted opt.Val[bool]   `json:"accepted"`
	Message  opt.Val[string] `json:"message"`
	Hosts    []HostExposure  `json:"hosts"`
}
