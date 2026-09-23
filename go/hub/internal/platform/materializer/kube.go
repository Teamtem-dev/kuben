package materializer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

// The kuben.dev resources the materializer reads and writes.
var (
	projectsGVR     = v1alpha1.SchemeGroupVersion.WithResource(v1alpha1.ProjectResource)
	environmentsGVR = v1alpha1.SchemeGroupVersion.WithResource(v1alpha1.EnvironmentResource)
	appsGVR         = v1alpha1.SchemeGroupVersion.WithResource(v1alpha1.AppResource)
	runtimesGVR     = v1alpha1.SchemeGroupVersion.WithResource(v1alpha1.ApplicationRuntimeResource)
	configsGVR      = v1alpha1.SchemeGroupVersion.WithResource(v1alpha1.KubenConfigResource)
)

// kubenObject is a kuben.dev object the materializer handles as a typed
// value: T is the struct, PT its pointer, which carries the metadata.
type kubenObject[T any] interface {
	*T
	metav1.Object
}

// BelongsTo reports whether an object with labels belongs to org: Kuben
// manages it and labelled it with the organization. An object anyone else
// created is never adopted.
func BelongsTo(labels map[string]string, org ids.OrgID) bool {
	return labels[v1alpha1.LabelManagedBy] == v1alpha1.LabelManagerValue && labels[v1alpha1.LabelOrg] == org.String()
}

// isConflict reports a 409: the object changed since it was read (a
// resourceVersion precondition failed) or appeared meanwhile (a create).
func isConflict(err error) bool {
	var status apierrors.APIStatus
	return errors.As(err, &status) && status.Status().Code == http.StatusConflict
}

// isNotFound reports a 404.
func isNotFound(err error) bool { return apierrors.IsNotFound(err) }

// patchSecret applies a JSON merge patch to the Secret name, as no field
// manager.
func patchSecret(ctx context.Context, api typedcorev1.SecretInterface, name string, patch any) error {
	data, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("patching %s: %w", name, err)
	}
	_, err = api.Patch(ctx, name, types.MergePatchType, data, metav1.PatchOptions{})
	return err //nolint:wrapcheck // a Kubernetes API error, classified by the caller
}

// get reads the object name through ri; false when there is none.
func get[T any, PT kubenObject[T]](ctx context.Context, ri dynamic.ResourceInterface, name string) (T, bool, error) {
	var zero T
	u, err := ri.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return zero, false, nil
	case err != nil:
		return zero, false, err //nolint:wrapcheck // a Kubernetes API error, classified by the caller
	case u == nil:
		return zero, false, errors.New("the API server returned no object")
	}
	v, err := decode[T](u)
	return v, err == nil, err
}

// decode is u as the typed object T, through its serde-compatible JSON.
func decode[T any](u *unstructured.Unstructured) (T, error) {
	var v T
	data, err := u.MarshalJSON()
	if err != nil {
		return v, fmt.Errorf("reading %s %s: %w", u.GetKind(), u.GetName(), err)
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return v, fmt.Errorf("reading %s %s: %w", u.GetKind(), u.GetName(), err)
	}
	return v, nil
}

// encode is obj as an unstructured object of kind. Members Go writes and
// Rust left out (`creationTimestamp: null`) are removed, so a server-side
// apply claims the same fields as the Rust materializer did.
func encode(obj any, kind string) (*unstructured.Unstructured, error) {
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("writing %s: %w", kind, err)
	}
	u := &unstructured.Unstructured{}
	if err := u.UnmarshalJSON(data); err != nil {
		return nil, fmt.Errorf("writing %s: %w", kind, err)
	}
	u.SetAPIVersion(v1alpha1.SchemeGroupVersion.String())
	u.SetKind(kind)
	if ts, found, _ := unstructured.NestedFieldNoCopy(u.Object, "metadata", "creationTimestamp"); found && ts == nil {
		unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	}
	return u, nil
}

// put writes desired over live, the object as last read: server-side apply
// as FieldManager carrying live's resourceVersion (a 409 when it changed
// since), or a create when there was none (a 409 when one appeared).
// Fields other managers own and the materializer does not render stay
// theirs.
func put[T any, PT kubenObject[T]](
	ctx context.Context, ri dynamic.ResourceInterface, kind string, desired PT, live PT,
) (T, error) {
	var zero T
	u, err := encode(desired, kind)
	if err != nil {
		return zero, err
	}
	var written *unstructured.Unstructured
	if live == nil {
		written, err = ri.Create(ctx, u, metav1.CreateOptions{FieldManager: FieldManager})
	} else {
		u.SetResourceVersion(live.GetResourceVersion())
		written, err = ri.Apply(ctx, desired.GetName(), u, metav1.ApplyOptions{FieldManager: FieldManager, Force: true})
	}
	if err != nil {
		return zero, err //nolint:wrapcheck // a Kubernetes API error, classified by the caller
	}
	if written == nil {
		return zero, errors.New("the API server returned no object")
	}
	return decode[T](written)
}

// mergePatch applies a JSON merge patch to name, as no field manager.
func mergePatch(ctx context.Context, ri dynamic.ResourceInterface, name string, patch any) error {
	data, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("patching %s: %w", name, err)
	}
	_, err = ri.Patch(ctx, name, types.MergePatchType, data, metav1.PatchOptions{})
	return err //nolint:wrapcheck // a Kubernetes API error, classified by the caller
}

// remove deletes name with the given propagation; one that is gone
// already is fine.
func remove(ctx context.Context, ri dynamic.ResourceInterface, name string, propagation metav1.DeletionPropagation) error {
	err := ri.Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &propagation})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err //nolint:wrapcheck // a Kubernetes API error, classified by the caller
}
