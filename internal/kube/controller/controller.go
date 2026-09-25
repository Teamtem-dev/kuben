// Package controller is the reconcilers (crates/kuben-platform/src/
// controller/mod.rs, crd_apply.rs, project.rs, environment.rs, app.rs and
// the controller half of gateway.rs): level-triggered and idempotent,
// writing with server-side apply under the field manager `kuben`. A failing
// object backs off on its own (per-object exponential backoff) without
// slowing the others.
//
// The plumbing is controller-runtime (watches, caches, work queues); the
// objects written, their order, the status fields, condition reasons and
// messages and the requeue intervals are the Rust controllers'. The pure
// builders are in platform/render.
package controller

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/Teamtem-dev/kuben/api/v1alpha1"
	"github.com/Teamtem-dev/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/kube/discovery"
	"github.com/Teamtem-dev/kuben/internal/kube/render"
	"github.com/Teamtem-dev/kuben/internal/metrics"
)

// KubenConfigName is the name of the cluster-scoped KubenConfig singleton.
const KubenConfigName = "kuben"

// ChosenConfig is the KubenConfig singleton among configs: the one named
// KubenConfigName, else the first; false when there is none.
func ChosenConfig(configs []v1alpha1.KubenConfig) (v1alpha1.KubenConfig, bool) {
	var first opt.Val[v1alpha1.KubenConfig]
	for _, c := range configs {
		if c.Name == KubenConfigName {
			return c, true
		}
		if first.IsNone() {
			first = opt.Some(c)
		}
	}
	return first.Get()
}

// PlatformOf is the platform settings of the KubenConfig singleton among
// configs (ChosenConfig), or the defaults. The App controller and the
// renderer's capability snapshot choose alike.
func PlatformOf(configs []v1alpha1.KubenConfig) render.Platform {
	if c, ok := ChosenConfig(configs); ok {
		return render.PlatformFromSpec(c.Spec)
	}
	return render.DefaultPlatform()
}

// Backoff is the delay before an object that failed failures times in a
// row is reconciled again: 2 s, 4 s, 8 s … capped at 5 minutes (0 counts
// as 1).
func Backoff(failures uint32) time.Duration {
	n := min(max(failures, 1), 16)
	return min(time.Duration(1<<n)*time.Second, backoffCap)
}

// The per-object backoff's first delay and cap.
const (
	backoffBase = 2 * time.Second
	backoffCap  = 5 * time.Minute
)

// RateLimiter is the per-object backoff as a work-queue rate limiter: the
// n-th failure in a row of an item waits Backoff(n); a success resets it.
func RateLimiter() workqueue.TypedRateLimiter[reconcile.Request] {
	return workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](backoffBase, backoffCap)
}

// Condition builds a condition, keeping lastTransitionTime while the status
// is unchanged (so an unchanged reconcile writes an identical status: no
// watch churn). An empty message is left out.
func Condition(clk clock.Clock, previous []v1alpha1.Condition, conditionType string, status bool,
	reason, message string, generation opt.Val[int64],
) v1alpha1.Condition {
	s := "False"
	if status {
		s = "True"
	}
	transition := time.UnixMilli(clk.NowMs()).UTC().Format("2006-01-02T15:04:05Z")
	for _, c := range previous {
		if c.Type == conditionType && c.Status == s {
			if c.LastTransitionTime != nil {
				transition = *c.LastTransitionTime
			}
			break
		}
	}
	out := v1alpha1.Condition{
		Type:               conditionType,
		Status:             s,
		Reason:             &reason,
		LastTransitionTime: &transition,
		ObservedGeneration: generation.Ptr(),
	}
	if message != "" {
		out.Message = &message
	}
	return out
}

// generationOf is metadata.generation, absent while unset (0).
func generationOf(m *metav1.ObjectMeta) opt.Val[int64] {
	if m.Generation == 0 {
		return opt.None[int64]()
	}
	return opt.Some(m.Generation)
}

// shared is the state every reconciler reads (the Rust Ctx).
type shared struct {
	// client writes, and reads the watched kinds from the cache.
	client client.Client
	// reader reads from the API server, where the Rust controllers did.
	reader  client.Reader
	facts   *discovery.Watch
	clock   clock.Clock
	logger  *slog.Logger
	metrics *metrics.Metrics
}

// configs is every KubenConfig in the cache, by name, so "the first" is
// the same on every call.
func (s *shared) configs(ctx context.Context) ([]v1alpha1.KubenConfig, error) {
	var list v1alpha1.KubenConfigList
	if err := s.client.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("listing KubenConfigs: %w", err)
	}
	slices.SortFunc(list.Items, func(a, b v1alpha1.KubenConfig) int { return cmp.Compare(a.Name, b.Name) })
	return list.Items, nil
}

// config is the KubenConfig singleton, if one exists.
func (s *shared) config(ctx context.Context) (v1alpha1.KubenConfig, bool, error) {
	configs, err := s.configs(ctx)
	if err != nil {
		return v1alpha1.KubenConfig{}, false, err
	}
	c, ok := ChosenConfig(configs)
	return c, ok, nil
}

// platform is the platform settings of the KubenConfig singleton, or the
// defaults, gated on the cluster's discovered capabilities.
func (s *shared) platform(ctx context.Context) (render.Platform, error) {
	configs, err := s.configs(ctx)
	if err != nil {
		return render.Platform{}, err
	}
	return Gated(PlatformOf(configs), s.currentFacts()), nil
}

// currentFacts is the cluster's capabilities, once discovered.
func (s *shared) currentFacts() opt.Val[discovery.ClusterFacts] {
	if s.facts == nil {
		return opt.None[discovery.ClusterFacts]()
	}
	f, ok := s.facts.Get()
	if !ok {
		return opt.None[discovery.ClusterFacts]()
	}
	return opt.Some(f)
}

// Gated narrows p to what a cluster with facts can do (Platform.gated);
// without facts the configured intent stands.
func Gated(p render.Platform, facts opt.Val[discovery.ClusterFacts]) render.Platform {
	f, ok := facts.Get()
	if !ok {
		return p.Gated(opt.None[render.IssuerFacts]())
	}
	return p.Gated(opt.Some[render.IssuerFacts](f))
}

// absent reports whether err means the object, or its kind, does not
// exist: a 404, or a kind the API server does not serve (a CRD that is not
// installed answered 404 to the Rust client).
func absent(err error) bool {
	return apierrors.IsNotFound(err) || meta.IsNoMatchError(err)
}

// toUnstructured is v (a JSON-shaped object) as an Unstructured.
func toUnstructured(v any) (*unstructured.Unstructured, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encoding object: %w", err)
	}
	u := &unstructured.Unstructured{}
	if err := u.UnmarshalJSON(data); err != nil {
		return nil, fmt.Errorf("decoding object: %w", err)
	}
	return u, nil
}

// apply server-side-applies obj as the field manager `kuben`, forcing
// ownership of conflicting fields.
func apply(ctx context.Context, c client.Client, obj any) error {
	u, err := toUnstructured(obj)
	if err != nil {
		return err
	}
	if err := c.Apply(ctx, client.ApplyConfigurationFromUnstructured(u),
		client.FieldOwner(v1alpha1.FieldManager), client.ForceOwnership); err != nil {
		return fmt.Errorf("applying %s %s: %w", u.GetKind(), objectName(u), err)
	}
	return nil
}

// applyStatus server-side-applies the status of the kuben.dev object kind
// name (cluster-scoped when namespace is empty) as `kuben`, forcing.
func applyStatus(ctx context.Context, c client.Client, kind, namespace, name string, status any) error {
	metadata := map[string]any{"name": name}
	if namespace != "" {
		metadata["namespace"] = namespace
	}
	u, err := toUnstructured(map[string]any{
		"apiVersion": v1alpha1.SchemeGroupVersion.String(),
		"kind":       kind,
		"metadata":   metadata,
		"status":     status,
	})
	if err != nil {
		return err
	}
	if err := c.Status().Apply(ctx, client.ApplyConfigurationFromUnstructured(u),
		client.FieldOwner(v1alpha1.FieldManager), client.ForceOwnership); err != nil {
		return fmt.Errorf("writing the status of %s %s: %w", kind, objectName(u), err)
	}
	return nil
}

// objectName is `namespace/name`, or `name` for a cluster-scoped object.
func objectName(o client.Object) string {
	if o.GetNamespace() == "" {
		return o.GetName()
	}
	return o.GetNamespace() + "/" + o.GetName()
}

// errMissing is an object without a member the controller needs.
func errMissing(what string) error { return fmt.Errorf("object has no %s", what) }

// policy is the shared error policy around a reconciler: a failure is
// logged with its count and delay; the controller's rate limiter
// (RateLimiter) requeues the object after that delay.
type policy struct {
	kind    string
	inner   reconcile.Reconciler
	limiter workqueue.TypedRateLimiter[reconcile.Request]
	logger  *slog.Logger
	metrics *metrics.Metrics
}

// Reconcile runs the inner reconciler and logs its failure.
func (p policy) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	res, err := p.inner.Reconcile(ctx, req)
	if err == nil {
		return res, nil
	}
	failures := uint32(min(p.limiter.NumRequeues(req)+1, 1<<16)) //nolint:gosec // 1 to 65536
	p.logger.Warn("reconcile failed",
		"kind", p.kind,
		"namespace", req.Namespace,
		"name", req.Name,
		"error", err.Error(),
		"failures", failures,
		"retry_in_s", int64(Backoff(failures)/time.Second))
	p.metrics.ReconcileFailed(p.kind)
	return reconcile.Result{}, err
}
