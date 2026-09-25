package setup

// What setup does in a Kubernetes cluster, behind an interface so the
// platform logic is tested without one. Objects are unstructured and their
// kind is resolved by discovery on every call, as kube's pinned_kind did:
// setup installs CRDs while it runs.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

// cluster reads and writes objects of any kind.
type cluster interface {
	// get reads one object; false when there is none.
	get(ctx context.Context, apiVersion, kind, namespace, name string) (*unstructured.Unstructured, bool, error)
	// apply writes obj by server-side apply as fieldManager; force takes
	// fields over from other managers.
	apply(ctx context.Context, obj *unstructured.Unstructured, force bool) (*unstructured.Unstructured, error)
	// remove deletes one object; a missing one is no error.
	remove(ctx context.Context, apiVersion, kind, namespace, name string) error
	// list lists objects of a kind in namespace (every namespace when
	// empty) matching selector.
	list(ctx context.Context, apiVersion, kind, namespace, selector string) ([]unstructured.Unstructured, error)
}

// kubeCluster is a real cluster.
type kubeCluster struct {
	dyn   dynamic.Interface
	disco discovery.DiscoveryInterface
}

// connectKube is a client for kubeconfig (its current context).
func connectKube(kubeconfig string) (cluster, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, err //nolint:wrapcheck // explains itself
	}
	cfg.UserAgent = "kuben"
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err //nolint:wrapcheck // explains itself
	}
	disco, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err //nolint:wrapcheck // explains itself
	}
	return kubeCluster{dyn: dyn, disco: disco}, nil
}

// resource is the kind's resource and whether it is namespaced.
func (k kubeCluster) resource(apiVersion, kind string) (schema.GroupVersionResource, bool, error) {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return schema.GroupVersionResource{}, false, err //nolint:wrapcheck // explains itself
	}
	list, err := k.disco.ServerResourcesForGroupVersion(apiVersion)
	if err != nil {
		return schema.GroupVersionResource{}, false, err //nolint:wrapcheck // explains itself
	}
	for _, r := range list.APIResources {
		if r.Kind == kind && !strings.Contains(r.Name, "/") {
			return gv.WithResource(r.Name), r.Namespaced, nil
		}
	}
	return schema.GroupVersionResource{}, false, fmt.Errorf("the server has no resource of kind %s in %s", kind, apiVersion)
}

// at is the resource interface for an object in namespace ("default" for
// a namespaced kind without one, as kube did).
func (k kubeCluster) at(apiVersion, kind, namespace string) (dynamic.ResourceInterface, error) {
	gvr, namespaced, err := k.resource(apiVersion, kind)
	if err != nil {
		return nil, err
	}
	if !namespaced {
		return k.dyn.Resource(gvr), nil
	}
	if namespace == "" {
		namespace = "default"
	}
	return k.dyn.Resource(gvr).Namespace(namespace), nil
}

func (k kubeCluster) get(ctx context.Context, apiVersion, kind, namespace, name string) (*unstructured.Unstructured, bool, error) {
	ri, err := k.at(apiVersion, kind, namespace)
	if err != nil {
		return nil, false, err
	}
	u, err := ri.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return nil, false, nil
	case err != nil:
		return nil, false, err //nolint:wrapcheck // a Kubernetes API error
	case u == nil:
		return nil, false, errors.New("the API server returned no object")
	}
	return u, true, nil
}

func (k kubeCluster) apply(ctx context.Context, obj *unstructured.Unstructured, force bool) (*unstructured.Unstructured, error) {
	ri, err := k.at(obj.GetAPIVersion(), obj.GetKind(), obj.GetNamespace())
	if err != nil {
		return nil, err
	}
	u, err := ri.Apply(ctx, obj.GetName(), obj, metav1.ApplyOptions{FieldManager: fieldManager, Force: force})
	if err != nil {
		return nil, err //nolint:wrapcheck // a Kubernetes API error
	}
	if u == nil {
		return nil, errors.New("the API server returned no object")
	}
	return u, nil
}

func (k kubeCluster) remove(ctx context.Context, apiVersion, kind, namespace, name string) error {
	ri, err := k.at(apiVersion, kind, namespace)
	if err != nil {
		return err
	}
	if err := ri.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err //nolint:wrapcheck // a Kubernetes API error
	}
	return nil
}

func (k kubeCluster) list(ctx context.Context, apiVersion, kind, namespace, selector string) ([]unstructured.Unstructured, error) {
	gvr, namespaced, err := k.resource(apiVersion, kind)
	if err != nil {
		return nil, err
	}
	var ri dynamic.ResourceInterface = k.dyn.Resource(gvr)
	if namespaced && namespace != "" {
		ri = k.dyn.Resource(gvr).Namespace(namespace)
	}
	l, err := ri.List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err //nolint:wrapcheck // a Kubernetes API error
	}
	if l == nil {
		return nil, nil
	}
	return l.Items, nil
}

// isConflict reports a 409 from the API server.
func isConflict(err error) bool { return apierrors.IsConflict(err) }

// Reading unstructured objects.

// member walks obj along path; nil when a step is missing.
func member(obj map[string]any, path ...string) any {
	var v any = obj
	for _, key := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[key]
	}
	return v
}

func stringAt(obj map[string]any, path ...string) string {
	s, _ := member(obj, path...).(string) //nolint:errcheck // a missing string is empty
	return s
}

// conditionTrue reports whether the conditions at path hold one of type t
// with status "True".
func conditionTrue(obj map[string]any, t string, path ...string) bool {
	list, ok := member(obj, path...).([]any)
	if !ok {
		return false
	}
	for _, c := range list {
		cm, ok := c.(map[string]any)
		if ok && cm["type"] == t && cm["status"] == "True" {
			return true
		}
	}
	return false
}
