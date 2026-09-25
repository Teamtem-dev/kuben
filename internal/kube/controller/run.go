package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/Teamtem-dev/kuben/api/v1alpha1"
	"github.com/Teamtem-dev/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/internal/health"
	"github.com/Teamtem-dev/kuben/internal/kube/discovery"
	"github.com/Teamtem-dev/kuben/internal/kube/projection"
	"github.com/Teamtem-dev/kuben/internal/kube/registry"
)

// Subsystem is the health entry of the controllers.
const Subsystem = "controllers"

// workers is how many objects of one kind are reconciled at once (never
// the same object twice at once).
const workers = 4

// Deps is what the controllers work with.
type Deps struct {
	Logger *slog.Logger
	// Cluster is the primary cluster; its Config is what the controllers
	// connect with.
	Cluster registry.Cluster
	// Projections are read by the gateway controller (routed hosts).
	Projections *projection.Projections
	// Facts is the discovered cluster capabilities.
	Facts  *discovery.Watch
	Health *health.Health
	Clock  clock.Clock
}

// Scheme is the types the controllers read and write: client-go's and
// kuben.dev/v1alpha1.
func Scheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, fmt.Errorf("client-go scheme: %w", err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		return nil, fmt.Errorf("kuben.dev scheme: %w", err)
	}
	return s, nil
}

// RunAll applies the CRDs, then runs the project, environment, gateway and
// app controllers until ctx ends; every App is reconciled again when the
// KubenConfig singleton or the discovered facts change. It returns nil
// once ctx ends. The caller elects the leader (kube/leader) and
// supervises: an error means the set stopped and should be restarted.
func RunAll(ctx context.Context, d Deps) error {
	if d.Cluster.Config == nil {
		return errors.New("controllers: the cluster has no REST config")
	}
	if d.Logger == nil || d.Clock == nil || d.Projections == nil || d.Facts == nil || d.Health == nil {
		return errors.New("controllers: incomplete dependencies")
	}
	scheme, err := Scheme()
	if err != nil {
		return err
	}
	direct, err := client.New(d.Cluster.Config, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("controllers: client: %w", err)
	}
	if err := EnsureCRDs(ctx, direct, d.Logger); err != nil {
		return err
	}
	mgr, err := newManager(d, scheme)
	if err != nil {
		return err
	}
	s := &shared{
		client:  mgr.GetClient(),
		reader:  mgr.GetAPIReader(),
		facts:   d.Facts,
		clock:   d.Clock,
		logger:  d.Logger,
		metrics: d.Health.Metrics(),
	}
	if err := register(mgr, s, d.Projections); err != nil {
		return err
	}
	d.Health.OK(Subsystem)
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("controllers: %w", err)
	}
	return nil
}

// newManager is a manager without leader election (the caller elects), without
// metrics or health listeners, whose cache drops managedFields and holds
// only the built-in objects Kuben manages.
func newManager(d Deps, scheme *runtime.Scheme) (manager.Manager, error) {
	managed := labels.SelectorFromSet(labels.Set{v1alpha1.LabelManagedBy: v1alpha1.LabelManagerValue})
	skipNameValidation := true // RunAll is restarted in the same process
	mgr, err := ctrl.NewManager(d.Cluster.Config, manager.Options{
		Scheme:                 scheme,
		Logger:                 logr.FromSlogHandler(d.Logger.Handler()),
		LeaderElection:         false,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Controller:             config.Controller{SkipNameValidation: &skipNameValidation},
		Cache: cache.Options{
			DefaultTransform: cache.TransformStripManagedFields(),
			ByObject: map[client.Object]cache.ByObject{
				&corev1.Namespace{}:  {Label: managed},
				&appsv1.Deployment{}: {Label: managed},
				&corev1.Service{}:    {Label: managed},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("controllers: manager: %w", err)
	}
	return mgr, nil
}

// register adds the four controllers to mgr.
func register(mgr manager.Manager, s *shared, projections *projection.Projections) error {
	withPolicy := func(kind string, inner reconcile.Reconciler) (reconcile.Reconciler, controller.Options) {
		limiter := RateLimiter()
		return policy{kind: kind, inner: inner, limiter: limiter, logger: s.logger, metrics: s.metrics},
			controller.Options{RateLimiter: limiter, MaxConcurrentReconciles: workers}
	}

	project, opts := withPolicy(v1alpha1.ProjectKind, projectReconciler{s})
	if err := ctrl.NewControllerManagedBy(mgr).Named("project").WithOptions(opts).
		For(&v1alpha1.Project{}).
		Watches(&v1alpha1.Environment{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
			env, ok := o.(*v1alpha1.Environment)
			if !ok {
				return nil
			}
			return projectOfEnvironment(ctx, env)
		})).
		Complete(project); err != nil {
		return fmt.Errorf("controllers: project: %w", err)
	}

	environment, opts := withPolicy(v1alpha1.EnvironmentKind, environmentReconciler{s})
	if err := ctrl.NewControllerManagedBy(mgr).Named("environment").WithOptions(opts).
		For(&v1alpha1.Environment{}).
		// Repair drift: a deleted or relabelled namespace re-triggers its environment.
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
			ns, ok := o.(*corev1.Namespace)
			if !ok {
				return nil
			}
			return environmentOfNamespace(ctx, ns)
		})).
		Complete(environment); err != nil {
		return fmt.Errorf("controllers: environment: %w", err)
	}

	if err := registerApps(mgr, s, withPolicy); err != nil {
		return err
	}

	gateway := &gatewayReconciler{shared: s, projections: projections}
	if err := mgr.Add(manager.RunnableFunc(gateway.run)); err != nil {
		return fmt.Errorf("controllers: gateway: %w", err)
	}
	return nil
}

// registerApps adds the App controller, which also reconciles every App
// when the KubenConfig singleton or the discovered facts change.
func registerApps(mgr manager.Manager, s *shared,
	withPolicy func(string, reconcile.Reconciler) (reconcile.Reconciler, controller.Options),
) error {
	everyApp := func(ctx context.Context) []reconcile.Request {
		var apps v1alpha1.AppList
		if err := s.client.List(ctx, &apps); err != nil {
			s.logger.Warn("listing apps to reconcile them all failed", "error", err.Error())
			return nil
		}
		out := make([]reconcile.Request, 0, len(apps.Items))
		for i := range apps.Items {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&apps.Items[i])})
		}
		return out
	}
	factsChanged := make(chan event.TypedGenericEvent[struct{}])
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		return forwardFactChanges(ctx, s.facts, factsChanged)
	})); err != nil {
		return fmt.Errorf("controllers: facts: %w", err)
	}

	app, opts := withPolicy(v1alpha1.AppKind, appReconciler{s})
	if err := ctrl.NewControllerManagedBy(mgr).Named("app").WithOptions(opts).
		For(&v1alpha1.App{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Watches(&v1alpha1.KubenConfig{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
			return everyApp(ctx)
		})).
		WatchesRawSource(source.Channel(factsChanged,
			handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, _ struct{}) []reconcile.Request {
				return everyApp(ctx)
			}))).
		Complete(app); err != nil {
		return fmt.Errorf("controllers: app: %w", err)
	}
	return nil
}

// forwardFactChanges sends an event on out whenever the facts change, until
// ctx ends.
func forwardFactChanges(ctx context.Context, facts *discovery.Watch, out chan<- event.TypedGenericEvent[struct{}]) error {
	for {
		changed := facts.Changed()
		select {
		case <-ctx.Done():
			return nil
		case <-changed:
		}
		select {
		case <-ctx.Done():
			return nil
		case out <- event.TypedGenericEvent[struct{}]{}:
		}
	}
}
