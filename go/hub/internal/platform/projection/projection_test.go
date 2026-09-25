package projection_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/projection"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/render"
)

func cmpOpts() cmp.Option {
	return cmp.AllowUnexported(opt.Val[string]{}, opt.Val[bool]{}, opt.Val[uint16]{})
}

func pod(name string, ready bool) projection.PodView {
	return projection.PodView{
		Key: "ns/" + name, Namespace: "ns", Name: name,
		App: opt.Some("api"), Process: opt.Some("web"),
		Phase: projection.PodRunning, Ready: ready,
	}
}

func env(name string, ready bool) projection.EnvironmentView {
	return projection.EnvironmentView{
		Name: name, Project: "shop", Org: opt.Some("o"), EnvType: "standard",
		Namespace: "kb-" + name, Ready: ready,
	}
}

func next(t *testing.T, s *projection.Subscription) projection.Delta {
	t.Helper()
	got, ok := s.TryRecv()
	if !ok || got.Delta == nil {
		t.Fatalf("no delta: %+v", got)
	}
	return got.Delta
}

func TestUpsertPublishesDeltaAndBumpsSeq(t *testing.T) {
	p := projection.New()
	rx := p.Subscribe()
	defer rx.Close()
	p.UpsertPod(pod("a", false))
	if p.Seq() != 1 {
		t.Fatal(p.Seq())
	}
	p.UpsertPod(pod("a", false)) // identical → no delta
	if p.Seq() != 1 {
		t.Fatal(p.Seq())
	}
	p.UpsertPod(pod("a", true))
	if p.Seq() != 2 {
		t.Fatal(p.Seq())
	}
	if d, ok := next(t, rx).(projection.PodUpsert); !ok || d.Seq != 1 {
		t.Fatalf("got %#v", d)
	}
	if d, ok := next(t, rx).(projection.PodUpsert); !ok || d.Seq != 2 || !d.Pod.Ready {
		t.Fatalf("got %#v", d)
	}
	p.RemovePod("ns/a")
	if p.PodCount() != 0 {
		t.Fatal("removed")
	}
	if d, ok := next(t, rx).(projection.PodDelete); !ok || d.Seq != 3 {
		t.Fatalf("got %#v", d)
	}
}

func TestSnapshotIsSortedAndTagged(t *testing.T) {
	p := projection.New()
	p.UpsertPod(pod("b", true))
	p.UpsertPod(pod("a", true))
	p.UpsertEnvironment(env("shop-prod", true))
	s := p.Snapshot()
	if s.Seq != 3 || len(s.Pods) != 2 || s.Pods[0].Name != "a" || s.Pods[1].Name != "b" || len(s.Environments) != 1 {
		t.Fatalf("snapshot: %+v", s)
	}
}

func TestResyncReplacesAndNotifies(t *testing.T) {
	p := projection.New()
	rx := p.Subscribe()
	defer rx.Close()
	p.UpsertPod(pod("old", true))
	p.ReplacePods([]projection.PodView{pod("new", true)})
	if _, ok := p.Pod("ns/old"); ok {
		t.Fatal("old is gone")
	}
	if _, ok := p.Pod("ns/new"); !ok {
		t.Fatal("new is there")
	}
	next(t, rx)
	if _, ok := next(t, rx).(projection.Resync); !ok {
		t.Fatal("resync")
	}
}

func TestSyncedOnlyAfterEveryInformerListedOnce(t *testing.T) {
	p := projection.New()
	p.UpsertPod(pod("a", true)) // watch events do not count as a sync
	p.ReplacePods(nil)
	p.ReplaceProjects(nil)
	p.ReplaceEnvironments(nil)
	if p.IsSynced() {
		t.Fatal("apps have not listed yet")
	}
	select {
	case <-p.Synced():
		t.Fatal("not synced yet")
	default:
	}
	p.ReplaceApps(nil)
	if !p.IsSynced() {
		t.Fatal("synced")
	}
	select {
	case <-p.Synced():
	case <-time.After(5 * time.Second):
		t.Fatal("Synced resolves")
	}
	p.ReplacePods(nil) // a later re-list keeps it synced
	if !p.IsSynced() {
		t.Fatal("still synced")
	}
}

func TestNamesTheInformersStillListing(t *testing.T) {
	p := projection.New()
	if diff := cmp.Diff([]string{"pods", "projects", "environments", "apps"}, p.PendingKinds()); diff != "" {
		t.Fatal(diff)
	}
	p.ReplacePods(nil)
	p.ReplaceEnvironments(nil)
	if diff := cmp.Diff([]string{"projects", "apps"}, p.PendingKinds()); diff != "" {
		t.Fatal(diff)
	}
	p.ReplaceProjects(nil)
	p.ReplaceApps(nil)
	if len(p.PendingKinds()) != 0 {
		t.Fatal(p.PendingKinds())
	}
}

func TestEnvironmentsAndPodsOfApp(t *testing.T) {
	p := projection.New()
	p.UpsertEnvironment(env("shop-dev", false))
	p.UpsertEnvironment(env("shop-dev", true))
	if e, ok := p.Environment("shop-dev"); !ok || !e.Ready {
		t.Fatal("ready")
	}
	p.RemoveEnvironment("shop-dev")
	if len(p.Environments()) != 0 {
		t.Fatal("removed")
	}
	p.UpsertPod(pod("api-1", true))
	other := pod("worker-1", true)
	other.App = opt.Some("worker")
	p.UpsertPod(other)
	if len(p.PodsOfApp("ns", "api")) != 1 {
		t.Fatal(p.PodsOfApp("ns", "api"))
	}
}

func routeObject(status any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "HTTPRoute",
		"metadata": map[string]any{
			"name": "api", "namespace": "kb-shop-prod",
			"annotations": map[string]any{
				render.DomainsAnnotation: `[{"host":"shop.acme.com","tls":"auto"},{"host":"old.acme.com","tls":"none"}]`,
			},
		},
		"spec": map[string]any{
			"hostnames": []any{"shop.acme.com", "old.acme.com"},
			"parentRefs": []any{
				map[string]any{"name": "kuben", "namespace": "kuben-system", "sectionName": render.HostListenerName("shop.acme.com")},
				map[string]any{"name": "kuben", "namespace": "kuben-system", "sectionName": "p-x"},
			},
		},
		"status": status,
	}}
}

func conditions(cs ...map[string]any) map[string]any {
	list := make([]any, 0, len(cs))
	for _, c := range cs {
		list = append(list, c)
	}
	return map[string]any{"parents": []any{map[string]any{"conditions": list}}}
}

func TestExposureJoinsTheRouteAndItsCertificates(t *testing.T) {
	p := projection.New()
	if _, ok := p.Exposure("kb-shop-prod", "api"); ok {
		t.Fatal("no route yet")
	}
	refused := projection.RouteViewOf(routeObject(conditions(
		map[string]any{"type": "Accepted", "status": "True"},
		map[string]any{"type": "ResolvedRefs", "status": "False", "reason": "RefNotPermitted", "message": ""},
	)))
	if refused.Accepted != opt.Some(false) || refused.Message != opt.Some("RefNotPermitted") ||
		!cmp.Equal(refused.Gateways, []string{"kuben-system/kuben"}) {
		t.Fatalf("refused: %+v", refused)
	}

	rx := p.Subscribe()
	defer rx.Close()
	p.UpsertRoute(projection.RouteViewOf(routeObject(conditions(
		map[string]any{"type": "Accepted", "status": "True"},
		map[string]any{"type": "ResolvedRefs", "status": "True"},
	))))
	if d, ok := next(t, rx).(projection.ExposureChanged); !ok || d.Key != "kb-shop-prod/api" {
		t.Fatalf("got %#v", d)
	}
	exposure, _ := p.Exposure("kb-shop-prod", "api")
	if exposure.Accepted != opt.Some(true) {
		t.Fatal("accepted")
	}
	want := []projection.HostExposure{
		{Host: "shop.acme.com", TLS: "auto", CertificateReady: opt.Some(false), CertificateMessage: opt.Some("not issued yet")},
		{Host: "old.acme.com", TLS: "none"},
	}
	if diff := cmp.Diff(want, exposure.Hosts, cmpOpts()); diff != "" {
		t.Fatal(diff)
	}

	// The certificate is issued: the app's exposure changes, under the app's key.
	p.UpsertCertificate(projection.CertificateView{
		Key: "kuben-system/" + render.HostSecretName("shop.acme.com"), Ready: true,
		NotAfter: opt.Some("2027-01-01T00:00:00Z"),
	})
	if d, ok := next(t, rx).(projection.ExposureChanged); !ok || d.Key != "kb-shop-prod/api" {
		t.Fatalf("got %#v", d)
	}
	if exposure, _ := p.Exposure("kb-shop-prod", "api"); exposure.Hosts[0].CertificateReady != opt.Some(true) {
		t.Fatal("issued")
	}
	p.UpsertCertificate(projection.CertificateView{Key: "kuben-system/kuben-tls-unrelated", Ready: true})
	if got, ok := rx.TryRecv(); ok {
		t.Fatalf("an unrelated certificate names no app: %+v", got)
	}
}

func TestRouteViewReadsOldRoutesAndSerializesAsRust(t *testing.T) {
	old := routeObject(nil)
	unstructured.RemoveNestedField(old.Object, "metadata", "annotations")
	unstructured.RemoveNestedField(old.Object, "status")
	v := projection.RouteViewOf(old)
	want := `{"key":"kb-shop-prod/api","namespace":"kb-shop-prod","name":"api","gateways":["kuben-system/kuben"],` +
		`"sections":["` + render.HostListenerName("shop.acme.com") + `","p-x"],` +
		`"domains":[{"host":"shop.acme.com","tls":"auto"},{"host":"old.acme.com","tls":"auto"}],"accepted":null,"message":null}`
	if got := mustJSON(t, v); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	// An annotation serde could not read falls back to the hostnames.
	bad := routeObject(nil)
	bad.SetAnnotations(map[string]string{render.DomainsAnnotation: `[{"host":"x"}]`})
	if got := projection.RouteViewOf(bad).Domains; len(got) != 2 || got[0].TLS != "auto" {
		t.Fatalf("fallback: %+v", got)
	}
}

func TestDeltasSerializeAsTheRustEnum(t *testing.T) {
	a := pod("a", true)
	cases := []struct {
		delta projection.Delta
		want  string
	}{
		{projection.PodDelete{Seq: 8, Key: "x"}, `{"kind":"pod_delete","seq":8,"key":"x"}`},
		{projection.ProjectDelete{Seq: 1, Key: "p"}, `{"kind":"project_delete","seq":1,"key":"p"}`},
		{projection.EnvironmentDelete{Seq: 1, Key: "e"}, `{"kind":"environment_delete","seq":1,"key":"e"}`},
		{projection.AppDelete{Seq: 1, Key: "ns/a"}, `{"kind":"app_delete","seq":1,"key":"ns/a"}`},
		{projection.ExposureChanged{Seq: 2, Key: "ns/a"}, `{"kind":"exposure_changed","seq":2,"key":"ns/a"}`},
		{projection.Resync{Seq: 3}, `{"kind":"resync","seq":3}`},
		{projection.PodUpsert{Seq: 4, Pod: &a}, `{"kind":"pod_upsert","seq":4,"pod":` + mustJSON(t, a) + `}`},
		{projection.ProjectUpsert{Seq: 5, Project: &projection.ProjectView{Name: "p"}}, `{"kind":"project_upsert","seq":5,"project":{"name":"p","uid":null,"display_name":"","description":null,"org":null,"environments":0,"ready":false,"deleting":false,"created_at":null}}`},
		{projection.EnvironmentUpsert{Seq: 6, Environment: &projection.EnvironmentView{Name: "e"}}, `{"kind":"environment_upsert","seq":6,"environment":` + mustJSON(t, projection.EnvironmentView{Name: "e"}) + `}`},
		{projection.AppUpsert{Seq: 7, App: &projection.AppView{Key: "ns/a"}}, `{"kind":"app_upsert","seq":7,"app":` + mustJSON(t, projection.AppView{Key: "ns/a"}) + `}`},
	}
	for _, c := range cases {
		if got := mustJSON(t, c.delta); got != c.want {
			t.Errorf("got  %s\nwant %s", got, c.want)
		}
		data, err := c.delta.MarshalJSON()
		if err != nil || string(data) != c.want {
			t.Errorf("MarshalJSON: %s %v", data, err)
		}
	}
}

// A subscriber that falls behind keeps at most DeltaCapacity deltas and is
// told how many it missed, instead of growing memory.
func TestASlowSubscriberIsToldToResync(t *testing.T) {
	p := projection.New()
	rx := p.Subscribe()
	defer rx.Close()
	for i := range projection.DeltaCapacity + 10 {
		p.UpsertPod(pod("a", i%2 == 0))
	}
	got, ok := rx.Recv(context.Background())
	if !ok || got.Delta != nil || got.Lagged != projection.DeltaCapacity+10 {
		t.Fatalf("got %+v", got)
	}
	if _, ok := rx.TryRecv(); ok {
		t.Fatal("the backlog was dropped")
	}
	p.UpsertPod(pod("b", true))
	if d, ok := next(t, rx).(projection.PodUpsert); !ok || d.Seq != projection.DeltaCapacity+11 {
		t.Fatalf("got %#v", d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := rx.Recv(ctx); ok {
		t.Fatal("a cancelled Recv ends")
	}
}
