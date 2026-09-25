package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/Teamtem-dev/kuben/go/agent/internal/clock"
	"github.com/Teamtem-dev/kuben/go/agent/link"
	"github.com/Teamtem-dev/kuben/go/kubenapi/protocol"
	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

// runtimes is where ApplicationRuntimes live.
func runtimes() schema.GroupVersionResource {
	return v1alpha1.SchemeGroupVersion.WithResource(v1alpha1.ApplicationRuntimeResource)
}

func resourceOf(k Kind) schema.GroupVersionResource {
	gv, err := schema.ParseGroupVersion(k.APIVersion)
	if err != nil {
		gv = schema.GroupVersion{Version: k.APIVersion}
	}
	return gv.WithResource(k.Plural)
}

// isStatus says whether err is the apiserver answering code.
func isStatus(err error, code int32) bool {
	var status apierrors.APIStatus
	return errors.As(err, &status) && status.Status().Code == code
}

// rustDuration is d as Rust's Debug printed a Duration ("900s", "1.5s",
// "250ms").
func rustDuration(d time.Duration) string {
	switch {
	case d >= time.Second:
		return strconv.FormatFloat(d.Seconds(), 'f', -1, 64) + "s"
	case d >= time.Millisecond:
		return strconv.FormatFloat(float64(d)/float64(time.Millisecond), 'f', -1, 64) + "ms"
	case d >= time.Microsecond:
		return strconv.FormatFloat(float64(d)/float64(time.Microsecond), 'f', -1, 64) + "µs"
	}
	return strconv.FormatInt(int64(d), 10) + "ns"
}

// KubeExecutor is the executor of a cluster: it talks to its apiserver
// with the agent's credentials.
type KubeExecutor struct {
	client         dynamic.Interface
	verifyDeadline time.Duration
	poll           time.Duration
	logger         *slog.Logger
	now            func() time.Time
}

// NewKubeExecutor waits up to 15 minutes for a rollout, looking every 3
// seconds.
func NewKubeExecutor(client dynamic.Interface, logger *slog.Logger) *KubeExecutor {
	return &KubeExecutor{client: client, verifyDeadline: 15 * time.Minute, poll: 3 * time.Second, logger: logger, now: clock.Now}
}

// WithVerifyDeadline waits up to deadline for a rollout, looking every
// poll.
func (e *KubeExecutor) WithVerifyDeadline(deadline, poll time.Duration) *KubeExecutor {
	c := *e
	c.verifyDeadline, c.poll = deadline, poll
	return &c
}

func applyOptions() metav1.PatchOptions {
	force := true
	return metav1.PatchOptions{FieldManager: FieldManager, Force: &force}
}

// writeRuntime writes the ApplicationRuntime with server-side apply.
func (e *KubeExecutor) writeRuntime(ctx context.Context, apply protocol.Apply, spec v1alpha1.ApplicationRuntimeSpec) (*unstructured.Unstructured, error) {
	body, err := json.Marshal(map[string]any{
		"apiVersion": "kuben.dev/v1alpha1",
		"kind":       "ApplicationRuntime",
		"metadata": map[string]any{
			"name":      apply.Name,
			"namespace": apply.Namespace,
			"labels":    map[string]any{v1alpha1.LabelManagedBy: v1alpha1.LabelManagerValue, v1alpha1.LabelApp: apply.Name},
		},
		"spec": spec,
	})
	if err != nil {
		return nil, err //nolint:wrapcheck // said as a Kubernetes error
	}
	return e.client.Resource(runtimes()).Namespace(apply.Namespace). //nolint:wrapcheck // said as a Kubernetes error
										Patch(ctx, apply.Name, types.ApplyPatchType, body, applyOptions())
}

// ownerOf is the controller owner reference of the runtime.
func ownerOf(runtime *unstructured.Unstructured) (map[string]any, error) {
	if runtime.GetUID() == "" {
		return nil, Refused{Reason: "KubernetesError", Message: "the ApplicationRuntime has no uid"}
	}
	return map[string]any{
		"apiVersion": "kuben.dev/v1alpha1", "kind": "ApplicationRuntime",
		"name": runtime.GetName(), "uid": string(runtime.GetUID()), "controller": true,
	}, nil
}

// applyResources applies the plan's resources, owned by runtime.
func (e *KubeExecutor) applyResources(ctx context.Context, apply protocol.Apply, checked Checked, runtime *unstructured.Unstructured) error {
	owner, err := ownerOf(runtime)
	if err != nil {
		return err
	}
	for _, resource := range checked.Resources {
		apiVersion, kind, name := str(resource["apiVersion"]), str(resource["kind"]), str(meta(resource)["name"])
		allowed, ok := kindOf(apiVersion, kind)
		if !ok {
			return Refused{Reason: "KindNotAllowed", Message: apiVersion + " " + kind}
		}
		object := k8sruntime.DeepCopyJSON(resource)
		metadata := object["metadata"].(map[string]any) //nolint:forcetypeassert,errcheck // checked by Check
		metadata["namespace"] = apply.Namespace
		// Volumes outlive their app (scenario 6): never owned by the
		// runtime, so never collected with it.
		if allowed.Prune {
			metadata["ownerReferences"] = []any{owner}
		}
		body, err := json.Marshal(object)
		if err != nil {
			return Refused{Reason: "KubernetesError", Message: fmt.Sprintf("%s/%s: %v", kind, name, err)}
		}
		_, err = e.client.Resource(resourceOf(allowed)).Namespace(apply.Namespace).
			Patch(ctx, name, types.ApplyPatchType, body, applyOptions())
		switch {
		case err == nil:
		// The Gateway API is not installed: nothing is routed.
		case isStatus(err, 404) && strings.HasPrefix(allowed.APIVersion, "gateway.networking.k8s.io/"):
		default:
			return Refused{Reason: "KubernetesError", Message: fmt.Sprintf("%s/%s: %v", kind, name, err)}
		}
	}
	return nil
}

// prune deletes what the runtime owns and this plan no longer has.
func (e *KubeExecutor) prune(ctx context.Context, apply protocol.Apply, checked Checked, ownerUID string) error {
	keep := checked.keep()
	selector := v1alpha1.LabelApp + "=" + apply.Name
	for _, kind := range Kinds() {
		if !kind.Prune {
			continue
		}
		api := e.client.Resource(resourceOf(kind)).Namespace(apply.Namespace)
		listed, err := api.List(ctx, metav1.ListOptions{LabelSelector: selector})
		switch {
		case isStatus(err, 404):
			continue
		case err != nil:
			return Refused{Reason: "KubernetesError", Message: fmt.Sprintf("%s: %v", kind.Kind, err)}
		}
		for _, object := range listed.Items {
			owned := false
			for _, r := range object.GetOwnerReferences() {
				owned = owned || string(r.UID) == ownerUID
			}
			name := object.GetName()
			if !owned || keep[[2]string{kind.Kind, name}] {
				continue
			}
			background := metav1.DeletePropagationBackground
			err := api.Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &background})
			switch {
			case err == nil:
				e.logger.Info("pruned an object the plan no longer has", "kind", kind.Kind, "name", name)
			case isStatus(err, 404):
			default:
				return Refused{Reason: "KubernetesError", Message: fmt.Sprintf("%s/%s: %v", kind.Kind, name, err)}
			}
		}
	}
	return nil
}

// readDeployment is the Deployment name, as read.
func (e *KubeExecutor) readDeployment(ctx context.Context, namespace, name string) (Live, error) {
	object, err := e.client.Resource(appsv1.SchemeGroupVersion.WithResource("deployments")).Namespace(namespace).
		Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return Live{Name: name}, nil
	case err != nil:
		return Live{}, err //nolint:wrapcheck // said with the Deployment's name
	}
	var d appsv1.Deployment
	if err := k8sruntime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &d); err != nil {
		return Live{}, err //nolint:wrapcheck // said with the Deployment's name
	}
	_, hasStatus := object.Object["status"]
	return Live{Name: name, Deployment: &d, Pending: !hasStatus}, nil
}

// verify follows the plan's Deployments until they settle or the deadline
// passes.
func (e *KubeExecutor) verify(ctx context.Context, apply protocol.Apply, checked Checked) Rollout {
	names := checked.deployments()
	deadline := e.now().Add(e.verifyDeadline)
	for {
		live := make([]Live, 0, len(names))
		for _, name := range names {
			l, err := e.readDeployment(ctx, apply.Namespace, name)
			if err != nil {
				return RolloutFailed{Why: fmt.Sprintf("%s: %v", name, err)}
			}
			live = append(live, l)
		}
		r := RolloutOf(live)
		waiting, isWaiting := r.(RolloutWaiting)
		if !isWaiting {
			return r
		}
		if !e.now().Before(deadline) {
			return RolloutFailed{Why: fmt.Sprintf("not ready within %s: %s", rustDuration(e.verifyDeadline), waiting.Why)}
		}
		pause := time.NewTimer(e.poll)
		select {
		case <-ctx.Done():
			pause.Stop()
			return RolloutFailed{Why: fmt.Sprintf("not ready: %s", waiting.Why)}
		case <-pause.C:
		}
	}
}

// writeStatus records what the agent saw of the runtime's generation.
func (e *KubeExecutor) writeStatus(ctx context.Context, apply protocol.Apply, checked Checked, ready bool, reason, message string) {
	generation := checked.Spec.Generation
	condition := v1alpha1.NewCondition(v1alpha1.ConditionReady, ready, reason)
	if message != "" {
		condition.Message = &message
	}
	condition.ObservedGeneration = &generation
	status := v1alpha1.ApplicationRuntimeStatus{
		ObservedGeneration: &generation,
		Inventory:          checked.Inventory(),
		Conditions:         v1alpha1.Conditions{condition},
	}
	if ready {
		release := checked.Spec.ReleaseID
		status.EffectiveGeneration, status.EffectiveRelease = &generation, &release
	}
	body, err := json.Marshal(map[string]any{"apiVersion": "kuben.dev/v1alpha1", "kind": "ApplicationRuntime", "status": status})
	if err == nil {
		_, err = e.client.Resource(runtimes()).Namespace(apply.Namespace).
			Patch(ctx, apply.Name, types.ApplyPatchType, body, applyOptions(), "status")
	}
	if err != nil {
		e.logger.Warn("cannot write the runtime's status", "error", err, "runtime", apply.Name)
	}
}

// Apply carries apply out, reporting each step (link.Executor).
func (e *KubeExecutor) Apply(ctx context.Context, apply protocol.Apply, reports chan<- protocol.Observation) {
	e.Carry(ctx, apply, reports)
}

// Carry carries apply out, reporting each step.
func (e *KubeExecutor) Carry(ctx context.Context, apply protocol.Apply, reports chan<- protocol.Observation) {
	// observe reports a phase; with a reason, the message goes along.
	observe := func(generation int64, phase protocol.RuntimePhase, reason, message string) {
		o := protocol.Observation{Target: apply.Target, Generation: generation, Phase: phase}
		if reason != "" {
			o.Reason, o.Message = &reason, &message
		}
		link.Report(ctx, reports, o)
	}
	checked, err := Check(apply)
	if err != nil {
		refused := asRefused(err)
		e.logger.Warn("envelope refused", "target", apply.Target, "reason", refused.Reason, "message", refused.Message)
		observe(0, protocol.RuntimePhaseRejected, refused.Reason, refused.Message)
		return
	}
	generation := checked.Spec.Generation
	runtime, err := e.writeRuntime(ctx, apply, checked.Spec)
	if err != nil {
		phase, reason := protocol.RuntimePhaseFailed, "KubernetesError"
		if isStatus(err, 422) {
			phase, reason = protocol.RuntimePhaseRejected, "EnvelopeRefused"
		}
		observe(generation, phase, reason, err.Error())
		return
	}
	observe(generation, protocol.RuntimePhaseAccepted, "", "")
	if err := e.applyResources(ctx, apply, checked, runtime); err != nil {
		refused := asRefused(err)
		e.writeStatus(ctx, apply, checked, false, refused.Reason, refused.Message)
		observe(generation, protocol.RuntimePhaseFailed, refused.Reason, refused.Message)
		return
	}
	if err := e.prune(ctx, apply, checked, string(runtime.GetUID())); err != nil {
		refused := asRefused(err)
		e.logger.Warn("prune incomplete", "reason", refused.Reason, "message", refused.Message)
	}
	observe(generation, protocol.RuntimePhaseApplying, "", "")
	settled := e.verify(ctx, apply, checked)
	if ctx.Err() != nil {
		return // a newer envelope replaced this one, or the link ended
	}
	switch r := settled.(type) {
	case RolloutReady:
		e.writeStatus(ctx, apply, checked, true, "Available", "")
		observe(generation, protocol.RuntimePhaseReady, "", "")
	case RolloutFailed:
		e.rolloutFailed(ctx, apply, checked, r.Why, observe)
	case RolloutWaiting:
		e.rolloutFailed(ctx, apply, checked, r.Why, observe)
	}
}

func (e *KubeExecutor) rolloutFailed(ctx context.Context, apply protocol.Apply, checked Checked, message string,
	observe func(int64, protocol.RuntimePhase, string, string),
) {
	e.writeStatus(ctx, apply, checked, false, "RolloutFailed", message)
	observe(checked.Spec.Generation, protocol.RuntimePhaseFailed, "RolloutFailed", message)
}

func asRefused(err error) Refused {
	if r, ok := errors.AsType[Refused](err); ok {
		return r
	}
	return Refused{Reason: "KubernetesError", Message: err.Error()}
}
