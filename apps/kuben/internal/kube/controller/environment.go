package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/render"
	"github.com/Teamtem-dev/kuben/packages/api/v1alpha1"
)

// Environment → Namespace (+ quota, limit defaults, isolation policy)
// (controller::environment).
//
// Deletion is enforced by a finalizer managed here: `Retain` hands the
// namespace back, `Delete` purges it once the protection grace period has
// elapsed — a soft delete that can be cancelled by nobody but is visible
// (`status.deletionScheduledAt`) the whole time (ADR-018).

// Requeue intervals of the environment controller.
const (
	environmentResync = 10 * time.Minute
	conflictRetry     = 5 * time.Minute
	terminatingRetry  = 5 * time.Second
	maxGraceWait      = time.Hour
)

type environmentReconciler struct{ *shared }

// Reconcile provisions or cleans up one Environment.
func (r environmentReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var env v1alpha1.Environment
	if err := r.client.Get(ctx, req.NamespacedName, &env); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	hasFinalizer := slices.Contains(env.Finalizers, render.EnvFinalizer)
	if env.DeletionTimestamp != nil {
		if !hasFinalizer {
			return reconcile.Result{}, nil
		}
		return r.cleanup(ctx, &env)
	}
	if !hasFinalizer {
		if err := r.setFinalizer(ctx, &env, true); err != nil {
			return reconcile.Result{}, err
		}
	}
	return r.provision(ctx, &env)
}

// namespace is the live namespace name, read from the API server (the
// cache holds only the namespaces Kuben manages).
func (r environmentReconciler) namespace(ctx context.Context, name string) (corev1.Namespace, bool, error) {
	var ns corev1.Namespace
	if err := r.reader.Get(ctx, client.ObjectKey{Name: name}, &ns); err != nil {
		if absent(err) {
			return corev1.Namespace{}, false, nil
		}
		return corev1.Namespace{}, false, fmt.Errorf("reading namespace %s: %w", name, err)
	}
	return ns, true, nil
}

func (r environmentReconciler) provision(ctx context.Context, env *v1alpha1.Environment) (reconcile.Result, error) {
	ns := render.NamespaceName(env.Name)
	existing, found, err := r.namespace(ctx, ns)
	if err != nil {
		return reconcile.Result{}, err
	}
	if found {
		ours := existing.Labels[v1alpha1.LabelManagedBy] == v1alpha1.LabelManagerValue &&
			existing.Labels[v1alpha1.LabelEnvironment] == env.Name
		if !ours {
			// Never adopt a namespace someone else created.
			msg := fmt.Sprintf("namespace %s already exists and is not managed by this environment", ns)
			if err := r.writeStatus(ctx, env, "Degraded", opt.None[string](), "NamespaceConflict", msg, false); err != nil {
				return reconcile.Result{}, err
			}
			return reconcile.Result{RequeueAfter: conflictRetry}, nil
		}
		if existing.DeletionTimestamp != nil {
			msg := fmt.Sprintf("waiting for namespace %s to finish terminating", ns)
			if err := r.writeStatus(ctx, env, "Pending", opt.None[string](), "NamespaceTerminating", msg, false); err != nil {
				return reconcile.Result{}, err
			}
			return reconcile.Result{RequeueAfter: terminatingRetry}, nil
		}
	}
	for _, obj := range []map[string]any{
		render.Namespace(env), render.ResourceQuota(env), render.LimitRange(env), render.NetworkPolicy(env),
	} {
		if err := apply(ctx, r.client, obj); err != nil {
			return reconcile.Result{}, err
		}
	}
	if err := r.writeStatus(ctx, env, "Ready", opt.None[string](), "Provisioned", "", true); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{RequeueAfter: environmentResync}, nil
}

func (r environmentReconciler) cleanup(ctx context.Context, env *v1alpha1.Environment) (reconcile.Result, error) {
	ns := render.NamespaceName(env.Name)
	policy := env.Spec.DeletionPolicy
	if policy == "" {
		policy = v1alpha1.DeletionPolicyRetain
	}
	switch policy {
	case v1alpha1.DeletionPolicyRetain:
		// Hand the namespace back: Kuben stops managing it, the data stays.
		release, err := json.Marshal(map[string]any{"metadata": map[string]any{"labels": map[string]any{
			v1alpha1.LabelManagedBy: nil, v1alpha1.LabelEnvironment: nil,
		}}})
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("encoding the release patch: %w", err)
		}
		target := &corev1.Namespace{}
		target.Name = ns
		if err := r.client.Patch(ctx, target, client.RawPatch(types.MergePatchType, release)); err != nil && !absent(err) {
			return reconcile.Result{}, fmt.Errorf("releasing namespace %s: %w", ns, err)
		}
	case v1alpha1.DeletionPolicyDelete:
		if res, wait, err := r.purge(ctx, env, ns); err != nil || wait {
			return res, err
		}
	}
	if err := r.setFinalizer(ctx, env, false); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

// purge deletes the namespace once the grace period has ended; wait is
// true while the finalizer must stay.
func (r environmentReconciler) purge(ctx context.Context, env *v1alpha1.Environment, ns string) (res reconcile.Result, wait bool, err error) {
	var graceMs int64
	if p := env.Spec.Protection; p != nil {
		if seconds, ok := parseDuration(p.DeletionGrace); ok {
			graceMs = secondsToMillis(seconds)
		}
	}
	nowMs := r.clock.NowMs()
	deletedMs := nowMs
	if t := env.DeletionTimestamp; t != nil {
		deletedMs = t.UnixMilli()
	}
	purgeMs := clock.SaturatingAdd(deletedMs, graceMs)
	if nowMs < purgeMs {
		const msg = "the namespace is deleted when the grace period ends"
		if err := r.writeStatus(ctx, env, "Terminating", timestampOf(purgeMs), "DeletionScheduled", msg, false); err != nil {
			return reconcile.Result{}, true, err
		}
		remaining := time.Duration(min(purgeMs-nowMs, maxGraceWait.Milliseconds())) * time.Millisecond
		return reconcile.Result{RequeueAfter: remaining}, true, nil
	}
	existing, found, err := r.namespace(ctx, ns)
	if err != nil {
		return reconcile.Result{}, true, err
	}
	if !found {
		return reconcile.Result{}, false, nil
	}
	if existing.DeletionTimestamp == nil {
		if err := r.client.Delete(ctx, &existing, client.PropagationPolicy("Background")); err != nil && !absent(err) {
			return reconcile.Result{}, true, fmt.Errorf("deleting namespace %s: %w", ns, err)
		}
		r.logger.Info("environment namespace deleted", "namespace", ns)
	}
	// Keep the finalizer until the namespace (and every app in it) is gone.
	return reconcile.Result{RequeueAfter: terminatingRetry}, true, nil
}

// maxTimestampMs is 9999-12-31T23:59:59.999Z, the last instant an RFC 3339
// timestamp can name.
const maxTimestampMs = 253_402_300_799_999

// timestampOf is msUnix as RFC 3339 (fractional seconds only when there
// are any), absent beyond year 9999.
func timestampOf(msUnix int64) opt.Val[string] {
	if msUnix > maxTimestampMs || msUnix < -62_135_596_800_000 {
		return opt.None[string]()
	}
	return opt.Some(time.UnixMilli(msUnix).UTC().Format(time.RFC3339Nano))
}

// setFinalizer adds or removes the environment finalizer. The patch
// carries resourceVersion, which makes it conditional: a concurrent update
// is a 409.
func (r environmentReconciler) setFinalizer(ctx context.Context, env *v1alpha1.Environment, present bool) error {
	finalizers := make([]string, 0, len(env.Finalizers)+1)
	for _, f := range env.Finalizers {
		if f != render.EnvFinalizer {
			finalizers = append(finalizers, f)
		}
	}
	if present {
		finalizers = append(finalizers, render.EnvFinalizer)
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"finalizers": finalizers, "resourceVersion": env.ResourceVersion,
	}})
	if err != nil {
		return fmt.Errorf("encoding the finalizer patch: %w", err)
	}
	target := &v1alpha1.Environment{}
	target.Name = env.Name
	if err := r.client.Patch(ctx, target, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("updating the finalizers of environment %s: %w", env.Name, err)
	}
	env.ResourceVersion = target.ResourceVersion
	env.Finalizers = target.Finalizers
	return nil
}

func (r environmentReconciler) writeStatus(ctx context.Context, env *v1alpha1.Environment, phase string,
	deletionScheduledAt opt.Val[string], reason, message string, ready bool,
) error {
	var previous []v1alpha1.Condition
	if env.Status != nil {
		previous = env.Status.Conditions
	}
	generation := generationOf(&env.ObjectMeta)
	ns := render.NamespaceName(env.Name)
	status := v1alpha1.EnvironmentStatus{
		ObservedGeneration:  generation.Ptr(),
		Namespace:           &ns,
		Phase:               &phase,
		DeletionScheduledAt: deletionScheduledAt.Ptr(),
		Conditions: v1alpha1.Conditions{
			Condition(r.clock, previous, v1alpha1.ConditionReady, ready, reason, message, generation),
		},
	}
	return applyStatus(ctx, r.client, v1alpha1.EnvironmentKind, "", env.Name, status)
}

// environmentOfNamespace maps a managed namespace to its Environment:
// repairing drift, a deleted or relabelled namespace re-triggers it.
func environmentOfNamespace(_ context.Context, ns *corev1.Namespace) []reconcile.Request {
	env, ok := ns.Labels[v1alpha1.LabelEnvironment]
	if !ok {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: env}}}
}
