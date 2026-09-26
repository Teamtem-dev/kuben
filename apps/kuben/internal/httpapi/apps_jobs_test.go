package httpapi_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi"
)

// routes/apps/jobs.rs manual_job_names_fit_the_limit.
func TestManualJobNamesFitTheLimit(t *testing.T) {
	name := httpapi.ManualJobName(strings.Repeat("x", 80), 1_757_548_800)
	if len(name) > 63 || !strings.HasSuffix(name, "-run-1757548800") {
		t.Fatalf("name: len=%d, name=%s", len(name), name)
	}
}

// Without a Kubernetes cluster configured, runApp answers 503 once the
// process is known (an app without a scheduled process is refused first,
// as in Rust). An empty process is a process as given, as Rust's
// Some(""): it goes on to the cluster instead of picking the first
// scheduled one.
func TestRunAppWithoutClusterAnswers503(t *testing.T) {
	f := newFixture(t)
	f.seedApp()
	alice := f.signIn("alice@example.com", seedPassword)
	tests := []struct {
		name string
		body map[string]any
		want int
	}{
		{"no process, none scheduled", map[string]any{}, http.StatusUnprocessableEntity},
		{"a process", map[string]any{"process": "web"}, http.StatusServiceUnavailable},
		{"an empty process", map[string]any{"process": ""}, http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, body, _ := alice.do("POST", "/api/v1/projects/shop/environments/prod/apps/api/run", tt.body)
			if status != tt.want {
				t.Fatalf("expected %d, got %d: %v", tt.want, status, body)
			}
		})
	}
}

// A manual run is a Job from the CronJob's template, named after the
// injected clock, marked as instantiated by hand and owned by the CronJob
// without being controlled by it.
func TestStartManualJob(t *testing.T) {
	cron := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "api-report", Namespace: "kb-shop-prod", UID: "cron-uid"},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 3 * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"kuben.dev/process": "report"}},
				Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "report", Image: "ghcr.io/acme/api:1.2.3"}},
				}}},
			},
		},
	}
	client := fake.NewClientset(cron)
	name, err := httpapi.StartManualJob(t.Context(), client, "kb-shop-prod", "api-report", 1_757_548_800_999)
	if err != nil {
		t.Fatal(err)
	}
	if name != "api-report-run-1757548800" {
		t.Fatalf("name: %s", name)
	}
	job, err := client.BatchV1().Jobs("kb-shop-prod").Get(t.Context(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(map[string]string{"cronjob.kubernetes.io/instantiate": "manual"}, job.Annotations); diff != "" {
		t.Fatalf("annotations (-want +got):\n%s", diff)
	}
	notController := false
	wantOwners := []metav1.OwnerReference{{
		APIVersion: "batch/v1", Kind: "CronJob", Name: "api-report", UID: "cron-uid", Controller: &notController,
	}}
	if diff := cmp.Diff(wantOwners, job.OwnerReferences); diff != "" {
		t.Fatalf("owners (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(cron.Spec.JobTemplate.Labels, job.Labels); diff != "" {
		t.Fatalf("labels (-want +got):\n%s", diff)
	}

	// The empty process's CronJob `api-` does not exist: 404.
	_, err = httpapi.StartManualJob(t.Context(), client, "kb-shop-prod", "api-", 1_757_548_800_000)
	var ke *kerrors.Error
	if !errors.As(err, &ke) || ke.Code != kerrors.NotFound {
		t.Fatalf("expected not found, got %v", err)
	}
}

// Seconds of the clock, floored; a time before the epoch is 0.
func TestUnixSeconds(t *testing.T) {
	for ms, want := range map[int64]uint64{0: 0, 999: 0, 1000: 1, 1_757_548_800_999: 1_757_548_800, -5: 0} {
		if got := httpapi.UnixSeconds(ms); got != want {
			t.Fatalf("UnixSeconds(%d) = %d, want %d", ms, got, want)
		}
	}
}
