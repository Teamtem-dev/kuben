package runtime_test

import (
	"encoding/json"
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Teamtem-dev/kuben/api/v1alpha1"
	"github.com/Teamtem-dev/kuben/internal/agent/runtime"
	"github.com/Teamtem-dev/kuben/internal/agentlink/protocol"
)

// Ported from crates/kuben-agent/src/runtime.rs.

func applyOf(t *testing.T, resources string, digest string) protocol.Apply {
	t.Helper()
	if digest == "" {
		digest = protocol.HexDigest([]byte(resources))
	}
	spec, err := json.Marshal(v1alpha1.ApplicationRuntimeSpec{
		TargetID: "t", LifecycleUID: "l", ControlEpoch: 0, Generation: 3,
		InputHash: protocol.HexDigest([]byte("input")), ReleaseID: "r",
		Plan: v1alpha1.PlanEnvelope{ID: "p", RendererVersion: "kuben-renderer/1", Digest: digest, Resources: resources},
	})
	if err != nil {
		t.Fatal(err)
	}
	return protocol.Apply{Target: "t", Namespace: "kb-shop-prod", Name: "web", Spec: string(spec)}
}

const deployment = `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web-web","namespace":"kb-shop-prod"}}`

func refusal(t *testing.T, apply protocol.Apply) string {
	t.Helper()
	_, err := runtime.Check(apply)
	refused, ok := errors.AsType[runtime.Refused](err)
	if !ok {
		t.Fatalf("not refused: %v", err)
	}
	return refused.Reason
}

func TestAPlanOfAllowedKindsInItsNamespacePasses(t *testing.T) {
	plan := `[` + deployment + `,{"apiVersion":"v1","kind":"Service","metadata":{"name":"web"}}]`
	checked, err := runtime.Check(applyOf(t, plan, ""))
	if err != nil {
		t.Fatal(err)
	}
	if checked.Spec.Generation != 3 || len(checked.Inventory()) != 2 {
		t.Fatalf("%+v", checked)
	}
}

func TestAPlanThatDoesNotMatchItsDigestIsRefused(t *testing.T) {
	if r := refusal(t, applyOf(t, `[`+deployment+`]`, protocol.HexDigest([]byte("other")))); r != "DigestMismatch" {
		t.Fatal(r)
	}
}

func TestOnlyAllowedKindsInTheEnvelopesNamespaceAreApplied(t *testing.T) {
	cases := map[string]string{
		`[{"apiVersion":"v1","kind":"Secret","metadata":{"name":"x"}}]`:                                    "KindNotAllowed",
		`[{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"ClusterRole","metadata":{"name":"x"}}]`:     "KindNotAllowed",
		`[{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"x","namespace":"kube-system"}}]`: "OtherNamespace",
		`[{"apiVersion":"apps/v1","kind":"Deployment","metadata":{}}]`:                                     "InvalidEnvelope",
		`{"apiVersion":"apps/v1"}`: "InvalidEnvelope",
		`[{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"x","namespace":"kb-shop-prod"}}] [1]`: "InvalidEnvelope",
	}
	for plan, want := range cases {
		if got := refusal(t, applyOf(t, plan, "")); got != want {
			t.Errorf("%s: %s, want %s", plan, got, want)
		}
	}
	// A spec that misses a member serde required.
	missing := protocol.Apply{Namespace: "kb-shop-prod", Spec: `{"targetId":"t"}`}
	if got := refusal(t, missing); got != "InvalidEnvelope" {
		t.Fatal(got)
	}
}

func live(replicas, available int32, deadline bool) *appsv1.Deployment {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Generation: 2},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 2, Replicas: replicas, UpdatedReplicas: replicas, AvailableReplicas: available,
		},
	}
	if deadline {
		d.Status.Conditions = []appsv1.DeploymentCondition{{
			Type: appsv1.DeploymentProgressing, Status: "False", Reason: "ProgressDeadlineExceeded",
		}}
	}
	return d
}

func TestARolloutIsReadyWhenEveryDeploymentIsAvailable(t *testing.T) {
	cases := []struct {
		name string
		live []runtime.Live
		want string
	}{
		{"available", []runtime.Live{{Name: "a", Deployment: live(2, 2, false)}}, "ready"},
		{"one short", []runtime.Live{{Name: "a", Deployment: live(2, 1, false)}}, "waiting"},
		{"not created", []runtime.Live{{Name: "a"}}, "waiting"},
		{"no status yet", []runtime.Live{{Name: "a", Deployment: live(2, 2, false), Pending: true}}, "waiting"},
		{"past its deadline", []runtime.Live{{Name: "a", Deployment: live(2, 1, true)}}, "failed"},
		{"nothing to wait for", nil, "ready"},
	}
	for _, c := range cases {
		var got string
		switch runtime.RolloutOf(c.live).(type) {
		case runtime.RolloutReady:
			got = "ready"
		case runtime.RolloutWaiting:
			got = "waiting"
		case runtime.RolloutFailed:
			got = "failed"
		}
		if got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}
