package store_test

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/internal/store/pgtest"
)

// Ported from lifecycle.rs, plus the subject's wire form.

func lifecycleAudit(action string) store.NewAudit {
	return store.NewAudit{ActorKind: "user", ActorID: opt.Some("alice"), Action: action, Outcome: "accepted"}
}

func TestRequestsAreOperationsTheMaterializerClaims(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	o := org(t, s, "a", "A")
	tn := tenant(t, s, o)
	project := must[ids.ProjectID](t, "project")(tn.CreateProject(ctx, "shop", "Shop"))
	environment := must[ids.EnvironmentID](t, "environment")(tn.CreateEnvironmentTyped(ctx, project, "dev", "Dev",
		store.Standard, opt.None[any]()))
	requested := must[ids.OperationID](t, "request")(tn.Request(ctx, store.EnvironmentApply,
		store.EnvironmentSubject(project, environment), "user:alice", lifecycleAudit("createEnvironment")))
	commit(t, tn)

	c, ok := claim(t, s, "m", store.LifecycleKinds()...)
	if !ok || c.ID != requested || c.Kind != store.EnvironmentApply {
		t.Fatalf("claim: %+v, %v", c, ok)
	}
	subject, ok, err := s.LifecycleSubject(ctx, c)
	if err != nil || !ok {
		t.Fatalf("subject: %v, %v", ok, err)
	}
	if subject != store.EnvironmentSubject(project, environment) {
		t.Fatalf("subject: %+v", subject)
	}
}

func TestDeletionIsMarkedThenFinished(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	o := org(t, s, "a", "A")
	tn := tenant(t, s, o)
	project := must[ids.ProjectID](t, "project")(tn.CreateProject(ctx, "shop", "Shop"))
	environment := must[ids.EnvironmentID](t, "environment")(tn.CreateEnvironmentTyped(ctx, project, "dev", "Dev",
		store.Standard, opt.None[any]()))
	cluster := must[ids.ClusterID](t, "cluster")(tn.EnsureCluster(ctx, "primary"))
	placement := must[ids.PlacementID](t, "placement")(tn.CreatePlacement(ctx, project, environment, cluster, "kb-shop-dev"))
	web := must[ids.ApplicationID](t, "app")(tn.CreateApplication(ctx, project, "web", "Web"))
	tgt := must[ids.TargetID](t, "target")(tn.CreateTarget(ctx, project, web, placement))
	api := must[ids.ApplicationID](t, "app")(tn.CreateApplication(ctx, project, "api", "API"))
	must[ids.TargetID](t, "target")(tn.CreateTarget(ctx, project, api, placement))

	if !must[bool](t, "mark")(tn.MarkTargetDeleting(ctx, tgt)) {
		t.Fatal("mark")
	}
	if must[bool](t, "again")(tn.MarkTargetDeleting(ctx, tgt)) {
		t.Fatal("marked twice")
	}
	if !must[bool](t, "finish")(tn.FinishTargetDeletion(ctx, project, tgt)) {
		t.Fatal("finish")
	}
	if _, ok, err := tn.App(ctx, environment, "web"); err != nil || ok {
		t.Fatalf("web: %v, %v", ok, err)
	}
	if _, ok, err := tn.App(ctx, environment, "api"); err != nil || !ok {
		t.Fatalf("api: %v, %v", ok, err)
	}

	if !must[bool](t, "mark")(tn.MarkEnvironmentDeleting(ctx, environment)) {
		t.Fatal("mark the environment")
	}
	if n := must[uint64](t, "count")(tn.LiveEnvironments(ctx, project)); n != 1 {
		t.Fatalf("deleting is still live: %d", n)
	}
	if !must[bool](t, "finish")(tn.FinishEnvironmentDeletion(ctx, environment)) {
		t.Fatal("finish the environment")
	}
	if n := must[uint64](t, "count")(tn.LiveEnvironments(ctx, project)); n != 0 {
		t.Fatalf("live environments: %d", n)
	}
	if _, ok, err := tn.Environment(ctx, project, "dev"); err != nil || ok {
		t.Fatalf("dev: %v, %v", ok, err)
	}

	if !must[bool](t, "mark")(tn.MarkProjectDeleting(ctx, project)) {
		t.Fatal("mark the project")
	}
	if !must[bool](t, "finish")(tn.FinishProjectDeletion(ctx, project)) {
		t.Fatal("finish the project")
	}
	if _, ok, err := tn.Project(ctx, "shop"); err != nil || ok {
		t.Fatalf("shop: %v, %v", ok, err)
	}

	// Everything's slug is free again.
	again := must[ids.ProjectID](t, "project again")(tn.CreateProject(ctx, "shop", "Shop"))
	env := must[ids.EnvironmentID](t, "environment again")(tn.CreateEnvironmentTyped(ctx, again, "dev", "Dev",
		store.Standard, opt.None[any]()))
	must[ids.PlacementID](t, "the namespace is free again")(tn.CreatePlacement(ctx, again, env, cluster, "kb-shop-dev"))
	commit(t, tn)
}

func TestSubjectsKeepTheRustWireForm(t *testing.T) {
	project := ids.New[ids.Project]()
	environment := ids.New[ids.Environment]()
	tgt := ids.New[ids.Target]()
	for _, c := range []struct {
		subject store.Subject
		want    string
	}{
		{store.ProjectSubject(project), `{"project":"` + project.String() + `","delete_volumes":false}`},
		{store.TargetSubject(project, environment, tgt, true), `{"project":"` + project.String() +
			`","environment":"` + environment.String() + `","target":"` + tgt.String() + `","delete_volumes":true}`},
		{store.DetachSubject(project, environment, tgt), `{"project":"` + project.String() +
			`","environment":"` + environment.String() + `","target":"` + tgt.String() +
			`","delete_volumes":false,"detach":true}`},
	} {
		text, err := json.Marshal(c.subject)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if diff := cmp.Diff(c.want, string(text)); diff != "" {
			t.Fatalf("wire form (-want +got):\n%s", diff)
		}
		var back store.Subject
		if err := json.Unmarshal(text, &back); err != nil || back != c.subject {
			t.Fatalf("round trip: %+v, %v", back, err)
		}
	}
	for _, text := range []string{
		`{}`,
		`{"project":null}`,
		`{"project":"` + project.String() + `","delete_volumes":null}`,
		`{"project":"` + project.String() + `","target":"nope"}`,
	} {
		var s store.Subject
		if err := json.Unmarshal([]byte(text), &s); err == nil {
			t.Fatalf("serde refused %s", text)
		}
	}
	var lenient store.Subject
	if err := json.Unmarshal([]byte(`{"project":"`+project.String()+`","environment":null,"extra":1}`), &lenient); err != nil ||
		lenient != store.ProjectSubject(project) {
		t.Fatalf("absent and null are the same, unknown members are ignored: %+v, %v", lenient, err)
	}
}
