package projection

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"

	"github.com/Teamtem-dev/kuben/api/v1alpha1"
	"github.com/Teamtem-dev/kuben/internal/kube/registry"
	"github.com/Teamtem-dev/kuben/internal/kube/render"
)

// The informers (projection/informer.rs): one shared watch per kind, with
// label selectors and managedFields stripped, feeding the Projections.
// client-go's reflector lists in pages of 500 (the Rust PAGE_SIZE) and
// re-lists with backoff by itself.

// API groups of the optional kinds.
const (
	GatewayGroup     = "gateway.networking.k8s.io"
	CertManagerGroup = "cert-manager.io"
)

// Timing of the informers.
type Timing struct {
	// FirstSyncDeadline is how long the informers get for their first LIST
	// before Run fails (and its supervisor starts them over). A request
	// that hangs on an API server that has only just come up would
	// otherwise keep Kuben unready until the client's read timeout,
	// minutes later.
	FirstSyncDeadline time.Duration
	// OptionalCheck is how often an optional kind's API group is looked
	// for until it appears.
	OptionalCheck time.Duration
	// Poll is how often a started informer is checked for its first LIST.
	Poll time.Duration
}

// DefaultTiming is the Rust constants.
func DefaultTiming() Timing {
	return Timing{FirstSyncDeadline: 45 * time.Second, OptionalCheck: time.Minute, Poll: 100 * time.Millisecond}
}

// Run runs every informer of cluster (the primary) into p until ctx ends,
// with the default timing.
func Run(ctx context.Context, cluster registry.Cluster, p *Projections, logger *slog.Logger) error {
	return RunWith(ctx, cluster, p, logger, DefaultTiming())
}

// kind is one watched kind: its informer and how its objects become views.
type kind struct {
	name     string
	informer cache.SharedIndexInformer
	synced   cache.InformerSynced
	// replace swaps in the views of every stored object.
	replace func(objects []any)
}

// RunWith is Run with the given timing. It fails when the required kinds
// (pods, projects, environments, apps) have not completed their first LIST
// within the deadline; optional kinds (Gateway API routes, cert-manager
// certificates) are watched once their API group is served and never hold
// back readiness.
func RunWith(ctx context.Context, cluster registry.Cluster, p *Projections, logger *slog.Logger, t Timing) error {
	ctx, cancel := context.WithCancel(ctx)
	typed := informers.NewSharedInformerFactoryWithOptions(cluster.Typed, 0,
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = v1alpha1.ManagedSelector }),
		informers.WithTransform(stripManagedFields))
	kuben := dynamicinformer.NewDynamicSharedInformerFactory(cluster.Dynamic, 0)
	managed := dynamicinformer.NewFilteredDynamicSharedInformerFactory(cluster.Dynamic, 0, metav1.NamespaceAll,
		func(o *metav1.ListOptions) { o.LabelSelector = v1alpha1.ManagedSelector })
	defer func() {
		cancel()
		typed.Shutdown()
		kuben.Shutdown()
		managed.Shutdown()
	}()

	required := []kind{
		podKind(typed.Core().V1().Pods().Informer(), p, logger),
		kubenKind(kuben, v1alpha1.ProjectResource, p.UpsertProject, p.RemoveProject, p.ReplaceProjects,
			ProjectViewOf, clusterKey, logger),
		kubenKind(kuben, v1alpha1.EnvironmentResource, p.UpsertEnvironment, p.RemoveEnvironment, p.ReplaceEnvironments,
			EnvironmentViewOf, clusterKey, logger),
		kubenKind(kuben, v1alpha1.AppResource, p.UpsertApp, p.RemoveApp, p.ReplaceApps,
			AppViewOf, namespacedKey, logger),
	}
	for _, k := range required {
		if k.synced == nil {
			return fmt.Errorf("%s informer: no event handler", k.name)
		}
	}
	typed.Start(ctx.Done())
	kuben.Start(ctx.Done())
	if err := firstSync(ctx, p, required, t); err != nil {
		return err
	}
	logger.Info("informers synced")
	optional := []optionalKind{
		{group: GatewayGroup, start: func() kind { return routeKind(managed, p, logger) }},
		{group: CertManagerGroup, start: func() kind { return certificateKind(kuben, p, logger) }},
	}
	watchOptional(ctx, cluster.Typed.Discovery(), optional, []func(<-chan struct{}){managed.Start, kuben.Start}, t, logger)
	return nil
}

// firstSync swaps in each required kind as soon as its first LIST is
// complete, and fails at the deadline naming those still listing.
func firstSync(ctx context.Context, p *Projections, kinds []kind, t Timing) error {
	deadline := time.NewTimer(t.FirstSyncDeadline)
	defer deadline.Stop()
	tick := time.NewTicker(t.Poll)
	defer tick.Stop()
	pending := slices.Clone(kinds)
	for {
		pending = slices.DeleteFunc(pending, func(k kind) bool {
			if !k.synced() {
				return false
			}
			k.replace(k.informer.GetStore().List())
			return true
		})
		if len(pending) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-deadline.C:
			return fmt.Errorf("no complete LIST within %ds for: %s",
				int(t.FirstSyncDeadline/time.Second), strings.Join(p.PendingKinds(), ", "))
		case <-tick.C:
		}
	}
}

// optionalKind is a kind that may not be installed.
type optionalKind struct {
	group string
	start func() kind
	// running is set once the informer was created.
	running *kind
	synced  bool
}

// watchOptional looks for each optional kind's API group every
// OptionalCheck and watches the kind once it is served, until ctx ends.
func watchOptional(ctx context.Context, d discovery.DiscoveryInterface, kinds []optionalKind,
	starts []func(<-chan struct{}), t Timing, logger *slog.Logger,
) {
	for {
		waiting := false
		var groups []string
		for i := range kinds {
			k := &kinds[i]
			if k.running == nil {
				if groups == nil {
					groups = servedGroups(d, logger)
				}
				if slices.Contains(groups, k.group) {
					started := k.start()
					k.running = &started
					for _, start := range starts {
						start(ctx.Done())
					}
				}
			}
			if k.running != nil && !k.synced && k.running.synced != nil && k.running.synced() {
				k.running.replace(k.running.informer.GetStore().List())
				k.synced = true
				logger.Info("informer synced", "kind", k.running.name)
			}
			waiting = waiting || (k.running != nil && !k.synced)
		}
		wait := t.OptionalCheck
		if waiting {
			wait = t.Poll
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func servedGroups(d discovery.DiscoveryInterface, logger *slog.Logger) []string {
	list, err := d.ServerGroups()
	if err != nil {
		logger.Debug("cannot list API groups", "error", err)
		return []string{}
	}
	groups := make([]string, 0, len(list.Groups))
	for _, g := range list.Groups {
		groups = append(groups, g.Name)
	}
	return groups
}

// stripManagedFields keeps memory low: managedFields are most of a small
// object and no view reads them.
func stripManagedFields(obj any) (any, error) {
	if m, err := meta.Accessor(obj); err == nil {
		m.SetManagedFields(nil)
	}
	return obj, nil
}

// objectKey is the namespace and name of a stored or deleted object.
func objectKey(obj any) (namespace, name string, ok bool) {
	if gone, isGone := obj.(cache.DeletedFinalStateUnknown); isGone {
		ns, n, err := cache.SplitMetaNamespaceKey(gone.Key)
		return ns, n, err == nil
	}
	m, err := meta.Accessor(obj)
	if err != nil {
		return "", "", false
	}
	return m.GetNamespace(), m.GetName(), true
}

func namespacedKey(namespace, name string) string { return namespace + "/" + name }

func clusterKey(_, name string) string { return name }

// handlers turns informer events into upserts and removes. Adds of the
// initial LIST are left to the replace at the first sync, so readers never
// see a half-filled set; later re-lists arrive as individual events.
func handlers[V any](view func(obj any) (V, bool), upsert func(V), remove func(string),
	key func(namespace, name string) string,
) cache.ResourceEventHandler {
	return cache.ResourceEventHandlerDetailedFuncs{
		AddFunc: func(obj any, initial bool) {
			if v, ok := view(obj); ok && !initial {
				upsert(v)
			}
		},
		UpdateFunc: func(_, obj any) {
			if v, ok := view(obj); ok {
				upsert(v)
			}
		},
		DeleteFunc: func(obj any) {
			if ns, name, ok := objectKey(obj); ok {
				remove(key(ns, name))
			}
		},
	}
}

func register[V any](name string, inf cache.SharedIndexInformer, view func(obj any) (V, bool), upsert func(V),
	remove func(string), replace func([]V), key func(namespace, name string) string, logger *slog.Logger,
) kind {
	k := kind{name: name, informer: inf, replace: func(objects []any) {
		views := make([]V, 0, len(objects))
		for _, o := range objects {
			if v, ok := view(o); ok {
				views = append(views, v)
			}
		}
		replace(views)
		logger.Info("informer listed", "kind", name, "objects", len(views))
	}}
	reg, err := inf.AddEventHandler(handlers(view, upsert, remove, key))
	if err != nil || reg == nil {
		logger.Error("cannot watch", "kind", name, "error", err)
		return k
	}
	k.synced = reg.HasSynced
	return k
}

func podKind(inf cache.SharedIndexInformer, p *Projections, logger *slog.Logger) kind {
	view := func(obj any) (PodView, bool) {
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			return PodView{}, false
		}
		return PodViewOf(pod), true
	}
	return register("pods", inf, view, p.UpsertPod, p.RemovePod, p.ReplacePods, namespacedKey, logger)
}

// decode reads an unstructured kuben.dev object into its type, through
// the type's own JSON decoder (which applies the serde defaults).
func decode[T any](obj any) (T, error) {
	var out T
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return out, fmt.Errorf("unexpected %T", obj)
	}
	data, err := u.MarshalJSON()
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, err
	}
	return out, nil
}

func kubenKind[T any, V any](f dynamicinformer.DynamicSharedInformerFactory, resource string,
	upsert func(V), remove func(string), replace func([]V), of func(*T) V,
	key func(namespace, name string) string, logger *slog.Logger,
) kind {
	gvr := v1alpha1.SchemeGroupVersion.WithResource(resource)
	inf := f.ForResource(gvr).Informer()
	if err := inf.SetTransform(stripManagedFields); err != nil {
		logger.Warn("cannot strip managedFields", "resource", resource, "error", err)
	}
	view := func(obj any) (V, bool) {
		t, err := decode[T](obj)
		if err != nil {
			// The Rust watcher reported an undecodable object and went on.
			logger.Warn("watch failed; skipping object", "resource", resource, "error", err)
			var zero V
			return zero, false
		}
		return of(&t), true
	}
	return register(resource, inf, view, upsert, remove, replace, key, logger)
}

func routeKind(f dynamicinformer.DynamicSharedInformerFactory, p *Projections, logger *slog.Logger) kind {
	inf := f.ForResource(schema.GroupVersionResource{Group: GatewayGroup, Version: "v1", Resource: "httproutes"}).Informer()
	if err := inf.SetTransform(stripManagedFields); err != nil {
		logger.Warn("cannot strip managedFields", "resource", "httproutes", "error", err)
	}
	view := func(obj any) (RouteView, bool) {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			return RouteView{}, false
		}
		return RouteViewOf(u), true
	}
	return register("httproutes", inf, view, p.UpsertRoute, p.RemoveRoute, p.ReplaceRoutes, namespacedKey, logger)
}

// certificateKind watches only the certificates Kuben's gateway orders; a
// cluster may hold many others.
func certificateKind(f dynamicinformer.DynamicSharedInformerFactory, p *Projections, logger *slog.Logger) kind {
	inf := f.ForResource(schema.GroupVersionResource{Group: CertManagerGroup, Version: "v1", Resource: "certificates"}).Informer()
	if err := inf.SetTransform(stripManagedFields); err != nil {
		logger.Warn("cannot strip managedFields", "resource", "certificates", "error", err)
	}
	view := func(obj any) (CertificateView, bool) {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok || !strings.HasPrefix(u.GetName(), render.TLSSecretPrefix) {
			return CertificateView{}, false
		}
		return CertificateViewOf(u), true
	}
	remove := func(key string) {
		if _, name, ok := strings.Cut(key, "/"); ok && strings.HasPrefix(name, render.TLSSecretPrefix) {
			p.RemoveCertificate(key)
		}
	}
	return register("certificates", inf, view, p.UpsertCertificate, remove, p.ReplaceCertificates, namespacedKey, logger)
}
