package render

import (
	"strconv"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/capacity"
	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

// The Environment half of controller::resources: an Environment becomes its
// namespace and the guard rails in it (quota, limit defaults, isolation
// policy); plus an app's peak demand (M4.5) and the one-off Job of a
// CronJob ("run now"). Objects are JSON maps shaped as k8s-openapi
// serialized its types (unset fields absent), like the rest of the builder.

// Names and labels of the environment objects.
const (
	// EnvFinalizer is the finalizer that implements the environment
	// deletion policy.
	EnvFinalizer = "kuben.dev/environment"
	// QuotaName is the environment namespace's ResourceQuota.
	QuotaName = "kuben-quota"
	// LimitsName is the environment namespace's LimitRange.
	LimitsName = "kuben-defaults"
	// NetpolName is the environment namespace's NetworkPolicy.
	NetpolName = "kuben-isolation"
	// EnvTypeLabel carries the environment type on namespaces.
	EnvTypeLabel = "kuben.dev/environment-type"
)

// envTypeName is the label value of an environment type; the zero value is
// standard, as it decodes.
func envTypeName(t v1alpha1.EnvironmentType) string {
	if t == "" {
		t = v1alpha1.EnvironmentTypeStandard
	}
	switch t {
	case v1alpha1.EnvironmentTypeStandard:
		return "standard"
	case v1alpha1.EnvironmentTypeProduction:
		return "production"
	case v1alpha1.EnvironmentTypePreview:
		return "preview"
	}
	return string(t)
}

// envMetadata is the metadata of an object in env's namespace.
func envMetadata(env *v1alpha1.Environment, name string) object {
	return object{
		"name":      name,
		"namespace": NamespaceName(env.Name),
		"labels":    stringMap(managedLabels()),
	}
}

// Namespace is env's namespace with Pod Security Admission: `baseline` is
// enforced (arbitrary images still run), `restricted` violations are
// warned and audited.
func Namespace(env *v1alpha1.Environment) map[string]any {
	l := managedLabels()
	l[v1alpha1.LabelProject] = env.Spec.Project
	l[v1alpha1.LabelEnvironment] = env.Name
	l[EnvTypeLabel] = envTypeName(env.Spec.Type)
	if org, ok := env.Labels[v1alpha1.LabelOrg]; ok {
		l[v1alpha1.LabelOrg] = org
	}
	l["pod-security.kubernetes.io/enforce"] = "baseline"
	l["pod-security.kubernetes.io/enforce-version"] = "latest"
	l["pod-security.kubernetes.io/warn"] = "restricted"
	l["pod-security.kubernetes.io/audit"] = "restricted"
	return object{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata":   object{"name": NamespaceName(env.Name), "labels": stringMap(l)},
	}
}

// ResourceQuota: tenants can never create LoadBalancer or NodePort
// Services (cost and exposure), plus the optional CPU, memory and pod caps
// of the spec.
func ResourceQuota(env *v1alpha1.Environment) map[string]any {
	hard := object{"services.loadbalancers": "0", "services.nodeports": "0"}
	if q := env.Spec.Quota; q != nil {
		if q.CPU != nil {
			hard["requests.cpu"] = *q.CPU
		}
		if q.Memory != nil {
			hard["requests.memory"] = *q.Memory
		}
		if q.Pods != nil {
			hard["pods"] = strconv.FormatUint(uint64(*q.Pods), 10)
		}
	}
	return object{
		"apiVersion": "v1",
		"kind":       "ResourceQuota",
		"metadata":   envMetadata(env, QuotaName),
		"spec":       object{"hard": hard},
	}
}

// LimitRange is the defaults of containers that declare no resources
// (keeps quota admission working and bounds noisy neighbours).
func LimitRange(env *v1alpha1.Environment) map[string]any {
	return object{
		"apiVersion": "v1",
		"kind":       "LimitRange",
		"metadata":   envMetadata(env, LimitsName),
		"spec": object{"limits": []any{object{
			"type":           "Container",
			"default":        object{"memory": "512Mi"},
			"defaultRequest": object{"cpu": "50m", "memory": "64Mi"},
		}}},
	}
}

// NetworkPolicy isolates tenants: ingress is allowed from the same
// namespace and from namespaces Kuben does not manage (gateway, monitoring,
// kuben-system), so environments cannot reach each other.
func NetworkPolicy(env *v1alpha1.Environment) map[string]any {
	sameNamespace := object{"podSelector": object{}}
	unmanagedNamespaces := object{"namespaceSelector": object{
		"matchExpressions": []any{object{
			"key":      v1alpha1.LabelManagedBy,
			"operator": "NotIn",
			"values":   []any{v1alpha1.LabelManagerValue},
		}},
	}}
	return object{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "NetworkPolicy",
		"metadata":   envMetadata(env, NetpolName),
		"spec": object{
			"podSelector": object{},
			"policyTypes": []any{"Ingress"},
			"ingress": []any{
				object{"from": []any{sameNamespace}},
				object{"from": []any{unmanagedNamespaces}},
			},
		},
	}
}

// PodRequests is what one pod requests.
type PodRequests struct {
	CPUMillis   uint64
	MemoryBytes uint64
}

// AppDemand is what an app may request at its peak, for admission (M4.5).
type AppDemand struct {
	// Peak is every long-running process at its maximum replicas, plus the
	// surge pod of a rolling update, and one pod of each scheduled process.
	Peak capacity.Demand
	// LargestPod is the requests of the app's largest pod.
	LargestPod PodRequests
}

// Demand is the peak requests of app with p's size presets. A size that is
// unknown, or whose requests cannot be read, is a ReasonUnknownSize error.
func Demand(app *v1alpha1.App, p Platform) (AppDemand, error) {
	var surge uint64
	if len(app.Spec.Volumes) == 0 {
		surge = 1
	}
	var out AppDemand
	for _, np := range processes(app) {
		size, err := preset(p, np)
		if err != nil {
			return AppDemand{}, err
		}
		cpu, ok := capacity.CPUMillis(size.CPURequest)
		if !ok {
			return AppDemand{}, errUnknownSize(np.name, np.process.Size)
		}
		memory, ok := capacity.Bytes(size.MemoryRequest)
		if !ok {
			return AppDemand{}, errUnknownSize(np.name, np.process.Size)
		}
		// A schedule runs with `concurrencyPolicy: Forbid`: one run at a time.
		pods := uint64(1)
		if np.process.Schedule == nil {
			pods = uint64(max(np.process.Replicas.Max, np.process.Replicas.Min)) + surge
		}
		out.Peak = out.Peak.Plus(capacity.Pods(pods, cpu, memory))
		out.LargestPod = PodRequests{
			CPUMillis:   max(out.LargestPod.CPUMillis, cpu),
			MemoryBytes: max(out.LargestPod.MemoryBytes, memory),
		}
	}
	return out, nil
}

// JobFromCron is a one-off Job named name from a live CronJob's template
// ("run now"), as `kubectl create job --from=cronjob/<name>` makes it. It
// is owned by the CronJob (not as its controller) when the CronJob has a
// uid.
func JobFromCron(cron *batchv1.CronJob, name string) batchv1.Job {
	template := cron.Spec.JobTemplate.DeepCopy()
	job := batchv1.Job{
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   cron.Namespace,
			Labels:      template.Labels,
			Annotations: map[string]string{"cronjob.kubernetes.io/instantiate": "manual"},
		},
		Spec: template.Spec,
	}
	if cron.UID != "" {
		notController := false
		job.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "batch/v1",
			Kind:       "CronJob",
			Name:       cron.Name,
			UID:        cron.UID,
			Controller: &notController,
		}}
	}
	return job
}
