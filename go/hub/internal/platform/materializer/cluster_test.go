package materializer_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/artifact"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ops/run"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ops/target"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/kubetest"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/materializer"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/registry"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/render"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store/pgtest"
	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

// The materializer against a real API server and PostgreSQL (ADR-032;
// crates/kuben-platform/tests/materializer.rs). Each test works in a project
// and namespace of its own, and in a database schema of its own. No
// controller runs: the tests create the environment's namespace and play
// the App controller.

var apiserver kubetest.Server

func TestMain(m *testing.M) { os.Exit(kubetest.Main(m, &apiserver)) }

const (
	clusterDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	repository    = "registry.example.com/acme/web"
	// instantly is a verification deadline that has passed at once (a zero
	// deadline means the default).
	instantly = time.Nanosecond
)

func gvr(plural string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: v1alpha1.SchemeGroupVersion.Group, Version: v1alpha1.SchemeGroupVersion.Version, Resource: plural}
}

// world is one organization with project `m<random>`, environment `prod`
// and app `web`, a release and a configuration revision, in SQL; the
// environment's namespace in the cluster.
type world struct {
	t            *testing.T
	store        *store.Store
	kube         kubetest.Clients
	cluster      registry.Cluster
	org          ids.OrgID
	project      ids.ProjectID
	environment  ids.EnvironmentID
	target       ids.TargetID
	lifecycleUID uuid.UUID
	release      ids.ReleaseID
	revision     ids.ConfigRevisionID
	slug         string
	namespace    string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	kube := apiserver.Connect(t)
	st := pgtest.Store(t)
	ctx := t.Context()
	id := strings.ReplaceAll(uuid.NewString(), "-", "")
	slug := "m" + id[len(id)-10:]
	w := &world{t: t, store: st, kube: kube, slug: slug, namespace: "kb-" + slug + "-prod"}
	cluster, err := registry.NewCluster(registry.Primary, kube.Config)
	must(t, err)
	w.cluster = cluster
	org, err := st.CreateOrg(ctx, slug, slug)
	must(t, err)
	w.org = org.ID
	tn := w.tenant()
	w.project = get[ids.ProjectID](t)(tn.CreateProject(ctx, slug, "Materialized"))
	w.environment = get[ids.EnvironmentID](t)(tn.CreateEnvironment(ctx, w.project, "prod", "Prod", false))
	clusterID := get[ids.ClusterID](t)(tn.CreateCluster(ctx, "kind"))
	placement := get[ids.PlacementID](t)(tn.CreatePlacement(ctx, w.project, w.environment, clusterID, w.namespace))
	application := get[ids.ApplicationID](t)(tn.CreateApplication(ctx, w.project, "web", "Web"))
	w.target = get[ids.TargetID](t)(tn.CreateTarget(ctx, w.project, application, placement))
	digest, err := artifact.ParseDigest(clusterDigest)
	must(t, err)
	release, _, err := tn.CreateRelease(ctx, w.project, store.PortableRelease{
		Application: application, Artifacts: map[string]artifact.Digest{"web": digest},
		ProcessContract: map[string]any{}, PortableConfig: map[string]any{}, RendererSchema: 1,
		Source: opt.Some[any](map[string]any{"image_repository": repository}), CreatedBy: "user:test",
	})
	must(t, err)
	w.release = release
	config := map[string]any{"runtime": map[string]any{"processes": map[string]any{"web": map[string]any{"port": 8080}}}}
	revision, ok, err := tn.CreateConfigRevision(ctx, w.project, w.target, config, "user:test")
	if err != nil || !ok {
		t.Fatalf("revision: %v %v", ok, err)
	}
	w.revision = revision.ID
	state, ok, err := tn.TargetState(ctx, w.target)
	if err != nil || !ok {
		t.Fatalf("target: %v %v", ok, err)
	}
	w.lifecycleUID = state.LifecycleUID
	must(t, tn.Commit(ctx))
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: w.namespace}}
	_, err = kube.Typed.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	must(t, err)
	return w
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// get unwraps (value, error).
func get[T any](t *testing.T) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		must(t, err)
		return v
	}
}

// tenant is a transaction of the world's organization; the caller commits
// it or leaves it to be rolled back when the test ends.
func (w *world) tenant() *store.Tenant {
	w.t.Helper()
	tn, err := w.store.Tenant(w.t.Context(), w.org)
	if err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(func() { _ = tn.Rollback(context.Background()) }) //nolint:errcheck // committed or read only
	return tn
}

// read runs a read in a transaction of its own, closed at once.
func read[T any](w *world, f func(*store.Tenant) (T, bool, error)) (T, bool) {
	w.t.Helper()
	tn, err := w.store.Tenant(w.t.Context(), w.org)
	if err != nil {
		w.t.Fatal(err)
	}
	defer tn.Rollback(context.Background()) //nolint:errcheck // read only
	v, ok, err := f(tn)
	must(w.t, err)
	return v, ok
}

// deploy accepts a deployment that expects generation expected.
func (w *world) deploy(expected uint64) (ids.OperationID, ids.DeploymentRunID) {
	w.t.Helper()
	tn := w.tenant()
	started, err := tn.StartDeployment(w.t.Context(), store.StartDeployment{
		Project: w.project, Target: w.target, Release: w.release, ConfigRevision: w.revision,
		ExpectedGeneration: target.Generation(expected), LifecycleUID: w.lifecycleUID,
		Reason: store.ReasonDeploy, RequestedBy: "user:test", InputHash: fmt.Appendf(nil, "deploy-%d", expected),
	}, store.NewAudit{ActorKind: "user", Action: "startDeployment", Outcome: "accepted"}, opt.None[store.IdempotencyKey]())
	must(w.t, err)
	must(w.t, tn.Commit(w.t.Context()))
	accepted, ok := started.(store.StartedAccepted)
	if !ok {
		w.t.Fatalf("not accepted: %#v", started)
	}
	return accepted.Operation, accepted.Run
}

func (w *world) phase(r ids.DeploymentRunID) run.Phase {
	w.t.Helper()
	state, ok := read(w, func(tn *store.Tenant) (store.RunState, bool, error) { return tn.RunPhase(w.t.Context(), r) })
	if !ok {
		w.t.Fatal("no such run")
	}
	return state.Phase
}

func (w *world) apps() dynamic.ResourceInterface {
	return w.kube.Dynamic.Resource(gvr(v1alpha1.AppResource)).Namespace(w.namespace)
}

func (w *world) worker(verify, deletion time.Duration) *materializer.Worker {
	return materializer.New(materializer.Deps{
		Store: w.store, Cluster: w.cluster, ID: "materializer-test", VerifyDeadline: verify, DeletionCheck: deletion,
	})
}

// workOnce runs one claim and returns the operation it worked on.
func (w *world) workOnce(worker *materializer.Worker) opt.Val[ids.OperationID] {
	w.t.Helper()
	worked, err := worker.WorkOnce(w.t.Context())
	must(w.t, err)
	return worked
}

func managedMeta(name string, org ids.OrgID) map[string]any {
	return map[string]any{"name": name, "labels": map[string]any{
		v1alpha1.LabelManagedBy: v1alpha1.LabelManagerValue, v1alpha1.LabelOrg: org.String(),
	}}
}

// app reads the App `web`; false when it does not exist.
func (w *world) app() (*v1alpha1.App, bool) {
	w.t.Helper()
	live, err := w.apps().Get(w.t.Context(), "web", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	must(w.t, err)
	data, err := live.MarshalJSON()
	must(w.t, err)
	var app v1alpha1.App
	must(w.t, json.Unmarshal(data, &app))
	return &app, true
}

// liveApp is the App written in the cluster; the test fails without one.
func (w *world) liveApp() *v1alpha1.App {
	w.t.Helper()
	app, ok := w.app()
	if !ok || app == nil {
		w.t.Fatal("no App was written")
	}
	return app
}

func (w *world) waitForApp() *v1alpha1.App {
	w.t.Helper()
	for range 100 {
		if app, ok := w.app(); ok {
			return app
		}
		time.Sleep(300 * time.Millisecond)
	}
	w.t.Fatal("the App was never written")
	return nil
}

// reportReady plays the App controller: the written generation is observed
// and ready.
func (w *world) reportReady(app *v1alpha1.App) {
	w.t.Helper()
	g := app.Generation
	status, err := json.Marshal(map[string]any{
		"apiVersion": v1alpha1.SchemeGroupVersion.String(), "kind": v1alpha1.AppKind,
		"status": map[string]any{"observedGeneration": g, "conditions": []any{map[string]any{
			"type": "Ready", "status": "True", "reason": "Available", "observedGeneration": g,
			"lastTransitionTime": "2026-01-01T00:00:00Z", "message": "",
		}}},
	})
	must(w.t, err)
	force := true
	_, err = w.apps().Patch(w.t.Context(), "web", types.ApplyPatchType, status,
		metav1.PatchOptions{FieldManager: "test-controller", Force: &force}, "status")
	must(w.t, err)
}

// workAsync runs one claim on another goroutine; the result arrives on the
// channel.
func (w *world) workAsync(worker *materializer.Worker) <-chan opt.Val[ids.OperationID] {
	done := make(chan opt.Val[ids.OperationID], 1)
	go func() { //nolint:forbidigo // a test's helper, joined through done
		worked, err := worker.WorkOnce(w.t.Context())
		if err != nil {
			w.t.Error(err)
		}
		done <- worked
	}()
	return done
}

func generation(app *v1alpha1.App) string { return app.Annotations[v1alpha1.AnnotationGeneration] }

func image(app *v1alpha1.App) string {
	if app.Spec.Source.Image == nil {
		return ""
	}
	return *app.Spec.Source.Image
}

func TestARunIsWrittenVerifiedAndRecorded(t *testing.T) {
	w := newWorld(t)
	operation, runID := w.deploy(0)
	done := w.workAsync(w.worker(2*time.Minute, 0))
	app := w.waitForApp()
	if generation(app) != "1" || app.Annotations[v1alpha1.AnnotationOperation] != operation.String() ||
		image(app) != repository+"@"+clusterDigest {
		t.Fatalf("the written App: %v %s", app.Annotations, image(app))
	}
	w.reportReady(app)
	if got := <-done; got != opt.Some(operation) {
		t.Fatalf("worked on %v", got)
	}
	if p := w.phase(runID); p != run.Succeeded {
		t.Fatalf("phase %s", p)
	}
	written, ok := read(w, func(tn *store.Tenant) (store.Materialized, bool, error) {
		return tn.Materialized(t.Context(), w.target)
	})
	if !ok {
		t.Fatal("not recorded")
	}
	plan, ok := read(w, func(tn *store.Tenant) (store.RunPlan, bool, error) { return tn.RunRenderPlan(t.Context(), runID) })
	if !ok {
		t.Fatal("no frozen plan")
	}
	if written.Generation != 1 || plan.RendererVersion != render.RendererVersion || written.ResourceUID != string(app.UID) {
		t.Fatalf("record %+v, plan %s", written, plan.RendererVersion)
	}
	objects, _ := plan.Resources.([]any)
	if !slices.ContainsFunc(objects, func(o any) bool {
		m, _ := o.(map[string]any)
		meta, _ := m["metadata"].(map[string]any)
		return m["kind"] == "Deployment" && meta["name"] == "web-web"
	}) {
		t.Fatalf("the plan holds the web Deployment: %v", plan.Resources)
	}
	env, err := w.kube.Dynamic.Resource(gvr(v1alpha1.EnvironmentResource)).Get(t.Context(), w.slug+"-prod", metav1.GetOptions{})
	must(t, err)
	if env.GetLabels()[v1alpha1.LabelOrg] != w.org.String() || len(env.GetOwnerReferences()) == 0 || env.GetOwnerReferences()[0].Kind != "Project" {
		t.Fatalf("environment: %v %v", env.GetLabels(), env.GetOwnerReferences())
	}
}

func TestASupersededRunNeverWritesAndAForgedGenerationIsReplaced(t *testing.T) {
	w := newWorld(t)
	older, olderRun := w.deploy(0)
	newer, newerRun := w.deploy(1)
	// Someone else writes the App and claims generation 99 on it.
	meta := managedMeta("web", w.org)
	meta["annotations"] = map[string]any{v1alpha1.AnnotationGeneration: "99"}
	forged := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.SchemeGroupVersion.String(), "kind": v1alpha1.AppKind, "metadata": meta,
		"spec": map[string]any{
			"source":  map[string]any{"image": "evil.example.com/web:latest"},
			"runtime": map[string]any{"processes": map[string]any{"web": map[string]any{"port": 80}}},
		},
	}}
	_, err := w.apps().Create(t.Context(), forged, metav1.CreateOptions{})
	must(t, err)
	// No controller answers: verification ends at once.
	worker := w.worker(instantly, 0)
	if got := w.workOnce(worker); got != opt.Some(older) {
		t.Fatalf("the older operation is due first, got %v", got)
	}
	if p := w.phase(olderRun); p != run.Superseded {
		t.Fatalf("older: %s", p)
	}
	if live := w.liveApp(); generation(live) != "99" {
		t.Fatalf("a superseded run writes nothing: %s", generation(live))
	}
	if got := w.workOnce(worker); got != opt.Some(newer) {
		t.Fatalf("then the newer, got %v", got)
	}
	live := w.liveApp()
	if generation(live) != "2" || image(live) != repository+"@"+clusterDigest {
		t.Fatalf("the forged value is replaced: %s %s", generation(live), image(live))
	}
	if p := w.phase(newerRun); p != run.Failed {
		t.Fatalf("nobody verified it in time: %s", p)
	}
}

func TestANameAnotherOrganizationHoldsIsNeverTaken(t *testing.T) {
	w := newWorld(t)
	projects := w.kube.Dynamic.Resource(gvr(v1alpha1.ProjectResource))
	theirs := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.SchemeGroupVersion.String(), "kind": v1alpha1.ProjectKind,
		"metadata": managedMeta(w.slug, ids.New[ids.Org]()), "spec": map[string]any{"displayName": "Theirs"},
	}}
	_, err := projects.Create(t.Context(), theirs, metav1.CreateOptions{})
	must(t, err)
	operation, runID := w.deploy(0)
	if got := w.workOnce(w.worker(0, 0)); got != opt.Some(operation) {
		t.Fatalf("worked on %v", got)
	}
	if p := w.phase(runID); p != run.Failed {
		t.Fatalf("phase %s", p)
	}
	if _, ok := w.app(); ok {
		t.Fatal("nothing was written")
	}
	live, err := projects.Get(t.Context(), w.slug, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if name, _, _ := unstructured.NestedString(live.Object, "spec", "displayName"); name != "Theirs" {
		t.Fatalf("their project: %s", name)
	}
}

func TestDriftIsRecordedAndReplaced(t *testing.T) {
	w := newWorld(t)
	operation, _ := w.deploy(0)
	worker := w.worker(2*time.Minute, 0)
	done := w.workAsync(worker)
	app := w.waitForApp()
	w.reportReady(app)
	if got := <-done; got != opt.Some(operation) {
		t.Fatalf("worked on %v", got)
	}
	app = w.liveApp()
	if f, err := worker.CheckDrift(t.Context(), app); err != nil || f != (materializer.Clean{}) {
		t.Fatalf("a status update is not drift: %#v %v", f, err)
	}

	// Someone edits the image with kubectl.
	body := []byte(`{"spec":{"source":{"image":"evil.example.com/web:latest"}}}`)
	_, err := w.apps().Patch(t.Context(), "web", types.MergePatchType, body, metav1.PatchOptions{FieldManager: "kubectl-edit"})
	must(t, err)
	edited := w.liveApp()
	f, err := worker.CheckDrift(t.Context(), edited)
	must(t, err)
	d, ok := f.(materializer.Drift)
	if !ok || !d.SpecChanged || d.Deleted || !slices.Equal(d.Managers, []string{"kubectl-edit"}) {
		t.Fatalf("an edit is drift: %#v", f)
	}
	replaced := w.liveApp()
	if image(replaced) != repository+"@"+clusterDigest {
		t.Fatalf("SQL's rendering is written again: %s", image(replaced))
	}
	if f, err := worker.CheckDrift(t.Context(), replaced); err != nil || f != (materializer.Clean{}) {
		t.Fatalf("the replacement is not drift: %#v %v", f, err)
	}

	// Someone deletes it: it is written again, as a new object.
	must(t, w.apps().Delete(t.Context(), "web", metav1.DeleteOptions{}))
	for range 50 {
		if _, ok := w.app(); !ok {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	f, err = worker.CheckDrift(t.Context(), replaced)
	must(t, err)
	if d, ok := f.(materializer.Drift); !ok || !d.Deleted {
		t.Fatalf("a deletion is drift: %#v", f)
	}
	recreated, ok := w.app()
	if !ok || recreated.UID == replaced.UID {
		t.Fatalf("written again as a new object: %v", ok)
	}
	record, ok := read(w, func(tn *store.Tenant) (store.Materialized, bool, error) {
		return tn.Materialized(t.Context(), w.target)
	})
	if !ok {
		t.Fatal("not recorded")
	}
	described, _ := record.Drift.Get()
	deleted, _ := described.(map[string]any)["deleted"].(bool)
	if record.DriftCount != 2 || record.ResourceUID != string(recreated.UID) ||
		record.ResourceGeneration != recreated.Generation || !deleted {
		t.Fatalf("record %+v", record)
	}
}

// request records kind on subject as the API does: a deletion marks its
// subject deleting in the same transaction.
func (w *world) request(kind string, subject store.Subject) ids.OperationID {
	w.t.Helper()
	ctx := w.t.Context()
	tn := w.tenant()
	marked := true
	var err error
	switch kind {
	case store.TargetDelete:
		marked, err = tn.MarkTargetDeleting(ctx, w.target)
	case store.EnvironmentDelete:
		marked, err = tn.MarkEnvironmentDeleting(ctx, w.environment)
	case store.ProjectDelete:
		marked, err = tn.MarkProjectDeleting(ctx, w.project)
	}
	if err != nil || !marked {
		w.t.Fatalf("%s: marked deleting once: %v %v", kind, marked, err)
	}
	operation, err := tn.Request(ctx, kind, subject, "user:test", store.NewAudit{ActorKind: "user", Action: kind, Outcome: "accepted"})
	must(w.t, err)
	must(w.t, tn.Commit(ctx))
	return operation
}

// retainedVolume is a volume of the app `web` that its deletion keeps
// unless asked.
func retainedVolume() *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web-data", Labels: map[string]string{v1alpha1.LabelApp: "web"},
			Annotations: map[string]string{render.Retain: "true"},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse("1Mi"),
			}},
		},
	}
}

func TestLifecycleOperationsWriteAndRemoveResources(t *testing.T) {
	w := newWorld(t)
	worker := w.worker(2*time.Minute, instantly)
	ctx := t.Context()
	environments := w.kube.Dynamic.Resource(gvr(v1alpha1.EnvironmentResource))
	projects := w.kube.Dynamic.Resource(gvr(v1alpha1.ProjectResource))
	envName := w.slug + "-prod"

	// An environment is written as soon as it exists.
	apply := w.request(store.EnvironmentApply, store.EnvironmentSubject(w.project, w.environment))
	if got := w.workOnce(worker); got != opt.Some(apply) {
		t.Fatalf("worked on %v", got)
	}
	env, err := environments.Get(ctx, envName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if p, _, _ := unstructured.NestedString(env.Object, "spec", "project"); p != w.slug || env.GetAnnotations()[v1alpha1.AnnotationOperation] != apply.String() {
		t.Fatalf("environment: %v %v", env.Object["spec"], env.GetAnnotations())
	}
	_, err = projects.Get(ctx, w.slug, metav1.GetOptions{})
	must(t, err)

	// An app is deployed, then deleted with the volume it would keep.
	deployed, _ := w.deploy(0)
	done := w.workAsync(worker)
	w.reportReady(w.waitForApp())
	if got := <-done; got != opt.Some(deployed) {
		t.Fatalf("worked on %v", got)
	}
	claims := w.kube.Typed.CoreV1().PersistentVolumeClaims(w.namespace)
	_, err = claims.Create(ctx, retainedVolume(), metav1.CreateOptions{})
	must(t, err)
	del := w.request(store.TargetDelete, store.TargetSubject(w.project, w.environment, w.target, true))
	if got := w.workOnce(worker); got != opt.Some(del) {
		t.Fatalf("worked on %v", got)
	}
	if _, ok := w.app(); ok {
		t.Fatal("the app is gone")
	}
	if pvc, err := claims.Get(ctx, "web-data", metav1.GetOptions{}); err == nil && pvc.DeletionTimestamp == nil {
		t.Fatal("the volume is deleted, as asked")
	}
	if _, ok := read(w, func(tn *store.Tenant) (store.AppRecord, bool, error) { return tn.App(ctx, w.environment, "web") }); ok {
		t.Fatal("the app's rows are left")
	}

	// The environment: its object goes first, then its rows.
	w.request(store.EnvironmentDelete, store.EnvironmentSubject(w.project, w.environment))
	for round := 0; ; round++ {
		if _, ok := read(w, func(tn *store.Tenant) (store.EnvironmentRecord, bool, error) {
			return tn.Environment(ctx, w.project, "prod")
		}); !ok {
			break
		}
		if round >= 20 {
			t.Fatal("the environment deletion never finished")
		}
		w.workOnce(worker)
		time.Sleep(200 * time.Millisecond)
	}
	if _, err := environments.Get(ctx, envName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the environment object: %v", err)
	}

	// The project, now empty.
	del = w.request(store.ProjectDelete, store.ProjectSubject(w.project))
	if got := w.workOnce(worker); got != opt.Some(del) {
		t.Fatalf("worked on %v", got)
	}
	if _, ok := read(w, func(tn *store.Tenant) (store.Project, bool, error) { return tn.Project(ctx, w.slug) }); ok {
		t.Fatal("the project's rows are left")
	}
	if p, err := projects.Get(ctx, w.slug, metav1.GetOptions{}); err == nil && p.GetDeletionTimestamp() == nil {
		t.Fatal("the project object is deleted")
	}
}
