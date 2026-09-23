package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/render"
	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

// App → Deployments (+ HPAs), CronJobs, volumes, a Service and a Gateway
// API HTTPRoute (controller::app). Every child but the volumes carries an
// owner reference (garbage-collected with the App) and is pruned when it
// disappears from the spec.

// Exposed is the App condition type: is the app reachable through the
// gateway?
const Exposed = "Exposed"

// Requeue intervals of the App controller: owned Deployments re-trigger it
// on rollout progress; the timer is a safety net.
const (
	appResync  = 10 * time.Minute
	appRollout = 30 * time.Second
)

// gatewayGroup is the Gateway API's group.
const gatewayGroup = "gateway.networking.k8s.io"

// The kinds the App controller writes (functions, not variables: no
// package-level mutable state).
func deploymentKind() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
}

func hpaKind() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "autoscaling", Version: "v2", Kind: "HorizontalPodAutoscaler"}
}

func cronJobKind() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "CronJob"}
}

func serviceKind() schema.GroupVersionKind {
	return schema.GroupVersionKind{Version: "v1", Kind: "Service"}
}

func httpRouteKind() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: gatewayGroup, Version: "v1", Kind: "HTTPRoute"}
}

func grantKind() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: gatewayGroup, Version: "v1beta1", Kind: "ReferenceGrant"}
}

// nameOf is metadata.name of a JSON-shaped object; "" when it has none.
func nameOf(obj map[string]any) string {
	meta, ok := obj["metadata"].(map[string]any)
	if !ok {
		return ""
	}
	name, _ := meta["name"].(string)
	return name
}

// HandsOff reports whether the App controller no longer writes app: it is
// being deleted (it has no finalizer, so there is nothing to clean up), or
// it was handed over to the cluster's agent (M1.9). Writing its workloads
// now would give them back an owner that is going away, and the garbage
// collector would take them along.
func HandsOff(app *v1alpha1.App) bool {
	if app.DeletionTimestamp != nil {
		return true
	}
	_, handedOver := app.Annotations[v1alpha1.AnnotationHandover]
	return handedOver
}

// ownerOf is the controller owner reference of app's children, as kube-rs
// made it (no blockOwnerDeletion).
func ownerOf(app *v1alpha1.App) (render.OwnerReference, error) {
	if app.UID == "" {
		return render.OwnerReference{}, errMissing("metadata.uid")
	}
	return render.OwnerReference{
		APIVersion: v1alpha1.SchemeGroupVersion.String(),
		Kind:       v1alpha1.AppKind,
		Name:       app.Name,
		UID:        string(app.UID),
		Controller: true,
	}, nil
}

// withoutBlockOwnerDeletion drops the `blockOwnerDeletion: false` the
// builder writes into owner references: kube-rs left the member out.
func withoutBlockOwnerDeletion(obj map[string]any) map[string]any {
	meta, ok := obj["metadata"].(map[string]any)
	if !ok {
		return obj
	}
	refs, ok := meta["ownerReferences"].([]any)
	if !ok {
		return obj
	}
	for _, r := range refs {
		if ref, ok := r.(map[string]any); ok && ref["blockOwnerDeletion"] == false {
			delete(ref, "blockOwnerDeletion")
		}
	}
	return obj
}

type appReconciler struct{ *shared }

// Reconcile applies what one App's spec makes and writes its status.
func (r appReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var app v1alpha1.App
	if err := r.client.Get(ctx, req.NamespacedName, &app); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if HandsOff(&app) {
		return reconcile.Result{}, nil
	}
	if app.Namespace == "" {
		return reconcile.Result{}, errMissing("metadata.namespace")
	}
	owner, err := ownerOf(&app)
	if err != nil {
		return reconcile.Result{}, err
	}
	platform, err := r.platform(ctx)
	if err != nil {
		return reconcile.Result{}, err
	}
	generation := generationOf(&app.ObjectMeta)
	var previous []v1alpha1.Condition
	if app.Status != nil {
		previous = app.Status.Conditions
	}

	desired, err := render.Build(&app, platform, owner)
	var invalid *render.BuildError
	if errors.As(err, &invalid) {
		// A spec problem: report it and wait for the user to change the App.
		ready := Condition(r.clock, previous, v1alpha1.ConditionReady, false, string(invalid.Reason), invalid.Error(), generation)
		return reconcile.Result{}, r.writeStatus(ctx, &app, opt.None[string](), []v1alpha1.Condition{ready})
	}
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("building app %s/%s: %w", app.Namespace, app.Name, err)
	}
	routed, err := r.applyChildren(ctx, &app, owner, desired)
	if err != nil {
		return reconcile.Result{}, err
	}
	ready, reason, message, err := r.rollout(ctx, app.Namespace, desired.Deployments)
	if err != nil {
		return reconcile.Result{}, err
	}
	if ready && len(desired.Deployments) == 0 && len(desired.CronJobs) > 0 {
		reason = "Scheduled"
	}
	conditions := []v1alpha1.Condition{
		Condition(r.clock, previous, v1alpha1.ConditionReady, ready, reason, message, generation),
	}
	if desired.ExposesHTTP {
		ok, reason, msg := exposure(desired.Route.IsSome(), routed, platform.Gateway.IsSome())
		conditions = append(conditions, Condition(r.clock, previous, Exposed, ok, reason, msg, generation))
	}
	url := opt.None[string]()
	if routed {
		url = render.URL(&app, platform)
	}
	if err := r.writeStatus(ctx, &app, url, conditions); err != nil {
		return reconcile.Result{}, err
	}
	if ready {
		return reconcile.Result{RequeueAfter: appResync}, nil
	}
	return reconcile.Result{RequeueAfter: appRollout}, nil
}

// exposure is the Exposed condition: (ok, reason, message).
func exposure(hasRoute, routed, hasGateway bool) (ok bool, reason, message string) {
	switch {
	case hasRoute && routed:
		return true, "RouteApplied", ""
	case hasRoute:
		return false, "GatewayAPIMissing", "the Gateway API CRDs are not installed"
	case !hasGateway:
		return false, "NoGateway", "set spec.gatewayClassName (or spec.gateway) in KubenConfig to expose apps"
	default:
		return false, "NoHostname", "add a domain or set spec.baseDomain in KubenConfig"
	}
}

// applyChildren writes every child, in the Rust order, and reports whether
// the route was applied.
func (r appReconciler) applyChildren(ctx context.Context, app *v1alpha1.App, owner render.OwnerReference,
	desired render.Desired,
) (bool, error) {
	selector := client.MatchingLabels{v1alpha1.LabelApp: app.Name}
	// Volumes first (pods wait for their claims). Never pruned: data
	// outlives spec edits and the App itself (scenario 6).
	if err := r.applyAll(ctx, desired.Volumes); err != nil {
		return false, err
	}
	for _, kind := range []struct {
		gvk     schema.GroupVersionKind
		objects []map[string]any
	}{
		{deploymentKind(), desired.Deployments},
		{hpaKind(), desired.Autoscalers},
		{cronJobKind(), desired.CronJobs},
	} {
		if err := r.applyAll(ctx, kind.objects); err != nil {
			return false, err
		}
		if err := r.prune(ctx, kind.gvk, app.Namespace, selector, owner.UID, kind.objects); err != nil {
			return false, err
		}
	}
	if svc, ok := desired.Service.Get(); ok {
		if err := r.applyAll(ctx, []map[string]any{svc}); err != nil {
			return false, err
		}
	} else if err := r.deleteIfExists(ctx, serviceKind(), app.Namespace, app.Name); err != nil {
		return false, err
	}
	if _, err := r.syncGatewayObject(ctx, grantKind(), app.Namespace, app.Name+render.GrantSuffix, desired.Grant); err != nil {
		return false, err
	}
	return r.syncGatewayObject(ctx, httpRouteKind(), app.Namespace, app.Name, desired.Route)
}

func (r appReconciler) applyAll(ctx context.Context, objects []map[string]any) error {
	for _, obj := range objects {
		if nameOf(obj) == "" {
			return errMissing("metadata.name")
		}
		if err := apply(ctx, r.client, withoutBlockOwnerDeletion(obj)); err != nil {
			return err
		}
	}
	return nil
}

// prune deletes the children of this App (by owner uid) that are no
// longer desired.
func (r appReconciler) prune(ctx context.Context, gvk schema.GroupVersionKind, namespace string,
	selector client.MatchingLabels, ownerUID string, keep []map[string]any,
) error {
	kept := make(map[string]bool, len(keep))
	for _, o := range keep {
		kept[nameOf(o)] = true
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
	if err := r.reader.List(ctx, list, client.InNamespace(namespace), selector); err != nil {
		return fmt.Errorf("listing %s in %s: %w", gvk.Kind, namespace, err)
	}
	for i := range list.Items {
		obj := &list.Items[i]
		owned := false
		for _, ref := range obj.GetOwnerReferences() {
			owned = owned || string(ref.UID) == ownerUID
		}
		if owned && !kept[obj.GetName()] {
			if err := r.deleteIfExists(ctx, gvk, namespace, obj.GetName()); err != nil {
				return err
			}
			r.logger.Info("pruned object removed from the app spec", "kind", gvk.Kind, "name", obj.GetName())
		}
	}
	return nil
}

// deleteIfExists deletes the object in the background; one that is gone,
// or whose kind is not served, is no error.
func (r appReconciler) deleteIfExists(ctx context.Context, gvk schema.GroupVersionKind, namespace, name string) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetNamespace(namespace)
	obj.SetName(name)
	if err := r.client.Delete(ctx, obj, client.PropagationPolicy("Background")); err != nil && !absent(err) {
		return fmt.Errorf("deleting %s %s/%s: %w", gvk.Kind, namespace, name, err)
	}
	return nil
}

// syncGatewayObject applies body, or deletes the object when there is none.
// False when the Gateway API (or this kind of it) is not installed, or
// there was nothing to apply.
func (r appReconciler) syncGatewayObject(ctx context.Context, gvk schema.GroupVersionKind, namespace, name string,
	body opt.Val[map[string]any],
) (bool, error) {
	obj, ok := body.Get()
	if !ok {
		return false, r.deleteIfExists(ctx, gvk, namespace, name)
	}
	if err := apply(ctx, r.client, withoutBlockOwnerDeletion(obj)); err != nil {
		if absent(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// rollout is (ready, reason, message) from the live Deployments.
func (r appReconciler) rollout(ctx context.Context, namespace string, desired []map[string]any) (
	ready bool, reason, message string, err error,
) {
	var waiting []string
	for _, d := range desired {
		name := nameOf(d)
		live := &unstructured.Unstructured{}
		live.SetGroupVersionKind(deploymentKind())
		if err := r.reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, live); err != nil {
			if absent(err) {
				waiting = append(waiting, name+": not created yet")
				continue
			}
			return false, "", "", fmt.Errorf("reading deployment %s/%s: %w", namespace, name, err)
		}
		verdict, failed, err := deploymentProgress(live)
		if err != nil {
			return false, "", "", err
		}
		if failed {
			return false, "RolloutFailed", verdict, nil
		}
		if verdict != "" {
			waiting = append(waiting, verdict)
		}
	}
	if len(waiting) == 0 {
		return true, "Available", "", nil
	}
	return false, "Progressing", strings.Join(waiting, "; "), nil
}

// deploymentProgress is why live is not rolled out yet ("" when it is),
// and whether its rollout failed.
func deploymentProgress(live *unstructured.Unstructured) (string, bool, error) {
	name := live.GetName()
	var d appsv1.Deployment
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(live.Object, &d); err != nil {
		return "", false, fmt.Errorf("decoding deployment %s: %w", name, err)
	}
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	if _, ok := live.Object["status"]; !ok {
		return name + ": pending", false, nil
	}
	st := d.Status
	for _, c := range st.Conditions {
		if c.Type == appsv1.DeploymentProgressing && c.Reason == "ProgressDeadlineExceeded" {
			return name + ": rollout exceeded its progress deadline", true, nil
		}
	}
	observed := st.ObservedGeneration >= d.Generation
	if !observed || st.UpdatedReplicas < want || st.AvailableReplicas < want || st.Replicas > st.UpdatedReplicas {
		return fmt.Sprintf("%s: %d/%d available", name, st.AvailableReplicas, want), false, nil
	}
	return "", false, nil
}

func (r appReconciler) writeStatus(ctx context.Context, app *v1alpha1.App, url opt.Val[string],
	conditions []v1alpha1.Condition,
) error {
	status := v1alpha1.AppStatus{
		ObservedGeneration: generationOf(&app.ObjectMeta).Ptr(),
		URL:                url.Ptr(),
		Conditions:         conditions,
	}
	return applyStatus(ctx, r.client, v1alpha1.AppKind, app.Namespace, app.Name, status)
}
