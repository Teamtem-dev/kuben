package controller

import (
	"context"
	"fmt"
	"math"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/Teamtem-dev/kuben/packages/api/v1alpha1"
)

// Project status: the number of live environments, and Ready
// (controller::project).

// projectResync is how often a Project is reconciled without a change.
const projectResync = 15 * time.Minute

type projectReconciler struct{ *shared }

// Reconcile writes the Project's status.
func (r projectReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var project v1alpha1.Project
	if err := r.client.Get(ctx, req.NamespacedName, &project); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	// Projects are few and environments per project fewer; a LIST is
	// cheaper than keeping a second cache in sync.
	var envs v1alpha1.EnvironmentList
	if err := r.reader.List(ctx, &envs); err != nil {
		return reconcile.Result{}, fmt.Errorf("listing environments: %w", err)
	}
	var count uint64
	for i := range envs.Items {
		e := &envs.Items[i]
		if e.Spec.Project == project.Name && e.DeletionTimestamp == nil {
			count++
		}
	}
	var previous []v1alpha1.Condition
	if project.Status != nil {
		previous = project.Status.Conditions
	}
	generation := generationOf(&project.ObjectMeta)
	status := v1alpha1.ProjectStatus{
		ObservedGeneration: generation.Ptr(),
		Environments:       uint32(min(count, math.MaxUint32)),
		Conditions: v1alpha1.Conditions{
			Condition(r.clock, previous, v1alpha1.ConditionReady, true, "Reconciled", "", generation),
		},
	}
	if err := applyStatus(ctx, r.client, v1alpha1.ProjectKind, "", project.Name, status); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{RequeueAfter: projectResync}, nil
}

// projectOfEnvironment maps an Environment to its Project.
func projectOfEnvironment(_ context.Context, env *v1alpha1.Environment) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: env.Spec.Project}}}
}
