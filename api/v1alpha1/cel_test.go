package v1alpha1_test

import (
	"context"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
)

// apiserver validates objects the way the apiserver does for the
// manifest's CRD of one kind: the OpenAPI schema (types, lengths,
// patterns, minimums) and the CEL rules, transition rules included. It
// replaces kube-rs's validate_cel/validate_cel_update of the Rust tests,
// with the rules read from the frozen manifest rather than the Rust types.
type apiserver struct {
	t          *testing.T
	structural *schema.Structural
	openAPI    validation.SchemaValidator
	cel        *cel.Validator
}

func newAPIServer(t *testing.T, kindName string) apiserver {
	t.Helper()
	var internal apiextensions.JSONSchemaProps
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(rootSchema(t, crdOf(t, kindName)), &internal, nil); err != nil {
		t.Fatal(err)
	}
	structural, err := schema.NewStructural(&internal)
	if err != nil {
		t.Fatal(err)
	}
	openAPI, _, err := validation.NewSchemaValidator(&internal)
	if err != nil {
		t.Fatal(err)
	}
	return apiserver{t: t, structural: structural, openAPI: openAPI, cel: cel.NewValidator(structural, true, celconfig.PerCallLimit)}
}

func (a apiserver) unstructured(obj runtime.Object) map[string]any {
	a.t.Helper()
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		a.t.Fatal(err)
	}
	return u
}

// create returns what the apiserver refuses in a new object.
func (a apiserver) create(obj runtime.Object) field.ErrorList {
	a.t.Helper()
	u := a.unstructured(obj)
	errs := validation.ValidateCustomResource(nil, u, a.openAPI)
	celErrs, _ := a.cel.Validate(context.Background(), nil, a.structural, u, nil, celconfig.RuntimeCELCostBudget)
	return append(errs, celErrs...)
}

// update returns what the apiserver refuses in an update of old to obj.
func (a apiserver) update(obj, old runtime.Object) field.ErrorList {
	a.t.Helper()
	u, o := a.unstructured(obj), a.unstructured(old)
	errs := validation.ValidateCustomResourceUpdate(nil, u, o, a.openAPI)
	celErrs, _ := a.cel.Validate(context.Background(), nil, a.structural, u, o, celconfig.RuntimeCELCostBudget)
	return append(errs, celErrs...)
}

// accepts fails the test when errs is not empty.
func (a apiserver) accepts(what string, errs field.ErrorList) {
	a.t.Helper()
	if len(errs) > 0 {
		a.t.Errorf("%s: refused: %v", what, errs.ToAggregate())
	}
}

// refuses fails the test when errs is empty.
func (a apiserver) refuses(what string, errs field.ErrorList) {
	a.t.Helper()
	if len(errs) == 0 {
		a.t.Errorf("%s: accepted", what)
	}
}
