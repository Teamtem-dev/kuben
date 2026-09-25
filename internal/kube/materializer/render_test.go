package materializer_test

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/api/v1alpha1"
	"github.com/Teamtem-dev/kuben/internal/core/artifact"
	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/ops/run"
	"github.com/Teamtem-dev/kuben/internal/core/ops/target"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/jsonx"
	"github.com/Teamtem-dev/kuben/internal/kube/materializer"
	"github.com/Teamtem-dev/kuben/internal/kube/render"
	"github.com/Teamtem-dev/kuben/internal/store"
)

const digestText = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func digest(t *testing.T) artifact.Digest {
	t.Helper()
	d, err := artifact.ParseDigest(digestText)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func config(t *testing.T, text string) any {
	t.Helper()
	v, err := jsonx.DecodeAny([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func sample(t *testing.T) store.Materialization {
	t.Helper()
	return store.Materialization{
		Org: ids.New[ids.Org](), Run: ids.New[ids.DeploymentRun](), Operation: ids.New[ids.Operation](),
		Phase: run.Planned, Generation: target.Generation(3), LifecycleUID: uuid.Must(uuid.NewV7()),
		Project: ids.New[ids.Project](), ProjectSlug: "shop", ProjectName: "Shop",
		ProjectDescription: opt.Some("The online shop"),
		Environment:        ids.New[ids.Environment](), EnvironmentSlug: "production", EnvironmentName: "Production",
		Protected: true, EnvType: "production", Namespace: "kb-shop-production",
		Cluster: ids.New[ids.Cluster](), Delivery: store.DeliveryController,
		Application: ids.New[ids.Application](), ApplicationSlug: "web", ApplicationName: "Web",
		Target: ids.New[ids.Target](), DesiredGeneration: target.Generation(3),
		Release:              ids.New[ids.Release](),
		Artifacts:            map[string]artifact.Digest{"web": digest(t)},
		ImageRepository:      opt.Some("ghcr.io/acme/web"),
		ConfigRevision:       ids.New[ids.ConfigRevision](),
		ConfigRevisionNumber: 2,
		Config: config(t, `{"runtime":{"processes":{"web":{"port":8080}}},`+
			`"env":[{"name":"LOG_LEVEL","value":"info"}]}`),
	}
}

func withConfig(t *testing.T, text string) store.Materialization {
	t.Helper()
	m := sample(t)
	m.Config = config(t, text)
	return m
}

func mustRender(t *testing.T, m store.Materialization) materializer.Rendered {
	t.Helper()
	r, err := materializer.Render(&m)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestBoundSecretsAreReadFromTheirRevisionObjects(t *testing.T) {
	m := withConfig(t, `{
		"runtime": { "processes": { "web": { "port": 8080 } } },
		"imagePullSecrets": ["anything-the-config-says"],
		"env": [
			{ "name": "DATABASE_URL", "fromSecret": { "name": "db", "key": "url" } },
			{ "name": "TOKEN", "fromSecret": { "name": "legacy", "key": "token" } }
		]
	}`)
	m.Secrets = []store.SecretBinding{
		{Name: "db", Secret: uuid.Must(uuid.NewV7()), Revision: 4},
		{Name: "ghcr", Secret: uuid.Must(uuid.NewV7()), Revision: 2, Registry: opt.Some("ghcr.io")},
	}
	app := mustRender(t, m).App
	var names [][2]string
	for _, e := range app.Spec.Env {
		if e.FromSecret != nil {
			names = append(names, [2]string{e.FromSecret.Name, e.FromSecret.Key})
		}
	}
	if diff := cmp.Diff([][2]string{{"db.r4", "url"}, {"legacy", "token"}}, names); diff != "" {
		t.Fatal(diff)
	}
	if diff := cmp.Diff([]string{"ghcr.r2"}, app.Spec.ImagePullSecrets); diff != "" {
		t.Fatalf("bound logins only: %s", diff)
	}
	if plain := mustRender(t, sample(t)).App; len(plain.Spec.ImagePullSecrets) != 0 {
		t.Fatal(plain.Spec.ImagePullSecrets)
	}
}

func TestARestartRunStampsTheApp(t *testing.T) {
	if _, ok := mustRender(t, sample(t)).App.Annotations[render.RestartedAt]; ok {
		t.Fatal("no restart")
	}
	m := sample(t)
	m.RestartedAt = opt.Some[int64](1_757_937_600_000)
	if got := mustRender(t, m).App.Annotations[render.RestartedAt]; got != "2025-09-15T12:00:00Z" {
		t.Fatal(got)
	}
}

func TestRendersTheObjectsTheControllersExpect(t *testing.T) {
	m := sample(t)
	r := mustRender(t, m)
	if r.Project.Name != "shop" || r.Project.Spec.DisplayName != "Shop" ||
		r.Project.Spec.Description == nil || *r.Project.Spec.Description != "The online shop" {
		t.Fatalf("project: %+v", r.Project)
	}
	if r.Environment.Name != "shop-production" || r.Environment.Spec.Project != "shop" ||
		r.Environment.Spec.Type != v1alpha1.EnvironmentTypeProduction || r.Environment.Spec.Protection == nil {
		t.Fatalf("environment: %+v", r.Environment)
	}
	if r.App.Name != "web" || r.App.Namespace != "kb-shop-production" {
		t.Fatalf("app: %+v", r.App.ObjectMeta)
	}
	wantLabels := map[string]string{
		v1alpha1.LabelManagedBy: v1alpha1.LabelManagerValue, v1alpha1.LabelOrg: m.Org.String(),
		v1alpha1.LabelProject: "shop", v1alpha1.LabelEnvironment: "shop-production",
	}
	if diff := cmp.Diff(wantLabels, r.App.Labels); diff != "" {
		t.Fatal(diff)
	}
	wantNotes := map[string]string{
		v1alpha1.AnnotationGeneration: "3", v1alpha1.AnnotationOperation: m.Operation.String(),
		v1alpha1.AnnotationLifecycleUID: m.LifecycleUID.String(), v1alpha1.AnnotationID: m.Target.String(),
	}
	if diff := cmp.Diff(wantNotes, r.App.Annotations); diff != "" {
		t.Fatal(diff)
	}
	for _, notes := range []map[string]string{r.Project.Annotations, r.Environment.Annotations} {
		if notes[v1alpha1.AnnotationOperation] != m.Operation.String() {
			t.Fatal(notes)
		}
		if _, ok := notes[v1alpha1.AnnotationGeneration]; ok {
			t.Fatal("generations belong to targets")
		}
	}
	if image := r.App.Spec.Source.Image; image == nil || *image != "ghcr.io/acme/web@"+digestText || r.App.Spec.Source.Git != nil {
		t.Fatalf("source: %+v", r.App.Spec.Source)
	}
	if port := r.App.Spec.Runtime.Processes["web"].Port; port == nil || *port != 8080 {
		t.Fatal("port")
	}
	if v := r.App.Spec.Env[0].Value; v == nil || *v != "info" {
		t.Fatal("env")
	}
}

func TestTheReleaseDecidesTheImage(t *testing.T) {
	m := withConfig(t, `{"source":{"git":{"repo":"https://github.com/acme/web"}},"runtime":{"processes":{"worker":{}}}}`)
	r := mustRender(t, m)
	if image := r.App.Spec.Source.Image; image == nil || *image != "ghcr.io/acme/web@"+digestText || r.App.Spec.Source.Git != nil {
		t.Fatalf("source: %+v", r.App.Spec.Source)
	}
	only := sample(t)
	only.Artifacts = map[string]artifact.Digest{"api": digest(t)}
	if _, err := materializer.Render(&only); err != nil {
		t.Fatalf("a release with one artifact names its image: %v", err)
	}
}

func TestEnvironmentsCarryTheirTypeAndQuota(t *testing.T) {
	m := sample(t)
	project := materializer.ProjectMaterialOf(&m)
	operation := ids.New[ids.Operation]()
	preview := materializer.EnvironmentMaterialOf(&m)
	preview.EnvType, preview.Namespace = "preview", opt.None[string]()
	preview.Quota = opt.Some(config(t, `{"cpu":"4","memory":"4Gi","pods":30}`))
	env, err := materializer.EnvironmentObject(project, preview, operation)
	if err != nil {
		t.Fatal(err)
	}
	if env.Spec.Type != v1alpha1.EnvironmentTypePreview || env.Spec.Protection != nil {
		t.Fatalf("%+v", env.Spec)
	}
	q := env.Spec.Quota
	if q == nil || q.CPU == nil || *q.CPU != "4" || q.Memory == nil || *q.Memory != "4Gi" || q.Pods == nil || *q.Pods != 30 {
		t.Fatalf("quota: %+v", q)
	}
	if env.Annotations[v1alpha1.AnnotationOperation] != operation.String() {
		t.Fatal(env.Annotations)
	}

	standard := materializer.EnvironmentMaterialOf(&m)
	standard.EnvType = "standard"
	if env, err := materializer.EnvironmentObject(project, standard, operation); err != nil ||
		env.Spec.Type != v1alpha1.EnvironmentTypeStandard {
		t.Fatalf("%+v %v", env.Spec, err)
	}

	broken := materializer.EnvironmentMaterialOf(&m)
	broken.Quota = opt.Some(config(t, `{"pods":"many"}`))
	if _, err := materializer.EnvironmentObject(project, broken, operation); code(err) != "InvalidQuota" {
		t.Fatalf("quota: %v", err)
	}
}

func code(err *materializer.RenderError) string {
	if err == nil {
		return ""
	}
	return err.Code
}

func TestAnObjectThatCannotBeRenderedSaysWhy(t *testing.T) {
	noRepository := sample(t)
	noRepository.ImageRepository = opt.None[string]()
	noArtifact := sample(t)
	noArtifact.Artifacts = map[string]artifact.Digest{}
	elsewhere := sample(t)
	elsewhere.Namespace = "elsewhere"
	long := sample(t)
	long.EnvironmentSlug = strings.Repeat("x", 60)
	cases := []struct {
		m    store.Materialization
		code string
	}{
		{noRepository, "NoImageRepository"},
		{noArtifact, "NoArtifact"},
		{withConfig(t, `[]`), "InvalidConfig"},
		{withConfig(t, `{"env":[]}`), "InvalidConfig"},
		{withConfig(t, `{"runtime":{"processes":{}}}`), "NoProcesses"},
		{elsewhere, "PlacementNamespace"},
		{long, "NameTooLong"},
	}
	for _, c := range cases {
		_, err := materializer.Render(&c.m)
		if code(err) != c.code {
			t.Errorf("%s: got %v", c.code, err)
		}
	}
}

func TestEnvironmentsAreOwnedByTheirProject(t *testing.T) {
	r := mustRender(t, sample(t))
	environment := r.Environment
	if materializer.SetOwner(&environment, &r.Project) {
		t.Fatal("no UID before the project is applied")
	}
	project := r.Project
	project.UID = "uid-1"
	if !materializer.SetOwner(&environment, &project) {
		t.Fatal("owned")
	}
	owners := environment.OwnerReferences
	if len(owners) != 1 || owners[0].Kind != "Project" || owners[0].UID != "uid-1" {
		t.Fatalf("%+v", owners)
	}
}
