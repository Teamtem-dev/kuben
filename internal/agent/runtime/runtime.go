// Package runtime is the agent's executor (ADR-027, M1.9; crates/
// kuben-agent/src/runtime.rs): it carries an execution envelope out in its
// cluster, with client-go's dynamic client and server-side apply on raw
// JSON (no controller-runtime, no typed clientset).
//
//  1. The envelope is checked before anything is written: the plan's
//     resources must match the plan's digest, and each must be of a kind
//     the agent applies ([Kinds]) and in the envelope's namespace. The agent
//     is not a general manifest runner.
//  2. The ApplicationRuntime is written with server-side apply. The
//     apiserver's CEL rules refuse a lower generation, or another envelope
//     under the same generation: that is a rejection, not a retry.
//  3. The plan's resources are applied as [FieldManager], owned by the
//     ApplicationRuntime. Resources an earlier plan had and this one does
//     not are deleted. Volumes are neither owned nor deleted: they outlive
//     their app.
//  4. The agent follows the plan's Deployments until every one is available
//     (Ready), one passes its progress deadline (Failed), or the verify
//     deadline passes (Failed), and writes what it saw into the runtime's
//     status.
package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"

	"github.com/Teamtem-dev/kuben/api/v1alpha1"
	"github.com/Teamtem-dev/kuben/internal/agentlink/protocol"
)

// FieldManager is the field manager of everything the agent writes.
const FieldManager = "kuben-agent"

// Kind is a kind the agent applies.
type Kind struct {
	APIVersion, Kind, Plural string
	// Prune: objects of this kind an earlier plan had and this one does not
	// are deleted. Volumes are kept (they outlive their app, ADR scenario 6).
	Prune bool
}

// Kinds is every kind the agent applies; anything else in a plan is
// refused.
func Kinds() []Kind {
	return []Kind{
		{"v1", "PersistentVolumeClaim", "persistentvolumeclaims", false},
		{"apps/v1", "Deployment", "deployments", true},
		{"autoscaling/v2", "HorizontalPodAutoscaler", "horizontalpodautoscalers", true},
		{"batch/v1", "CronJob", "cronjobs", true},
		{"v1", "Service", "services", true},
		{"gateway.networking.k8s.io/v1beta1", "ReferenceGrant", "referencegrants", true},
		{"gateway.networking.k8s.io/v1", "HTTPRoute", "httproutes", true},
	}
}

func kindOf(apiVersion, kind string) (Kind, bool) {
	for _, k := range Kinds() {
		if k.APIVersion == apiVersion && k.Kind == kind {
			return k, true
		}
	}
	return Kind{}, false
}

// Refused is why an envelope was not carried out.
type Refused struct {
	Reason  string
	Message string
}

func (r Refused) Error() string { return r.Reason + ": " + r.Message }

// Checked is an envelope that passed the checks.
type Checked struct {
	Spec v1alpha1.ApplicationRuntimeSpec
	// Resources are the plan's resources, each of an allowed kind in the
	// envelope's namespace, as decoded (numbers kept as written).
	Resources []map[string]any
}

// str is v when it is a string, else "" (serde_json's as_str with
// unwrap_or_default).
func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func object(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

// meta is a resource's metadata.
func meta(r map[string]any) map[string]any { return object(r["metadata"]) }

// Inventory is what the plan applies.
func (c Checked) Inventory() []v1alpha1.InventoryItem {
	out := make([]v1alpha1.InventoryItem, 0, len(c.Resources))
	for _, r := range c.Resources {
		out = append(out, v1alpha1.InventoryItem{
			APIVersion: str(r["apiVersion"]), Kind: str(r["kind"]), Name: str(meta(r)["name"]),
		})
	}
	return out
}

// decodeJSON decodes data keeping numbers as written.
func decodeJSON(data []byte, into any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := d.Decode(into); err != nil {
		return err //nolint:wrapcheck // said as InvalidEnvelope
	}
	if d.More() {
		return errors.New("trailing characters")
	}
	return nil
}

// specMembers are the members serde required of the spec and its plan.
func specMembers() ([]string, []string) {
	return []string{"targetId", "lifecycleUid", "controlEpoch", "generation", "inputHash", "releaseId", "plan"},
		[]string{"id", "rendererVersion", "digest", "resources"}
}

// decodeSpec decodes the ApplicationRuntimeSpec as strictly as serde did:
// a required member missing or null is an error.
func decodeSpec(text string) (v1alpha1.ApplicationRuntimeSpec, error) {
	var generic map[string]any
	if err := decodeJSON([]byte(text), &generic); err != nil {
		return v1alpha1.ApplicationRuntimeSpec{}, err
	}
	specRequired, planRequired := specMembers()
	for _, check := range []struct {
		object  map[string]any
		members []string
	}{{generic, specRequired}, {object(generic["plan"]), planRequired}} {
		for _, m := range check.members {
			if v, ok := check.object[m]; !ok || v == nil {
				return v1alpha1.ApplicationRuntimeSpec{}, fmt.Errorf("missing field `%s`", m)
			}
		}
	}
	var spec v1alpha1.ApplicationRuntimeSpec
	if err := json.Unmarshal([]byte(text), &spec); err != nil {
		return v1alpha1.ApplicationRuntimeSpec{}, err //nolint:wrapcheck // said as InvalidEnvelope
	}
	return spec, nil
}

// Check checks apply before anything is written. The error is a Refused.
func Check(apply protocol.Apply) (Checked, error) {
	spec, err := decodeSpec(apply.Spec)
	if err != nil {
		return Checked{}, Refused{Reason: "InvalidEnvelope", Message: err.Error()}
	}
	if digest := protocol.HexDigest([]byte(spec.Plan.Resources)); digest != spec.Plan.Digest {
		return Checked{}, Refused{
			Reason:  "DigestMismatch",
			Message: fmt.Sprintf("the plan's resources hash to %s, not %s", digest, spec.Plan.Digest),
		}
	}
	var raw []any
	if err := decodeJSON([]byte(spec.Plan.Resources), &raw); err != nil {
		return Checked{}, Refused{Reason: "InvalidEnvelope", Message: err.Error()}
	}
	resources := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		resource := object(item)
		apiVersion, kind := str(resource["apiVersion"]), str(resource["kind"])
		if _, ok := kindOf(apiVersion, kind); !ok {
			return Checked{}, Refused{Reason: "KindNotAllowed", Message: apiVersion + " " + kind}
		}
		if str(meta(resource)["name"]) == "" {
			return Checked{}, Refused{Reason: "InvalidEnvelope", Message: "a " + kind + " without a name"}
		}
		if namespace, ok := meta(resource)["namespace"].(string); ok && namespace != apply.Namespace {
			return Checked{}, Refused{Reason: "OtherNamespace", Message: fmt.Sprintf("%s in %s, not %s", kind, namespace, apply.Namespace)}
		}
		resources = append(resources, resource)
	}
	return Checked{Spec: spec, Resources: resources}, nil
}

// Rollout is how the plan's Deployments stand.
//
//sumtype:decl
type Rollout interface{ rollout() }

// RolloutReady is every Deployment available.
type RolloutReady struct{}

// RolloutWaiting is some not available yet, and why.
type RolloutWaiting struct{ Why string }

// RolloutFailed is one that will not become available, and why.
type RolloutFailed struct{ Why string }

func (RolloutReady) rollout()   {}
func (RolloutWaiting) rollout() {}
func (RolloutFailed) rollout()  {}

// Live is a Deployment the plan names, as read; nil when it does not exist
// yet.
type Live struct {
	Name       string
	Deployment *appsv1.Deployment
	// Pending: the object has no status yet.
	Pending bool
}

// RolloutOf is the rollout of the Deployments in live: the App
// controller's rules.
func RolloutOf(live []Live) Rollout {
	var waiting []string
	for _, l := range live {
		d := l.Deployment
		if d == nil {
			waiting = append(waiting, l.Name+": not created yet")
			continue
		}
		want := int32(1)
		if d.Spec.Replicas != nil {
			want = *d.Spec.Replicas
		}
		if l.Pending {
			waiting = append(waiting, l.Name+": pending")
			continue
		}
		status := d.Status
		for _, c := range status.Conditions {
			if c.Type == appsv1.DeploymentProgressing && c.Reason == "ProgressDeadlineExceeded" {
				return RolloutFailed{Why: l.Name + ": rollout exceeded its progress deadline"}
			}
		}
		observed := status.ObservedGeneration >= d.Generation
		if !observed || status.UpdatedReplicas < want || status.AvailableReplicas < want || status.Replicas > status.UpdatedReplicas {
			waiting = append(waiting, fmt.Sprintf("%s: %d/%d available", l.Name, status.AvailableReplicas, want))
		}
	}
	if len(waiting) == 0 {
		return RolloutReady{}
	}
	return RolloutWaiting{Why: strings.Join(waiting, "; ")}
}

// keep is the (kind, name) of every resource of the plan.
func (c Checked) keep() map[[2]string]bool {
	out := map[[2]string]bool{}
	for _, item := range c.Inventory() {
		out[[2]string{item.Kind, item.Name}] = true
	}
	return out
}

// deployments are the names of the plan's Deployments.
func (c Checked) deployments() []string {
	var names []string
	for _, r := range c.Resources {
		if name, ok := meta(r)["name"].(string); ok && r["kind"] == "Deployment" {
			names = append(names, name)
		}
	}
	return names
}
