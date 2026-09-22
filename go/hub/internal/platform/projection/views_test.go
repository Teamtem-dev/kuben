package projection_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/projection"
	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestPodViewExtractsReasonAndReadiness(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api-web-abc", Namespace: "kb-shop-prod",
			Labels: map[string]string{v1alpha1.LabelApp: "api", v1alpha1.LabelProcess: "web", v1alpha1.LabelOrg: "org-1"},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "web", Ready: false, RestartCount: 4,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			}},
		},
	}
	v := projection.PodViewOf(pod)
	want := projection.PodView{
		Key: "kb-shop-prod/api-web-abc", Namespace: "kb-shop-prod", Name: "api-web-abc",
		Org: opt.Some("org-1"), App: opt.Some("api"), Process: opt.Some("web"),
		Phase: projection.PodRunning, Ready: false, Restarts: 4, Reason: opt.Some("CrashLoopBackOff"),
	}
	if diff := cmp.Diff(want, v, cmpOpts()); diff != "" {
		t.Fatalf("(-want +got):\n%s", diff)
	}
}

func webApp(t *testing.T) *v1alpha1.App {
	t.Helper()
	a := &v1alpha1.App{}
	a.Name = "web"
	a.Namespace = "kb-shop-prod"
	err := json.Unmarshal([]byte(`{
		"source": { "image": "nginx:1.27" },
		"runtime": { "processes": { "web": { "port": 80 } } },
		"env": [
			{ "name": "MODE", "value": "s3cr3t-plain-value" },
			{ "name": "TOKEN", "fromSecret": { "name": "api", "key": "token" } }
		]
	}`), &a.Spec)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAppViewNeverCarriesEnvValues(t *testing.T) {
	v := projection.AppViewOf(webApp(t))
	if v.Key != "kb-shop-prod/web" || v.Image.Or("") != "nginx:1.27" {
		t.Fatalf("view: %+v", v)
	}
	if diff := cmp.Diff(projection.EnvVarRef{Name: "MODE"}, v.Env[0], cmpOpts()); diff != "" {
		t.Fatal(diff)
	}
	if v.Env[1].Secret.Or("") != "api/token" {
		t.Fatalf("secret: %+v", v.Env[1])
	}
	if text := mustJSON(t, v); strings.Contains(text, "s3cr3t-plain-value") {
		t.Fatalf("plain values must not be serialized: %s", text)
	}
}

// The views reach the console as the Rust structs serialized them:
// snake_case members in declaration order, absent optionals as null,
// lists always as arrays.
func TestViewsSerializeAsTheRustStructs(t *testing.T) {
	created := metav1.NewTime(time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC))
	pod := projection.PodViewOf(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}})
	if got, want := mustJSON(t, pod), `{"key":"ns/p","namespace":"ns","name":"p","org":null,"app":null,"process":null,"phase":"unknown","ready":false,"restarts":0,"reason":null,"node":null,"started_at":null}`; got != want {
		t.Errorf("pod:\n got %s\nwant %s", got, want)
	}

	project := &v1alpha1.Project{}
	project.Name, project.UID, project.CreationTimestamp = "shop", "u-1", created
	project.Spec.DisplayName = "Shop"
	if got, want := mustJSON(t, projection.ProjectViewOf(project)), `{"name":"shop","uid":"u-1","display_name":"Shop","description":null,"org":null,"environments":0,"ready":false,"deleting":false,"created_at":"2026-09-15T00:00:00Z"}`; got != want {
		t.Errorf("project:\n got %s\nwant %s", got, want)
	}

	env := &v1alpha1.Environment{}
	env.Name = "shop-prod"
	env.Spec.Project, env.Spec.Type = "shop", v1alpha1.EnvironmentTypeProduction
	if got, want := mustJSON(t, projection.EnvironmentViewOf(env)), `{"name":"shop-prod","uid":null,"project":"shop","org":null,"env_type":"production","namespace":"kb-shop-prod","phase":null,"ready":false,"message":null,"deleting":false,"deletion_scheduled_at":null,"created_at":null}`; got != want {
		t.Errorf("environment:\n got %s\nwant %s", got, want)
	}

	a := webApp(t)
	a.Spec.Env = nil
	if got, want := mustJSON(t, projection.AppViewOf(a)), `{"key":"kb-shop-prod/web","namespace":"kb-shop-prod","name":"web","uid":null,"org":null,"project":null,"environment":null,"image":"nginx:1.27","git_repo":null,"url":null,"ready":false,"reason":null,"message":null,"processes":[{"name":"web","command":[],"port":80,"size":"small","min_replicas":1,"max_replicas":1,"schedule":null,"protocol":"http"}],"env":[],"domains":[],"volumes":[],"created_at":null}`; got != want {
		t.Errorf("app:\n got %s\nwant %s", got, want)
	}

	exposure := projection.ExposureView{Hosts: []projection.HostExposure{{Host: "a.b", TLS: "none"}}}
	if got, want := mustJSON(t, exposure), `{"accepted":null,"message":null,"hosts":[{"host":"a.b","tls":"none","certificate_ready":null,"certificate_message":null}]}`; got != want {
		t.Errorf("exposure:\n got %s\nwant %s", got, want)
	}
	cert := projection.CertificateView{Key: "ns/c", NotAfter: opt.Some("2027-01-01T00:00:00Z")}
	if got, want := mustJSON(t, cert), `{"key":"ns/c","ready":false,"message":null,"not_after":"2027-01-01T00:00:00Z"}`; got != want {
		t.Errorf("certificate:\n got %s\nwant %s", got, want)
	}
}
