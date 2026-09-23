package materializer

// Lifecycle operations (ADR-032): the resources of projects and
// environments as soon as they exist in SQL, and the removal of what is
// being deleted.
//
// An environment's namespace hosts secrets before any app is deployed, so
// `environment.apply` writes the Project and Environment objects right
// away; deployment runs write them again, identically. A deletion removes
// the object and marks the rows deleted once it is gone. An Environment
// object stays until its controller has removed the namespace (after the
// protection grace period for production), so `environment.delete` checks
// back until then; it never gives up.

import (
	"context"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ops/run"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/render"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

// carryLifecycle carries out the lifecycle operation claim holds.
func (w *Worker) carryLifecycle(ctx context.Context, claim store.Claim) stop {
	subject, found, err := w.d.Store.LifecycleSubject(ctx, claim)
	switch {
	case err != nil:
		return retryStore(err)
	case !found:
		return settled(run.Failed, "BadPayload")
	}
	var done stop
	switch {
	case claim.Kind == store.ProjectApply:
		done = w.applyProject(ctx, claim.Org, claim.ID, subject)
	case claim.Kind == store.EnvironmentApply:
		done = w.applyEnvironment(ctx, claim.Org, claim.ID, subject)
	case claim.Kind == store.TargetDelete && subject.Detach:
		done = w.detachTarget(ctx, claim.Org, subject)
	case claim.Kind == store.TargetDelete:
		done = w.deleteTarget(ctx, claim.Org, subject)
	case claim.Kind == store.EnvironmentDelete:
		done = w.deleteEnvironment(ctx, claim.Org, subject)
	case claim.Kind == store.ProjectDelete:
		done = w.deleteProject(ctx, claim.Org, subject)
	default:
		done = refused("UnknownKind")
	}
	switch s := done.(type) {
	case nil:
		return settled(run.Succeeded, "")
	case stopRefused:
		return settled(run.Failed, s.code)
	case stopSuperseded:
		// Nothing to do any more: the subject is gone or being deleted.
		return settled(run.Cancelled, "")
	case stopFenced, stopShutdown, stopSettled, stopRetry, stopWait:
	}
	return done
}

// readTenant runs read in a read-only transaction of org.
func readTenant[T any](ctx context.Context, w *Worker, org ids.OrgID, read func(*store.Tenant) (T, error)) (T, stop) {
	t, err := w.d.Store.Tenant(ctx, org)
	if err != nil {
		var zero T
		return zero, retryStore(err)
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	v, err := read(t)
	if err != nil {
		return v, retryStore(err)
	}
	return v, nil
}

// writeTenant runs write in a transaction of org and commits it.
func (w *Worker) writeTenant(ctx context.Context, org ids.OrgID, write func(*store.Tenant) error) stop {
	t, err := w.d.Store.Tenant(ctx, org)
	if err != nil {
		return retryStore(err)
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	if err := write(t); err != nil {
		return retryStore(err)
	}
	if err := t.Commit(ctx); err != nil {
		return retryStore(err)
	}
	return nil
}

// projectRecord is the project of org with id, and false when it is gone.
func (w *Worker) projectRecord(ctx context.Context, org ids.OrgID, id ids.ProjectID) (store.Project, bool, stop) {
	projects, st := readTenant(ctx, w, org, func(t *store.Tenant) ([]store.Project, error) { return t.Projects(ctx) })
	if st != nil {
		return store.Project{}, false, st
	}
	i := slices.IndexFunc(projects, func(p store.Project) bool { return p.ID == id })
	if i < 0 {
		return store.Project{}, false, nil
	}
	return projects[i], true, nil
}

// environmentRecord is subject's environment, and false when it is gone.
func (w *Worker) environmentRecord(ctx context.Context, org ids.OrgID, subject store.Subject) (store.EnvironmentRecord, bool, stop) {
	id, ok := subject.Environment.Get()
	if !ok {
		return store.EnvironmentRecord{}, false, refused("BadPayload")
	}
	environments, st := readTenant(ctx, w, org, func(t *store.Tenant) ([]store.EnvironmentRecord, error) {
		return t.Environments(ctx, subject.Project)
	})
	if st != nil {
		return store.EnvironmentRecord{}, false, st
	}
	i := slices.IndexFunc(environments, func(e store.EnvironmentRecord) bool { return e.ID == id })
	if i < 0 {
		return store.EnvironmentRecord{}, false, nil
	}
	return environments[i], true, nil
}

func material(org ids.OrgID, p store.Project) ProjectMaterial {
	return ProjectMaterial{Org: org, ID: p.ID, Slug: p.Slug, Name: p.Name, Description: p.Description}
}

func environmentMaterial(e store.EnvironmentRecord) EnvironmentMaterial {
	return EnvironmentMaterial{
		ID: e.ID, Slug: e.Slug, Name: e.Name, EnvType: e.EnvType, Quota: e.Quota, Namespace: e.Namespace,
	}
}

func (w *Worker) applyProject(ctx context.Context, org ids.OrgID, operation ids.OperationID, subject store.Subject) stop {
	project, found, st := w.projectRecord(ctx, org, subject.Project)
	switch {
	case st != nil:
		return st
	case !found || project.Deleting:
		return stopSuperseded{}
	}
	desired := ProjectObject(material(org, project), operation)
	_, st = ensure(ctx, w, projectsGVR, "", v1alpha1.ProjectKind, &desired, org)
	return st
}

func (w *Worker) applyEnvironment(ctx context.Context, org ids.OrgID, operation ids.OperationID, subject store.Subject) stop {
	project, found, st := w.projectRecord(ctx, org, subject.Project)
	switch {
	case st != nil:
		return st
	case !found || project.Deleting:
		return stopSuperseded{}
	}
	environment, found, st := w.environmentRecord(ctx, org, subject)
	switch {
	case st != nil:
		return st
	case !found || environment.Deleting:
		return stopSuperseded{}
	}
	p := material(org, project)
	desired, rerr := EnvironmentObject(p, environmentMaterial(environment), operation)
	if rerr != nil {
		return refused(rerr.Code)
	}
	projectObject := ProjectObject(p, operation)
	written, st := ensure(ctx, w, projectsGVR, "", v1alpha1.ProjectKind, &projectObject, org)
	if st != nil {
		return st
	}
	if !SetOwner(&desired, &written) {
		return refused("IncompleteObject")
	}
	_, st = ensure(ctx, w, environmentsGVR, "", v1alpha1.EnvironmentKind, &desired, org)
	return st
}

func (w *Worker) deleteTarget(ctx context.Context, org ids.OrgID, subject store.Subject) stop {
	environment, hasEnvironment := subject.Environment.Get()
	target, hasTarget := subject.Target.Get()
	if !hasEnvironment || !hasTarget {
		return refused("BadPayload")
	}
	type found struct {
		app      store.AppRecord
		exists   bool
		delivery store.Delivery
	}
	f, st := readTenant(ctx, w, org, func(t *store.Tenant) (found, error) {
		apps, err := t.Apps(ctx, environment)
		if err != nil {
			return found{}, err //nolint:wrapcheck // said as a store error
		}
		var out found
		if i := slices.IndexFunc(apps, func(a store.AppRecord) bool { return a.Target == target }); i >= 0 {
			out.app, out.exists = apps[i], true
		}
		out.delivery, _, err = t.TargetDelivery(ctx, target)
		return out, err //nolint:wrapcheck // said as a store error
	})
	switch {
	case st != nil:
		return st
	case !f.exists:
		return nil // finished before
	}
	if f.delivery == store.DeliveryAgent {
		// The runtime owns the target's objects, volumes excepted: they go
		// with it.
		if err := remove(ctx, w.resource(runtimesGVR, f.app.Namespace), f.app.Slug, metav1.DeletePropagationBackground); err != nil {
			return retryKube(err)
		}
	}
	if st := w.removeApp(ctx, org, target, f.app); st != nil {
		return st
	}
	if subject.DeleteVolumes {
		if st := w.removeVolumes(ctx, f.app); st != nil {
			return st
		}
	}
	return w.writeTenant(ctx, org, func(t *store.Tenant) error {
		_, err := t.FinishTargetDeletion(ctx, subject.Project, target)
		return err //nolint:wrapcheck // said as a store error
	})
}

// removeApp removes the App object of app; for a target handed over to its
// agent, one a handover that never finished left behind. Only an object of
// org that is target's (or carries no id) goes.
func (w *Worker) removeApp(ctx context.Context, org ids.OrgID, target ids.TargetID, app store.AppRecord) stop {
	ri := w.resource(appsGVR, app.Namespace)
	live, found, err := get[v1alpha1.App](ctx, ri, app.Slug)
	switch {
	case err != nil:
		return retryKube(err)
	case !found:
		return nil
	}
	id, annotated := live.Annotations[v1alpha1.AnnotationID]
	ours := !annotated || id == target.String()
	if BelongsTo(live.Labels, org) && ours {
		if err := remove(ctx, ri, app.Slug, metav1.DeletePropagationBackground); err != nil {
			return retryKube(err)
		}
	}
	return nil
}

// removeVolumes deletes the retained volumes of app.
func (w *Worker) removeVolumes(ctx context.Context, app store.AppRecord) stop {
	claims := w.d.Cluster.Typed.CoreV1().PersistentVolumeClaims(app.Namespace)
	list, err := claims.List(ctx, metav1.ListOptions{LabelSelector: v1alpha1.LabelApp + "=" + app.Slug})
	if err != nil {
		return retryKube(err)
	}
	background := metav1.DeletePropagationBackground
	for _, pvc := range list.Items {
		if _, retained := pvc.Annotations[render.Retain]; !retained {
			continue
		}
		err := claims.Delete(ctx, pvc.Name, metav1.DeleteOptions{PropagationPolicy: &background})
		if err != nil && !isNotFound(err) {
			return retryKube(err)
		}
	}
	return nil
}

func (w *Worker) deleteEnvironment(ctx context.Context, org ids.OrgID, subject store.Subject) stop {
	project, found, st := w.projectRecord(ctx, org, subject.Project)
	switch {
	case st != nil:
		return st
	case !found:
		return nil
	}
	environment, found, st := w.environmentRecord(ctx, org, subject)
	switch {
	case st != nil:
		return st
	case !found:
		return nil // finished before
	}
	name := EnvironmentName(project.Slug, environment.Slug)
	ri := w.resource(environmentsGVR, "")
	live, exists, err := get[v1alpha1.Environment](ctx, ri, name)
	if err != nil {
		return retryKube(err)
	}
	if exists {
		if !BelongsTo(live.Labels, org) {
			return refused("NameTaken")
		}
		if live.DeletionTimestamp == nil {
			if st := w.keepDetachedNamespace(ctx, org, environment.ID, &live); st != nil {
				return st
			}
			if err := remove(ctx, ri, name, metav1.DeletePropagationBackground); err != nil {
				return retryKube(err)
			}
		}
		// Its controller removes the namespace first, then the object.
		return stopWait{after: w.d.DeletionCheck, code: "WaitingForNamespace"}
	}
	return w.writeTenant(ctx, org, func(t *store.Tenant) error {
		_, err := t.FinishEnvironmentDeletion(ctx, environment.ID)
		return err //nolint:wrapcheck // said as a store error
	})
}

// keepDetachedNamespace: an environment with unreleased detached apps keeps
// its namespace (M4.11); its object is switched to Retain before it is
// deleted.
func (w *Worker) keepDetachedNamespace(ctx context.Context, org ids.OrgID, environment ids.EnvironmentID, live *v1alpha1.Environment) stop {
	if live.Spec.DeletionPolicy == v1alpha1.DeletionPolicyRetain {
		return nil
	}
	held, st := readTenant(ctx, w, org, func(t *store.Tenant) (uint64, error) { return t.DetachedHeld(ctx, environment) })
	if st != nil {
		return st
	}
	if held == 0 {
		return nil
	}
	retain := map[string]any{"spec": map[string]any{"deletionPolicy": v1alpha1.DeletionPolicyRetain}}
	if err := mergePatch(ctx, w.resource(environmentsGVR, ""), live.Name, retain); err != nil {
		return retryKube(err)
	}
	w.d.Logger.Info("the namespace stays for detached apps", "environment", environment, "held", held)
	return nil
}

func (w *Worker) deleteProject(ctx context.Context, org ids.OrgID, subject store.Subject) stop {
	project, found, st := w.projectRecord(ctx, org, subject.Project)
	switch {
	case st != nil:
		return st
	case !found:
		return nil // finished before
	}
	ri := w.resource(projectsGVR, "")
	live, exists, err := get[v1alpha1.Project](ctx, ri, project.Slug)
	if err != nil {
		return retryKube(err)
	}
	if exists && BelongsTo(live.Labels, org) {
		if err := remove(ctx, ri, project.Slug, metav1.DeletePropagationBackground); err != nil {
			return retryKube(err)
		}
	}
	return w.writeTenant(ctx, org, func(t *store.Tenant) error {
		_, err := t.FinishProjectDeletion(ctx, project.ID)
		return err //nolint:wrapcheck // said as a store error
	})
}
