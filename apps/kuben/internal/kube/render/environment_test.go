package render_test

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"
	batchv1 "k8s.io/api/batch/v1"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/capacity"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/render"
	"github.com/Teamtem-dev/kuben/packages/api/v1alpha1"
)

// The tests of controller::resources for the Environment half, demand and
// job_from_cron.

func environment(t *testing.T, quota string) *v1alpha1.Environment {
	t.Helper()
	e := &v1alpha1.Environment{}
	e.Name = "shop-prod"
	e.Labels = map[string]string{v1alpha1.LabelOrg: "org-1"}
	if err := json.Unmarshal([]byte(`{ "project": "shop", "type": "production" }`), &e.Spec); err != nil {
		t.Fatalf("spec: %v", err)
	}
	if quota != "" {
		e.Spec.Quota = &v1alpha1.Quota{}
		if err := json.Unmarshal([]byte(quota), e.Spec.Quota); err != nil {
			t.Fatalf("quota: %v", err)
		}
	}
	return e
}

func TestNamespaceHasPSAAndOwnershipLabels(t *testing.T) {
	ns := render.Namespace(environment(t, ""))
	if at(t, ns, "metadata", "name") != "kb-shop-prod" {
		t.Fatalf("name: %v", ns)
	}
	want := map[string]any{
		"pod-security.kubernetes.io/enforce":         "baseline",
		"pod-security.kubernetes.io/enforce-version": "latest",
		"pod-security.kubernetes.io/warn":            "restricted",
		"pod-security.kubernetes.io/audit":           "restricted",
		v1alpha1.LabelManagedBy:                      "kuben",
		v1alpha1.LabelProject:                        "shop",
		v1alpha1.LabelEnvironment:                    "shop-prod",
		v1alpha1.LabelOrg:                            "org-1",
		render.EnvTypeLabel:                          "production",
	}
	if diff := cmp.Diff(want, at(t, ns, "metadata", "labels")); diff != "" {
		t.Fatalf("labels (-want +got):\n%s", diff)
	}
	if got := at(t, ns, "apiVersion").(string) + "/" + at(t, ns, "kind").(string); got != "v1/Namespace" {
		t.Fatalf("type: %s", got)
	}
}

func TestQuotaAlwaysBlocksLoadBalancersAndAddsCaps(t *testing.T) {
	base := at(t, render.ResourceQuota(environment(t, "")), "spec", "hard")
	if diff := cmp.Diff(map[string]any{"services.loadbalancers": "0", "services.nodeports": "0"}, base); diff != "" {
		t.Fatalf("base (-want +got):\n%s", diff)
	}
	quota := render.ResourceQuota(environment(t, `{ "cpu": "4", "memory": "8Gi", "pods": 50 }`))
	capped := at(t, quota, "spec", "hard")
	want := map[string]any{
		"services.loadbalancers": "0", "services.nodeports": "0",
		"requests.cpu": "4", "requests.memory": "8Gi", "pods": "50",
	}
	if diff := cmp.Diff(want, capped); diff != "" {
		t.Fatalf("capped (-want +got):\n%s", diff)
	}
	if at(t, quota, "metadata", "name") != render.QuotaName || at(t, quota, "metadata", "namespace") != "kb-shop-prod" {
		t.Fatalf("metadata: %v", quota)
	}
}

func TestNetworkPolicyIsolatesManagedNamespaces(t *testing.T) {
	spec := at(t, render.NetworkPolicy(environment(t, "")), "spec")
	if diff := cmp.Diff([]any{"Ingress"}, at(t, spec, "policyTypes")); diff != "" {
		t.Fatal(diff)
	}
	if n := len(at(t, spec, "ingress").([]any)); n != 2 {
		t.Fatalf("%d ingress rules", n)
	}
	req := at(t, spec, "ingress", "1", "from", "0", "namespaceSelector", "matchExpressions", "0")
	if at(t, req, "key") != v1alpha1.LabelManagedBy || at(t, req, "operator") != "NotIn" {
		t.Fatalf("requirement: %v", req)
	}
	if diff := cmp.Diff(map[string]any{}, at(t, spec, "ingress", "0", "from", "0", "podSelector")); diff != "" {
		t.Fatal("same namespace:", diff)
	}
}

func TestLimitRangeDefaultsContainers(t *testing.T) {
	lr := render.LimitRange(environment(t, ""))
	want := map[string]any{
		"type":           "Container",
		"default":        map[string]any{"memory": "512Mi"},
		"defaultRequest": map[string]any{"cpu": "50m", "memory": "64Mi"},
	}
	if diff := cmp.Diff(want, at(t, lr, "spec", "limits", "0")); diff != "" {
		t.Fatal(diff)
	}
}

func TestThePeakDemandCountsReplicasSurgeAndOneRunPerSchedule(t *testing.T) {
	spec := `{
		"source": { "image": "ghcr.io/acme/api:1.2.3" },
		"runtime": { "processes": {
			"web": { "port": 3000, "size": "medium", "replicas": { "min": 1, "max": 3 } },
			"worker": { "size": "small" },
			"report": { "command": ["bin/report"], "schedule": "0 3 * * *", "size": "nano" }
		} }
	}`
	d, err := render.Demand(app(t, spec), render.DefaultPlatform())
	if err != nil {
		t.Fatalf("demand: %v", err)
	}
	// web: (3 + 1) × 250m/512Mi; worker: (1 + 1) × 100m/128Mi; report: 50m/64Mi.
	want := render.AppDemand{
		Peak:       capacity.Demand{Pods: 7, CPUMillis: 4*250 + 2*100 + 50, MemoryBytes: (4*512 + 2*128 + 64) << 20},
		LargestPod: render.PodRequests{CPUMillis: 250, MemoryBytes: 512 << 20},
	}
	if diff := cmp.Diff(want, d); diff != "" {
		t.Fatalf("(-want +got):\n%s", diff)
	}

	withVolume := app(t, `{
		"source": { "image": "ghcr.io/acme/api:1.2.3" },
		"runtime": { "processes": { "web": { "port": 3000, "size": "medium", "replicas": { "min": 1, "max": 1 } } } },
		"volumes": [{ "name": "data", "mountPath": "/data", "size": "1Gi" }]
	}`)
	d, err = render.Demand(withVolume, render.DefaultPlatform())
	if err != nil || d.Peak.Pods != 1 {
		t.Fatalf("Recreate has no surge pod: %+v %v", d, err)
	}

	unknown := app(t, spec)
	web := unknown.Spec.Runtime.Processes["web"]
	web.Size = "huge"
	unknown.Spec.Runtime.Processes["web"] = web
	if _, err := render.Demand(unknown, render.DefaultPlatform()); reason(err) != render.ReasonUnknownSize {
		t.Fatalf("unknown size: %v", err)
	}
}

// The job_from_cron half of the Rust test scheduled_processes_become_cron_jobs
// (the CronJob half is in build_test.go).
func TestAJobFromACronJobIsOwnedByIt(t *testing.T) {
	d := build(t, app(t, `{
		"source": { "image": "ghcr.io/acme/api:1.2.3" },
		"runtime": { "processes": {
			"web": { "port": 3000 },
			"report": { "command": ["bin/report"], "schedule": "0 3 * * *", "timeZone": "Europe/Berlin" }
		} }
	}`), render.DefaultPlatform())
	data, err := json.Marshal(d.CronJobs[0])
	if err != nil {
		t.Fatal(err)
	}
	var live batchv1.CronJob
	if err := json.Unmarshal(data, &live); err != nil {
		t.Fatal(err)
	}
	live.UID = "cron-uid"
	job := render.JobFromCron(&live, "api-report-manual-1")
	if len(job.OwnerReferences) != 1 || job.OwnerReferences[0].Kind != "CronJob" ||
		job.OwnerReferences[0].UID != "cron-uid" || *job.OwnerReferences[0].Controller {
		t.Fatalf("owner: %+v", job.OwnerReferences)
	}
	if len(job.Spec.Template.Spec.Containers) != 1 || job.Name != "api-report-manual-1" || job.Namespace != "kb-shop-prod" {
		t.Fatalf("job: %+v", job)
	}
	if job.Annotations["cronjob.kubernetes.io/instantiate"] != "manual" || job.Labels[v1alpha1.LabelProcess] != "report" {
		t.Fatalf("metadata: %+v", job.ObjectMeta)
	}
	live.UID = ""
	if job := render.JobFromCron(&live, "x"); job.OwnerReferences != nil {
		t.Fatal("no uid, no owner")
	}
}

// routes/scope.rs: an environment's Kubernetes name is `<project>-<env>`,
// its namespace `kb-<that>`.
func TestEnvironmentNames(t *testing.T) {
	if got := render.EnvironmentResourceName("shop", "prod"); got != "shop-prod" {
		t.Errorf("resource name: %s", got)
	}
	if got := render.NamespaceName(render.EnvironmentResourceName("shop", "prod")); got != "kb-shop-prod" {
		t.Errorf("namespace: %s", got)
	}
}
