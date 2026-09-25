package httpapi_test

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Teamtem-dev/kuben/internal/core/ops/build"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/doctor"
	"github.com/Teamtem-dev/kuben/internal/evidence"
	"github.com/Teamtem-dev/kuben/internal/httpapi"
	"github.com/Teamtem-dev/kuben/internal/kube/projection"
)

// routes/apps/evidence.rs deployments_fail_on_their_conditions.
func TestDeploymentsFailOnTheirConditions(t *testing.T) {
	failed := appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web-web"},
		Status: appsv1.DeploymentStatus{Conditions: []appsv1.DeploymentCondition{{
			Type: appsv1.DeploymentProgressing, Status: "False", Reason: "ProgressDeadlineExceeded",
		}}},
	}
	node := httpapi.DeploymentNode([]appsv1.Deployment{failed}, nil)
	if node.Status != doctor.StatusFail || len(node.Evidence) == 0 ||
		!strings.Contains(node.Evidence[0], "ProgressDeadlineExceeded") {
		t.Fatalf("a deadline exceeded: %+v", node)
	}
	if node.Evidence[0] != "web-web: Progressing False (ProgressDeadlineExceeded)" {
		t.Fatalf("fact: %q", node.Evidence[0])
	}
	if got := httpapi.DeploymentNode(nil, nil).Status; got != doctor.StatusFail {
		t.Fatalf("no Deployment: %s", got)
	}

	// An unavailable one without a reason, and a healthy one.
	unavailable := appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web-worker"},
		Status: appsv1.DeploymentStatus{Conditions: []appsv1.DeploymentCondition{{
			Type: appsv1.DeploymentAvailable, Status: "False",
		}}},
	}
	node = httpapi.DeploymentNode([]appsv1.Deployment{unavailable}, nil)
	if node.Status != doctor.StatusFail || node.Evidence[0] != "web-worker: Available False (-)" {
		t.Fatalf("unavailable: %+v", node)
	}
	fine := appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web-web"}}
	if node := httpapi.DeploymentNode([]appsv1.Deployment{fine}, nil); node.Status != doctor.StatusOK ||
		node.Subject != "1 Deployment(s)" || len(node.Evidence) != 0 {
		t.Fatalf("fine: %+v", node)
	}
}

// routes/apps/evidence.rs checks_fold_into_layers.
func TestChecksFoldIntoLayers(t *testing.T) {
	check := func(id string, status doctor.Status) doctor.Check {
		return doctor.Check{ID: id, Subject: "a.example.com", Status: status, Detail: "d", Hint: opt.Some("h")}
	}
	checks := []doctor.Check{
		check("dns", doctor.StatusOK),
		check("claim", doctor.StatusWarn),
		check("route", doctor.StatusOK),
	}
	dns, ok := httpapi.FromChecks(evidence.DNS, "DNS", checks, []string{"dns", "claim"})
	if !ok {
		t.Fatal("node")
	}
	if action, _ := dns.Action.Get(); dns.Status != doctor.StatusWarn || len(dns.Evidence) != 1 || action != "h" {
		t.Fatalf("dns: %+v", dns)
	}
	if dns.Evidence[0] != "a.example.com: d" {
		t.Fatalf("fact: %q", dns.Evidence[0])
	}
	if _, ok := httpapi.FromChecks(evidence.TLS, "TLS", checks, []string{"certificate"}); ok {
		t.Fatal("nothing checked")
	}
}

// pods_node: ready pods, a fatal reason, a pending pod.
func TestPodsNode(t *testing.T) {
	pod := func(name string, ready bool) *projection.PodView {
		return &projection.PodView{Name: name, Phase: projection.PodRunning, Ready: ready}
	}
	if node := httpapi.PodsNode(nil); node.Status != doctor.StatusFail || node.Subject != "no pods" {
		t.Fatalf("no pods: %+v", node)
	}
	if node := httpapi.PodsNode([]*projection.PodView{pod("a", true)}); node.Status != doctor.StatusOK ||
		node.Subject != "1 of 1 pods ready" || node.Action.IsSome() {
		t.Fatalf("ready: %+v", node)
	}
	pending := pod("b", false)
	pending.Phase = projection.PodPending
	node := httpapi.PodsNode([]*projection.PodView{pod("a", true), pending})
	if action, _ := node.Action.Get(); node.Status != doctor.StatusWarn || len(node.Evidence) != 1 ||
		node.Evidence[0] != "b: pending" || action != "Read the app's logs and events (app → Logs)." {
		t.Fatalf("pending: %+v", node)
	}
	crashing := pod("c", false)
	crashing.Reason = opt.Some("CrashLoopBackOff")
	crashing.Restarts = 4
	node = httpapi.PodsNode([]*projection.PodView{pod("a", true), crashing})
	if node.Status != doctor.StatusFail || node.Evidence[0] != "c: CrashLoopBackOff (4 restarts)" {
		t.Fatalf("crashing: %+v", node)
	}
	if node := httpapi.PodsNode([]*projection.PodView{pod("a", false)}); node.Status != doctor.StatusFail {
		t.Fatalf("none ready: %+v", node)
	}
}

// A build phase reads as Rust's derived Debug printed it.
func TestBuildPhasesReadAsRustDebug(t *testing.T) {
	for _, c := range []struct {
		phase build.Phase
		want  string
	}{
		{build.Failed, "Failed"}, {build.VerifyingOutput, "VerifyingOutput"}, {build.CancelRequested, "CancelRequested"},
	} {
		if got := httpapi.DebugName(c.phase); got != c.want {
			t.Fatalf("%s: %q, want %q", c.phase, got, c.want)
		}
	}
}
