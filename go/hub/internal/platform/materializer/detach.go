package materializer

// Detach (M4.11): Kuben lets go of an app and leaves it running.
//
// A detach is a `target.delete` that orphans instead of removing:
//  1. The app's ApplicationRuntime (agent delivery) and App are deleted
//     with orphan propagation. Before the App goes, it is marked, so its
//     controller leaves it alone. The garbage collector then drops them
//     from their workloads' owners, so nothing Kuben runs prunes those
//     workloads again.
//  2. The Secrets of the app's last delivered run lose Kuben's
//     `managed-by` label, so secret garbage collection never takes them.
//  3. The target is marked deleted, and the detach is marked complete.
//
// Volumes were never owned and stay. Routes keep their labels: while Kuben
// runs, its Gateway keeps serving the detached hostnames. An unreleased
// detached app also keeps its namespace when the environment is deleted.

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

// orphanWait is how long a detach waits for the garbage collector before
// looking again.
const orphanWait = 2 * time.Second

// LabelDetached is the label a detached Secret gets instead of Kuben's
// `managed-by`.
const LabelDetached = "kuben.dev/detached"

// orphanAppObject takes slug's App in namespace from its controller and
// deletes it, orphaning its workloads; done once it is gone. An App that
// is not target's (of org) is refused.
func (w *Worker) orphanAppObject(ctx context.Context, namespace, slug string, org ids.OrgID, target ids.TargetID) stop {
	ri := w.resource(appsGVR, namespace)
	live, found, err := get[v1alpha1.App](ctx, ri, slug)
	switch {
	case err != nil:
		return retryKube(err)
	case !found:
		return nil
	}
	id, annotated := live.Annotations[v1alpha1.AnnotationID]
	ours := !annotated || id == target.String()
	if !BelongsTo(live.Labels, org) || !ours {
		return refused("NameTaken")
	}
	if _, marked := live.Annotations[v1alpha1.AnnotationHandover]; !marked {
		// First take the App from its controller, which leaves a marked App
		// alone; delete it only after a pause, once a write of that
		// controller already under way has landed. A write after the
		// orphaning would give the workloads back an owner that is going,
		// and the garbage collector would take them with it (CI run
		// 34985518950).
		mark := map[string]any{"metadata": map[string]any{"annotations": map[string]any{
			v1alpha1.AnnotationHandover: target.String(),
		}}}
		if err := mergePatch(ctx, ri, slug, mark); err != nil {
			return retryKube(err)
		}
		return stopWait{after: orphanWait, code: "HandingOverApp"}
	}
	if live.DeletionTimestamp == nil {
		err := ri.Delete(ctx, slug, metav1.DeleteOptions{PropagationPolicy: ptr(metav1.DeletePropagationOrphan)})
		switch {
		case isNotFound(err):
			return nil
		case err != nil:
			return retryKube(err)
		}
		w.d.Logger.Info("the App goes, its workloads stay", "target", target)
	}
	// The garbage collector drops the App from its workloads' owners, then
	// removes it.
	return stopWait{after: orphanWait, code: "OrphaningApp"}
}

// orphanRuntime deletes slug's ApplicationRuntime, orphaning its
// workloads; done once it is gone.
func (w *Worker) orphanRuntime(ctx context.Context, namespace, slug string, target ids.TargetID) stop {
	ri := w.resource(runtimesGVR, namespace)
	live, found, err := get[v1alpha1.ApplicationRuntime](ctx, ri, slug)
	switch {
	case err != nil:
		return retryKube(err)
	case !found:
		return nil
	}
	managed := live.Labels[v1alpha1.LabelManagedBy] == v1alpha1.LabelManagerValue
	if !managed || live.Spec.TargetID != target.String() {
		return refused("NameTaken")
	}
	if live.DeletionTimestamp == nil {
		err := ri.Delete(ctx, slug, metav1.DeleteOptions{PropagationPolicy: ptr(metav1.DeletePropagationOrphan)})
		switch {
		case isNotFound(err):
			return nil
		case err != nil:
			return retryKube(err)
		}
		w.d.Logger.Info("the runtime goes, its workloads stay", "runtime", slug)
	}
	return stopWait{after: orphanWait, code: "OrphaningRuntime"}
}

// detachTarget carries out the detach of subject's target.
func (w *Worker) detachTarget(ctx context.Context, org ids.OrgID, subject store.Subject) stop {
	target, ok := subject.Target.Get()
	if !ok {
		return refused("BadPayload")
	}
	type found struct {
		record   store.DetachedApp
		detached bool
		delivery store.Delivery
		secrets  []store.SecretReference
	}
	f, st := readTenant(ctx, w, org, func(t *store.Tenant) (found, error) {
		var out found
		var err error
		if out.record, out.detached, err = t.DetachedApp(ctx, target); err != nil || !out.detached ||
			out.record.CompletedAt.IsSome() {
			return out, err //nolint:wrapcheck // said as a store error
		}
		if out.delivery, _, err = t.TargetDelivery(ctx, target); err != nil {
			return out, err //nolint:wrapcheck // said as a store error
		}
		material, exported, err := t.ExportMaterial(ctx, target)
		if exported {
			out.secrets = material.Secrets
		}
		return out, err //nolint:wrapcheck // said as a store error
	})
	switch {
	case st != nil:
		return st
	case !f.detached:
		return refused("NotDetached")
	case f.record.CompletedAt.IsSome():
		return nil // finished before
	}
	namespace, slug := f.record.Namespace, f.record.App
	if f.delivery == store.DeliveryAgent {
		if st := w.orphanRuntime(ctx, namespace, slug, target); st != nil {
			return st
		}
	}
	if st := w.orphanAppObject(ctx, namespace, slug, org, target); st != nil {
		return st
	}
	if st := w.releaseSecrets(ctx, namespace, org, target, f.secrets); st != nil {
		return st
	}
	if st := w.writeTenant(ctx, org, func(t *store.Tenant) error {
		if _, err := t.FinishTargetDeletion(ctx, subject.Project, target); err != nil {
			return err //nolint:wrapcheck // said as a store error
		}
		_, err := t.CompleteDetach(ctx, target)
		return err //nolint:wrapcheck // said as a store error
	}); st != nil {
		return st
	}
	w.d.Logger.Info("app detached", "target", target, "app", slug, "namespace", namespace, "secrets", len(f.secrets))
	return nil
}

// releaseSecrets takes Kuben's `managed-by` label off the Secrets of the
// detached app's last delivered run, so secret garbage collection never
// takes them.
func (w *Worker) releaseSecrets(ctx context.Context, namespace string, org ids.OrgID, target ids.TargetID, secrets []store.SecretReference) stop {
	api := w.d.Cluster.Typed.CoreV1().Secrets(namespace)
	release := map[string]any{"metadata": map[string]any{"labels": map[string]any{
		v1alpha1.LabelManagedBy: nil,
		LabelDetached:           target.String(),
	}}}
	for _, secret := range secrets {
		name := secret.Object()
		live, err := api.Get(ctx, name, metav1.GetOptions{})
		switch {
		case isNotFound(err):
			continue
		case err != nil:
			return retryKube(err)
		}
		if live == nil || !BelongsTo(live.Labels, org) {
			continue
		}
		if err := patchSecret(ctx, api, name, release); err != nil {
			return retryKube(err)
		}
	}
	return nil
}

func ptr[T any](v T) *T { return &v }
