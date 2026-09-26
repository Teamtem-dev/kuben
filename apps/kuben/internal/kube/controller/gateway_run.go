package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/projection"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/render"
	"github.com/Teamtem-dev/kuben/packages/api/v1alpha1"
)

// The gateway reconciler is not a controller-runtime controller: it has one
// object to write, from the projections' routes, debounced.

// Timing of the gateway reconciler.
const (
	// gatewayDebounce: at most one apply this often, skipped when nothing
	// changed.
	gatewayDebounce = 2 * time.Second
	// gatewayStatusRefresh is how often the Gateway's readiness is read
	// again.
	gatewayStatusRefresh = 30 * time.Second
	// gatewayResync is how often the Gateway is written even when the body
	// did not change.
	gatewayResync = 5 * time.Minute
)

func gatewayKind() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: gatewayGroup, Version: "v1", Kind: "Gateway"}
}

type gatewayReconciler struct {
	*shared
	projections *projection.Projections
	// last is the body applied last; absent forces the next apply.
	last opt.Val[[]byte]
}

// gatewayAt reads a Gateway API object from the API server; absent when it,
// or its kind, does not exist.
func (r *gatewayReconciler) gatewayAt(ctx context.Context, gvk schema.GroupVersionKind, namespace, name string) (
	opt.Val[*unstructured.Unstructured], error,
) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	if err := r.reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, obj); err != nil {
		if absent(err) {
			return opt.None[*unstructured.Unstructured](), nil
		}
		return opt.None[*unstructured.Unstructured](), fmt.Errorf("reading %s %s/%s: %w", gvk.Kind, namespace, name, err)
	}
	return opt.Some(obj), nil
}

// report records verdict as the Gateway condition of KubenConfig (none:
// drops it), writing only on change.
func (r *gatewayReconciler) report(ctx context.Context, verdict opt.Val[Verdict]) error {
	config, ok, err := r.config(ctx)
	if err != nil || !ok {
		return err
	}
	var previous []v1alpha1.Condition
	if config.Status != nil {
		previous = config.Status.Conditions
	}
	conditions := make([]v1alpha1.Condition, 0, len(previous)+1)
	for _, c := range previous {
		if c.Type != GatewayCondition {
			conditions = append(conditions, c)
		}
	}
	generation := generationOf(&config.ObjectMeta)
	if v, ok := verdict.Get(); ok {
		conditions = append(conditions, Condition(r.clock, previous, GatewayCondition, v.OK, v.Reason, v.Message, generation))
	}
	if conditionsEqual(conditions, previous) {
		return nil
	}
	patch, err := json.Marshal(map[string]any{"status": map[string]any{
		"observedGeneration": generation.Ptr(),
		"conditions":         v1alpha1.Conditions(conditions),
	}})
	if err != nil {
		return fmt.Errorf("encoding the KubenConfig status: %w", err)
	}
	target := &v1alpha1.KubenConfig{}
	target.Name = config.Name
	if err := r.client.Status().Patch(ctx, target, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("writing the status of KubenConfig %s: %w", config.Name, err)
	}
	return nil
}

// conditionsEqual compares conditions member by member.
func conditionsEqual(a, b []v1alpha1.Condition) bool {
	if len(a) != len(b) {
		return false
	}
	same := func(x, y *string) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	for i := range a {
		x, y := a[i], b[i]
		if x.Type != y.Type || x.Status != y.Status || !same(x.Reason, y.Reason) || !same(x.Message, y.Message) ||
			!same(x.LastTransitionTime, y.LastTransitionTime) ||
			(x.ObservedGeneration == nil) != (y.ObservedGeneration == nil) ||
			(x.ObservedGeneration != nil && y.ObservedGeneration != nil && *x.ObservedGeneration != *y.ObservedGeneration) {
			return false
		}
	}
	return true
}

// syncRedirect keeps the redirect route while TLS is on and removes
// Kuben's when it is off.
func (r *gatewayReconciler) syncRedirect(ctx context.Context, gateway render.GatewayRef, tls bool) error {
	if tls {
		return apply(ctx, r.client, RedirectRouteFor(gateway))
	}
	live, err := r.gatewayAt(ctx, httpRouteKind(), gateway.Namespace, RedirectRoute)
	if err != nil {
		return err
	}
	route, ok := live.Get()
	if !ok || route == nil || route.GetLabels()[v1alpha1.LabelManagedBy] != v1alpha1.LabelManagerValue {
		return nil
	}
	if err := r.client.Delete(ctx, route); err != nil && !absent(err) {
		return fmt.Errorf("deleting the redirect route: %w", err)
	}
	return nil
}

// reconcile writes the Gateway's listeners and reports its readiness.
func (r *gatewayReconciler) reconcile(ctx context.Context) error {
	platform, err := r.platform(ctx)
	if err != nil {
		return err
	}
	facts := r.currentFacts()
	gateway, ok := platform.Gateway.Get()
	if !ok {
		return r.report(ctx, opt.None[Verdict]()) // Apps are not exposed; nothing to say.
	}
	if f, known := facts.Get(); known && f.GatewayAPI.IsNone() && len(f.Unknown) == 0 {
		why := "the Gateway API CRDs are not installed (standard channel)"
		return r.report(ctx, opt.Some(Verdict{false, "GatewayAPIMissing", why}))
	}
	live, err := r.gatewayAt(ctx, gatewayKind(), gateway.Namespace, gateway.Name)
	if err != nil {
		return err
	}
	if verdict, refused := Refusal(gateway, platform, OwnershipOf(live)).Get(); refused {
		r.last = opt.None[[]byte]()
		return r.report(ctx, opt.Some(verdict))
	}
	plan := PlanListeners(RoutedHosts(r.projections.Routes(), gateway), platform)
	for _, conflict := range plan.Conflicts {
		r.logger.Warn("hostname requested by two namespaces; the first keeps it", "conflict", conflict)
	}
	if len(plan.Skipped) > 0 {
		r.logger.Warn("gateway listener limit reached", "hosts", plan.Skipped, "max", MaxListeners)
	}
	if err := r.applyListeners(ctx, gateway, platform, plan, live.IsNone()); err != nil {
		return err
	}
	live, err = r.gatewayAt(ctx, gatewayKind(), gateway.Namespace, gateway.Name)
	if err != nil {
		return err
	}
	verdict := Verdict{false, "Pending", "the Gateway is being created"}
	if g, ok := live.Get(); ok && g != nil {
		verdict = Readiness(g, platform, facts)
	}
	return r.report(ctx, opt.Some(verdict))
}

// applyListeners writes the Gateway's listeners when they changed since
// the last write (or the Gateway is missing), and the HTTP→HTTPS redirect
// with them. A refused write is reported on KubenConfig as
// GatewayWriteFailed before it is returned.
func (r *gatewayReconciler) applyListeners(ctx context.Context, gateway render.GatewayRef, platform render.Platform, plan ListenerPlan, missing bool) error {
	body := GatewayPatch(gateway, platform, plan)
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encoding the Gateway: %w", err)
	}
	last, applied := r.last.Get()
	if missing || !applied || !bytes.Equal(last, encoded) {
		if err := apply(ctx, r.client, body); err != nil {
			cause := err
			if inner := errors.Unwrap(err); inner != nil {
				cause = inner // the API server's words, as the Rust message had them
			}
			if rerr := r.report(ctx, opt.Some(Verdict{false, "GatewayWriteFailed", cause.Error()})); rerr != nil {
				return rerr
			}
			return err
		}
		if err := r.syncRedirect(ctx, gateway, platform.TLS); err != nil {
			return err
		}
		r.logger.Info("gateway listeners applied",
			"gateway", gateway.Namespace+"/"+gateway.Name, "listeners", len(plan.Listeners), "tls", platform.TLS)
		r.last = opt.Some(encoded)
	}
	return nil
}

// run is the debounced loop: any projection change marks the listener set
// dirty; at most one apply every 2 s, skipped when nothing changed;
// readiness read every 30 s; full resync every 5 minutes. It returns nil
// when ctx ends.
func (r *gatewayReconciler) run(ctx context.Context) error {
	sub := r.projections.Subscribe()
	defer sub.Close()
	changed := make(chan struct{}, 1)
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		for {
			if _, ok := sub.Recv(ctx); !ok {
				return nil
			}
			select {
			case changed <- struct{}{}:
			default:
			}
		}
	})
	g.Go(func() error { return r.loop(ctx, changed) })
	if err := g.Wait(); err != nil {
		return fmt.Errorf("gateway reconciler: %w", err)
	}
	return nil
}

func (r *gatewayReconciler) loop(ctx context.Context, changed <-chan struct{}) error {
	debounce := time.NewTicker(gatewayDebounce)
	defer debounce.Stop()
	status := time.NewTicker(gatewayStatusRefresh)
	defer status.Stop()
	resync := time.NewTicker(gatewayResync)
	defer resync.Stop()
	dirty := true
	flush := func() {
		if !dirty {
			return
		}
		dirty = false
		if err := r.reconcile(ctx); err != nil && ctx.Err() == nil {
			r.logger.Warn("gateway reconcile failed; retrying", "error", err.Error())
			r.last = opt.None[[]byte]()
		}
	}
	flush() // the Rust intervals tick at once
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-changed:
			dirty = true
		case <-status.C:
			dirty = true
		case <-resync.C:
			dirty = true
			r.last = opt.None[[]byte]()
		case <-debounce.C:
			flush()
		}
	}
}
