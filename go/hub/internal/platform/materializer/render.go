package materializer

// Rendering (ADR-032): the Project, Environment and App objects of SQL
// rows alone.
//
// The objects keep the names, labels and shape the API has always given
// them, so the existing controllers reconcile them unchanged. Projects and
// environments are rendered the same way for a deployment run and for the
// lifecycle operations that write them as soon as they exist. The release
// decides an App's image: `spec.source` becomes
// `<image repository>@<digest>`, whatever the configuration revision says
// about the source.

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/render"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

const (
	// productionDeletionGrace is the grace period before a deleted
	// production environment is purged, as the API sets it.
	productionDeletionGrace = "168h"
	// webProcess is the process whose digest the App runs (the config
	// revision contract).
	webProcess = "web"
	// maxName is the longest object name that is also a DNS label.
	maxName = 63
)

// Rendered is the objects of one run, in the order they are applied.
type Rendered struct {
	Project     v1alpha1.Project
	Environment v1alpha1.Environment
	App         v1alpha1.App
}

// ProjectMaterial is what a Project object is rendered from.
type ProjectMaterial struct {
	Org         ids.OrgID
	ID          ids.ProjectID
	Slug        string
	Name        string
	Description opt.Val[string]
}

// EnvironmentMaterial is what an Environment object is rendered from.
type EnvironmentMaterial struct {
	ID   ids.EnvironmentID
	Slug string
	Name string
	// EnvType is `standard`, `production` or `preview`.
	EnvType string
	Quota   opt.Val[any]
	// Namespace is the namespace of its placement, when it has one.
	Namespace opt.Val[string]
}

// ProjectMaterialOf is the project material of the run m.
func ProjectMaterialOf(m *store.Materialization) ProjectMaterial {
	return ProjectMaterial{
		Org: m.Org, ID: m.Project, Slug: m.ProjectSlug, Name: m.ProjectName, Description: m.ProjectDescription,
	}
}

// EnvironmentMaterialOf is the environment material of the run m.
func EnvironmentMaterialOf(m *store.Materialization) EnvironmentMaterial {
	return EnvironmentMaterial{
		ID: m.Environment, Slug: m.EnvironmentSlug, Name: m.EnvironmentName, EnvType: m.EnvType,
		Quota: m.Quota, Namespace: opt.Some(m.Namespace),
	}
}

// RenderError is why an object cannot be rendered; the operation fails
// with its Code.
type RenderError struct {
	// Code is the error code of the failed operation: NoImageRepository,
	// NoArtifact, InvalidConfig, PlacementNamespace, NameTooLong,
	// InvalidQuota, or the reason of a spec the builder refuses.
	Code    string
	message string
}

func (e *RenderError) Error() string { return e.message }

func renderError(code, format string, args ...any) *RenderError {
	return &RenderError{Code: code, message: fmt.Sprintf(format, args...)}
}

// EnvironmentName is the Kubernetes name of an environment:
// `<project>-<environment>`, as the API names it.
func EnvironmentName(projectSlug, environmentSlug string) string {
	return projectSlug + "-" + environmentSlug
}

// Render renders the objects of the run m. Only one placement per
// environment exists until M1.9, so the placement's namespace must be the
// one the environment controller creates.
func Render(m *store.Materialization) (Rendered, *RenderError) {
	project := ProjectMaterialOf(m)
	environment, rerr := EnvironmentObject(project, EnvironmentMaterialOf(m), m.Operation)
	if rerr != nil {
		return Rendered{}, rerr
	}
	if len(m.ApplicationSlug) > maxName {
		return Rendered{}, renderError("NameTooLong", "`%s` is too long for a Kubernetes name", m.ApplicationSlug)
	}
	labels := scopeLabels(m.Org, m.ProjectSlug)
	labels[v1alpha1.LabelEnvironment] = EnvironmentName(m.ProjectSlug, m.EnvironmentSlug)
	annotations := map[string]string{
		v1alpha1.AnnotationGeneration:   strconv.FormatUint(uint64(m.Generation), 10),
		v1alpha1.AnnotationOperation:    m.Operation.String(),
		v1alpha1.AnnotationLifecycleUID: m.LifecycleUID.String(),
		v1alpha1.AnnotationID:           m.Target.String(),
	}
	if at, ok := m.RestartedAt.Get(); ok {
		// The builder copies it onto every pod template: a restart run
		// replaces the pods, a later run with the same stamp does not.
		annotations[render.RestartedAt] = restartStamp(at)
	}
	spec, rerr := appSpec(m)
	if rerr != nil {
		return Rendered{}, rerr
	}
	app := v1alpha1.App{
		ObjectMeta: metav1.ObjectMeta{
			Name: m.ApplicationSlug, Namespace: m.Namespace, Labels: labels, Annotations: annotations,
		},
		Spec: spec,
	}
	if err := render.Validate(&app); err != nil {
		var build *render.BuildError
		if errors.As(err, &build) && build != nil {
			return Rendered{}, &RenderError{Code: string(build.Reason), message: build.Error()}
		}
		return Rendered{}, renderError("InvalidConfig", "%v", err)
	}
	return Rendered{Project: ProjectObject(project, m.Operation), Environment: environment, App: app}, nil
}

// restartStamp is a restart stamp as the API writes it on an App: RFC 3339,
// in seconds.
func restartStamp(ms int64) string {
	t := time.UnixMilli(ms).UTC()
	if y := t.Year(); y < -9999 || y > 9999 {
		// Outside the range jiff can print: Rust wrote the number.
		return strconv.FormatInt(ms, 10)
	}
	return t.Format("2006-01-02T15:04:05Z")
}

// ProjectObject is the Project object of p, written by operation.
func ProjectObject(p ProjectMaterial, operation ids.OperationID) v1alpha1.Project {
	var description *string
	if d, ok := p.Description.Get(); ok {
		description = &d
	}
	return v1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name: p.Slug, Labels: managedLabels(p.Org), Annotations: writtenBy(operation, p.ID.String()),
		},
		Spec: v1alpha1.ProjectSpec{
			DisplayName: p.Name, Description: description, Previews: v1alpha1.DefaultPreviewPolicy(),
		},
	}
}

// EnvironmentObject is the Environment object of e in project p, written by
// operation.
func EnvironmentObject(p ProjectMaterial, e EnvironmentMaterial, operation ids.OperationID) (v1alpha1.Environment, *RenderError) {
	name := EnvironmentName(p.Slug, e.Slug)
	namespace := render.NamespaceName(name)
	if len(namespace) > maxName {
		return v1alpha1.Environment{}, renderError("NameTooLong", "`%s` is too long for a Kubernetes name", namespace)
	}
	if placement, ok := e.Namespace.Get(); ok && placement != namespace {
		return v1alpha1.Environment{}, renderError("PlacementNamespace",
			"the placement namespace `%s` is not the environment's namespace `%s`", placement, namespace)
	}
	spec := v1alpha1.EnvironmentSpec{Project: p.Slug, DeletionPolicy: v1alpha1.DeletionPolicyDelete}
	switch e.EnvType {
	case "production":
		spec.Type = v1alpha1.EnvironmentTypeProduction
		spec.Protection = &v1alpha1.Protection{DeletionGrace: productionDeletionGrace}
	case "preview":
		spec.Type = v1alpha1.EnvironmentTypePreview
	default:
		spec.Type = v1alpha1.EnvironmentTypeStandard
	}
	if q, ok := e.Quota.Get(); ok {
		quota, err := decodeQuota(q)
		if err != nil {
			return v1alpha1.Environment{}, renderError("InvalidQuota", "the environment quota is not valid: %v", err)
		}
		spec.Quota = quota
	}
	return v1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Labels: scopeLabels(p.Org, p.Slug), Annotations: writtenBy(operation, e.ID.String()),
		},
		Spec: spec,
	}, nil
}

// decodeQuota reads a stored quota as serde read a `Quota`: a JSON null is
// not one.
func decodeQuota(v any) (*v1alpha1.Quota, error) {
	if v == nil {
		return nil, errors.New("invalid type: null, expected struct Quota")
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err //nolint:wrapcheck // said as InvalidQuota
	}
	var quota v1alpha1.Quota
	if err := json.Unmarshal(data, &quota); err != nil {
		return nil, err //nolint:wrapcheck // said as InvalidQuota
	}
	return &quota, nil
}

// SetOwner makes environment owned by project, as the API creates
// environments: deleting the project deletes them. False while the
// project has no UID.
func SetOwner(environment *v1alpha1.Environment, project *v1alpha1.Project) bool {
	if project.Name == "" || project.UID == "" {
		return false
	}
	environment.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: v1alpha1.SchemeGroupVersion.String(),
		Kind:       v1alpha1.ProjectKind,
		Name:       project.Name,
		UID:        types.UID(string(project.UID)),
	}}
	return true
}

func managedLabels(org ids.OrgID) map[string]string {
	return map[string]string{v1alpha1.LabelManagedBy: v1alpha1.LabelManagerValue, v1alpha1.LabelOrg: org.String()}
}

func scopeLabels(org ids.OrgID, projectSlug string) map[string]string {
	l := managedLabels(org)
	l[v1alpha1.LabelProject] = projectSlug
	return l
}

func writtenBy(operation ids.OperationID, id string) map[string]string {
	return map[string]string{v1alpha1.AnnotationOperation: operation.String(), v1alpha1.AnnotationID: id}
}

// SecretObjectName is the name of the immutable Secret holding revision
// of the managed secret name (crate::secrets::object_name).
func SecretObjectName(name string, revision uint64) string {
	return name + ".r" + strconv.FormatUint(revision, 10)
}

// appSpec is the configuration revision with the release's image as its
// source.
func appSpec(m *store.Materialization) (v1alpha1.AppSpec, *RenderError) {
	digest, ok := m.Artifacts[webProcess]
	if !ok && len(m.Artifacts) == 1 {
		for _, only := range m.Artifacts {
			digest, ok = only, true
		}
	}
	if !ok {
		return v1alpha1.AppSpec{}, renderError("NoArtifact", "release %s has no `web` artifact", m.Release)
	}
	repository := m.ImageRepository.Or("")
	if repository == "" {
		return v1alpha1.AppSpec{}, renderError("NoImageRepository",
			"release %s has no image repository to pull its digest from", m.Release)
	}
	invalid := func(reason string) *RenderError {
		return renderError("InvalidConfig", "configuration revision %d is not an app spec: %s",
			m.ConfigRevisionNumber, reason)
	}
	config, isObject := m.Config.(map[string]any)
	if !isObject {
		return v1alpha1.AppSpec{}, invalid("not a JSON object")
	}
	// A copy: the stored configuration is not changed.
	withImage := make(map[string]any, len(config)+1)
	maps.Copy(withImage, config)
	config = withImage
	config["source"] = map[string]any{"image": repository + "@" + digest.String()}
	data, err := json.Marshal(config)
	if err != nil {
		return v1alpha1.AppSpec{}, invalid(err.Error())
	}
	spec, err := v1alpha1.DecodeAppSpec(data)
	if err != nil {
		return v1alpha1.AppSpec{}, invalid(err.Error())
	}
	// A managed secret is read from the immutable object of the revision
	// the run is bound to; other names stay the cluster's own Secrets.
	// Images are pulled with the bound registry logins only.
	for i := range spec.Env {
		reference := spec.Env[i].FromSecret
		if reference == nil {
			continue
		}
		j := slices.IndexFunc(m.Secrets, func(b store.SecretBinding) bool {
			return b.Registry.IsNone() && b.Name == reference.Name
		})
		if j >= 0 {
			reference.Name = SecretObjectName(m.Secrets[j].Name, m.Secrets[j].Revision)
		}
	}
	spec.ImagePullSecrets = nil
	for _, b := range m.Secrets {
		if b.Registry.IsSome() {
			spec.ImagePullSecrets = append(spec.ImagePullSecrets, SecretObjectName(b.Name, b.Revision))
		}
	}
	return spec, nil
}
