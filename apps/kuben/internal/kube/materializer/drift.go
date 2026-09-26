package materializer

// Drift (ADR-032): changes someone else made to a materialized App object.
//
// A watch on the App objects Kuben manages compares each with what SQL
// renders for it. An object whose spec or generation annotation differs
// from the rendering of its last materialized run, or that was deleted, was
// changed by someone else. The drift is recorded on the target with the
// field managers that own fields of the object, and the object is written
// again under the same fence as a delivery. While a newer run of the
// target is in flight, the object is left to that run. The watch runs once
// per installation, next to the controllers under the leader lease.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ops/target"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/packages/api/v1alpha1"
)

// Finding is what a drift check found.
//
//sumtype:decl
type Finding interface{ isFinding() }

type (
	// Clean means the object is as SQL renders it.
	Clean struct{}
	// Drift means someone else changed or deleted it.
	Drift struct {
		Deleted     bool
		SpecChanged bool
		// AnnotationChanged: the generation annotation is not the written
		// one.
		AnnotationChanged bool
		// Managers are the field managers other than the materializer that
		// own fields of the object (not of its status), sorted.
		Managers []string
	}
)

func (Clean) isFinding() {}
func (Drift) isFinding() {}

// Describe is the description recorded on the target.
func (d Drift) Describe() map[string]any {
	managers := make([]any, 0, len(d.Managers))
	for _, m := range d.Managers {
		managers = append(managers, m)
	}
	return map[string]any{
		"deleted": d.Deleted, "specChanged": d.SpecChanged, "annotationChanged": d.AnnotationChanged,
		"managers": managers,
	}
}

// Untouched reports whether live is exactly as the last write left it: its
// metadata.generation and generation annotation are the recorded ones.
func Untouched(live *v1alpha1.App, written store.Materialized) bool {
	return live.Generation == written.ResourceGeneration &&
		LiveGeneration(live.Annotations) == opt.Some(uint64(written.Generation))
}

// Compare compares live (none: deleted) with desired, the rendering of the
// last materialized run. Specs compare after the same typed round trip, so
// defaults the API server filled in are no difference.
func Compare(live opt.Val[*v1alpha1.App], desired *v1alpha1.App) Finding {
	app, ok := live.Get()
	if !ok || app == nil {
		return Drift{Deleted: true}
	}
	specChanged := !sameJSON(app.Spec, desired.Spec)
	live1, has1 := app.Annotations[v1alpha1.AnnotationGeneration]
	want, has2 := desired.Annotations[v1alpha1.AnnotationGeneration]
	annotationChanged := has1 != has2 || live1 != want
	if !specChanged && !annotationChanged {
		return Clean{}
	}
	return Drift{SpecChanged: specChanged, AnnotationChanged: annotationChanged, Managers: managers(app)}
}

// sameJSON compares a and b as the JSON they write; one that does not
// serialize is never the same.
func sameJSON(a, b any) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && bytes.Equal(x, y)
}

func managers(app *v1alpha1.App) []string {
	var out []string
	for _, e := range app.ManagedFields {
		if e.Subresource == "" && e.Manager != "" && e.Manager != FieldManager && !slices.Contains(out, e.Manager) {
			out = append(out, e.Manager)
		}
	}
	slices.Sort(out)
	return out
}

// WatchDrift watches the App objects Kuben manages until ctx ends. A
// restarted watch lists every object again, so each is checked on start.
// Objects are checked one at a time, in the order they were touched.
func WatchDrift(ctx context.Context, w *Worker) error {
	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(w.d.Cluster.Dynamic, 0, metav1.NamespaceAll,
		func(o *metav1.ListOptions) { o.LabelSelector = v1alpha1.ManagedSelector })
	defer factory.Shutdown()
	check := func(obj any) {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok || u == nil {
			return
		}
		touched, err := decode[v1alpha1.App](u)
		if err != nil {
			w.d.Logger.Warn("drift check failed", "app", u.GetName(), "error", err)
			return
		}
		if _, err := w.CheckDrift(ctx, &touched); err != nil {
			w.d.Logger.Warn("drift check failed", "app", touched.Name, "error", err)
		}
	}
	informer := factory.ForResource(appsGVR).Informer()
	if err := informer.SetWatchErrorHandler(func(_ *cache.Reflector, err error) {
		w.d.Logger.Warn("drift watch interrupted; resuming", "error", err)
	}); err != nil {
		return fmt.Errorf("drift watch: %w", err)
	}
	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { check(obj) },
		UpdateFunc: func(_, obj any) { check(obj) },
	}); err != nil {
		return fmt.Errorf("drift watch: %w", err)
	}
	factory.Start(ctx.Done())
	<-ctx.Done()
	return nil
}

// CheckDrift checks the App object touched, as the watch last saw it, for
// drift, and replaces it with its rendering when it drifted.
func (w *Worker) CheckDrift(ctx context.Context, touched *v1alpha1.App) (Finding, error) {
	uid, namespace := string(touched.UID), touched.Namespace
	if uid == "" || namespace == "" {
		return Clean{}, nil
	}
	written, found, err := w.d.Store.MaterializedResource(ctx, uid)
	switch {
	case err != nil:
		return nil, storeError(err)
	case !found:
		// Not materialized: an object of the resource model, left alone.
		return Clean{}, nil
	}
	app, exists, err := get[v1alpha1.App](ctx, w.resource(appsGVR, namespace), touched.Name)
	if err != nil {
		return nil, kubeError(err)
	}
	live := opt.None[*v1alpha1.App]()
	if exists && string(app.UID) == uid {
		live = opt.Some(&app)
		if Untouched(&app, written) {
			return Clean{}, nil
		}
	}
	m, desired, clean, err := w.driftDesired(ctx, written)
	if err != nil || clean {
		return Clean{}, err
	}
	finding := Compare(live, &desired.App)
	drift, drifted := finding.(Drift)
	if !drifted {
		return finding, nil
	}
	replaced := opt.None[store.Replacement]()
	written2, st := w.writeApp(ctx, &m, &desired.App)
	switch s := st.(type) {
	case nil:
		if written2.UID != "" && written2.Generation != 0 {
			replaced = opt.Some(store.Replacement{UID: string(written2.UID), Generation: written2.Generation})
		}
	case stopRetry:
		return nil, s.err
	case stopFenced, stopShutdown, stopSettled, stopRefused, stopSuperseded, stopWait:
		w.d.Logger.Info("drift left in place", "target", m.Target, "stop", fmt.Sprintf("%#v", st))
	}
	if _, err := w.d.Store.RecordDrift(ctx, written, replaced, drift.Describe()); err != nil {
		return nil, storeError(err)
	}
	w.d.Logger.Warn("drift on a materialized App", "target", m.Target, "managers", drift.Managers,
		"deleted", drift.Deleted, "replaced", replaced.IsSome())
	return finding, nil
}

// driftDesired is the run written last and its rendering; clean when the
// object is not the materializer's to put back.
func (w *Worker) driftDesired(ctx context.Context, written store.Materialized) (store.Materialization, Rendered, bool, error) {
	type read struct {
		state    target.State
		m        store.Materialization
		complete bool
	}
	r, err := func() (read, error) {
		t, err := w.d.Store.Tenant(ctx, written.Org)
		if err != nil {
			return read{}, err //nolint:wrapcheck // said as a store error
		}
		defer t.Rollback(ctx) //nolint:errcheck // read only
		var out read
		var hasState, hasRun bool
		if out.state, hasState, err = t.TargetState(ctx, written.Target); err != nil {
			return out, err //nolint:wrapcheck // said as a store error
		}
		out.m, hasRun, err = t.Materialization(ctx, written.Operation)
		out.complete = hasState && hasRun
		return out, err //nolint:wrapcheck // said as a store error
	}()
	m := r.m
	switch {
	case err != nil:
		return m, Rendered{}, false, storeError(err)
	case !r.complete:
		return m, Rendered{}, true, nil
	case m.Delivery == store.DeliveryAgent:
		// Handed over to the cluster's agent: the App is gone on purpose.
		return m, Rendered{}, true, nil
	case m.Deleting:
		// Its deletion removes the object; nothing to put back.
		return m, Rendered{}, true, nil
	case r.state.DesiredGeneration != written.Generation:
		// A newer run is in flight; its delivery writes the object.
		return m, Rendered{}, true, nil
	}
	desired, rerr := Render(&m)
	if rerr != nil {
		w.d.Logger.Warn("cannot render the written run again", "target", m.Target, "error", rerr)
		return m, Rendered{}, true, nil
	}
	return m, desired, false, nil
}
