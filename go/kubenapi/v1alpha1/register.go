package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	// Group is the API group of every Kuben custom resource.
	Group = "kuben.dev"
	// Version is the API version of the current custom resources.
	Version = "v1alpha1"
	// FieldManager is the field manager Kuben uses for server-side apply.
	FieldManager = "kuben"
)

// Kinds and plural resource names of the kuben.dev/v1alpha1 resources, as
// the CRD manifest names them.
const (
	KubenConfigKind            = "KubenConfig"
	KubenConfigResource        = "kubenconfigs"
	ProjectKind                = "Project"
	ProjectResource            = "projects"
	EnvironmentKind            = "Environment"
	EnvironmentResource        = "environments"
	AppKind                    = "App"
	AppResource                = "apps"
	ReleaseKind                = "Release"
	ReleaseResource            = "releases"
	BuildRunKind               = "BuildRun"
	BuildRunResource           = "buildruns"
	ApplicationRuntimeKind     = "ApplicationRuntime"
	ApplicationRuntimeResource = "applicationruntimes"
	ExecutionTaskKind          = "ExecutionTask"
	ExecutionTaskResource      = "executiontasks"
)

// SchemeGroupVersion is kuben.dev/v1alpha1, the group version the types of
// this package are registered under.
var SchemeGroupVersion = schema.GroupVersion{Group: Group, Version: Version}

// Resource qualifies a plural resource name of this group, e.g.
// Resource(AppResource) is apps.kuben.dev.
func Resource(resource string) schema.GroupResource {
	return SchemeGroupVersion.WithResource(resource).GroupResource()
}

// AddToScheme registers every kind of this package, and its list, with the
// scheme, together with the meta/v1 types a group version needs (options,
// Status, WatchEvent).
func AddToScheme(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemeGroupVersion,
		&KubenConfig{}, &KubenConfigList{},
		&Project{}, &ProjectList{},
		&Environment{}, &EnvironmentList{},
		&App{}, &AppList{},
		&Release{}, &ReleaseList{},
		&BuildRun{}, &BuildRunList{},
		&ApplicationRuntime{}, &ApplicationRuntimeList{},
		&ExecutionTask{}, &ExecutionTaskList{},
	)
	metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
	return nil
}
