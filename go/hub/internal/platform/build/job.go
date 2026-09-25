package build

// The objects of one build attempt (ADR-028; job.rs), rendered as pure data.
//
//	BuildRun kbuild-<attempt>            the attempt in the cluster; owns the rest
//	├── Secret kbuild-<attempt>-source   the repository-scoped fetch token
//	└── Job kbuild-<attempt>             one pod, never retried by Kubernetes
//	    ├── init fetch   git fetch of exactly one commit (the only mount of the token)
//	    ├── init plan    Dockerfile or Railpack; the Railpack plan
//	    └── build        rootless BuildKit: build, push, report the digest
//
// The pod gets no service-account token, runs as an unprivileged user with
// mandatory budgets and a deadline, and receives untrusted values (paths,
// commit, image) only as environment variables of fixed scripts. A context
// or Dockerfile directory that resolves outside the checkout (a symlink) is
// refused before BuildKit reads it, and the fetch token is mounted in the
// fetch container alone.

import (
	"fmt"
	"math"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/config"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ops/outcome"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/source"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

const (
	// AttemptLabel names the attempt on every object of a build.
	AttemptLabel = "kuben.dev/build-attempt"
	// PlanContainer is the init container that picks the strategy and writes
	// the Railpack plan.
	PlanContainer = "plan"
	// TokenKey is the key of the fetch token in its Secret.
	TokenKey = "token"

	targetLabel = "kuben.dev/target"
	tokenDir    = "/var/run/kuben/source" //nolint:gosec // a mount path, not a credential
	dockerDir   = "/docker"
	user        = 1000
	// tokenMode lets owner and group read the token (the pod's fsGroup).
	tokenMode    = 0o440
	helperCPU    = "100m"
	helperMemory = "256Mi"
	// minDeadlineSecs is the shortest deadline a build gets.
	minDeadlineSecs = 60
)

// NodePool is the node label of the required build pool.
type NodePool struct {
	Key   string
	Value string
}

// Settings are where and with what a build runs.
type Settings struct {
	Namespace     string
	BuildkitImage string
	FetchImage    string
	// RailpackImage is set only when the Railpack frontend is too.
	RailpackImage    opt.Val[string]
	RailpackFrontend opt.Val[string]
	CPURequest       string
	CPULimit         string
	Memory           string
	EphemeralStorage string
	// DeadlineSecs is the hard deadline of one attempt, at least 60.
	DeadlineSecs     uint64
	PushSecret       opt.Val[string]
	InsecureRegistry bool
	// NodePool is the required build pool.
	NodePool opt.Val[NodePool]
	// ScannerImage is the Trivy image; none builds without a scan.
	ScannerImage  opt.Val[string]
	ScannerMemory string
}

// SettingsFromConfig are the settings of cfg, in namespace.
func SettingsFromConfig(cfg config.BuildCfg, namespace string) Settings {
	nonEmpty := func(v opt.Val[string]) opt.Val[string] {
		if s, ok := v.Get(); ok && s != "" {
			return v
		}
		return opt.None[string]()
	}
	s := Settings{
		Namespace:        namespace,
		BuildkitImage:    cfg.BuildkitImage,
		FetchImage:       cfg.FetchImage,
		RailpackFrontend: nonEmpty(cfg.RailpackFrontend),
		CPURequest:       cfg.CPURequest,
		CPULimit:         cfg.CPULimit,
		Memory:           cfg.Memory,
		EphemeralStorage: cfg.EphemeralStorage,
		DeadlineSecs:     max(cfg.DeadlineSecs, minDeadlineSecs),
		PushSecret:       nonEmpty(cfg.PushSecret),
		InsecureRegistry: cfg.InsecureRegistry,
		ScannerImage:     nonEmpty(opt.Some(cfg.ScannerImage)),
		ScannerMemory:    cfg.ScannerMemory,
	}
	// Railpack needs both images; with either missing the plan step
	// explains that only Dockerfile builds run.
	if s.RailpackFrontend.IsSome() {
		s.RailpackImage = nonEmpty(cfg.RailpackImage)
	}
	if pool, ok := cfg.NodePool.Get(); ok {
		if key, value, found := strings.Cut(pool, "="); found {
			s.NodePool = opt.Some(NodePool{Key: strings.TrimSpace(key), Value: strings.TrimSpace(value)})
		}
	}
	return s
}

// Name is the deterministic name of attempt's objects.
func Name(attempt store.BuildAttempt) string {
	return "kbuild-" + strings.ReplaceAll(attempt.ID.String(), "-", "")
}

// SecretName is the name of the fetch-token Secret.
func SecretName(attempt store.BuildAttempt) string { return Name(attempt) + "-source" }

func labels(attempt store.BuildAttempt) map[string]string {
	return map[string]string{
		AttemptLabel:            attempt.ID.String(),
		v1alpha1.LabelManagedBy: v1alpha1.LabelManagerValue,
		targetLabel:             attempt.Target.String(),
	}
}

func crdStrategy(s source.BuildStrategy) v1alpha1.BuildStrategy {
	switch s.OrAuto() {
	case source.Auto:
		return v1alpha1.BuildStrategyAuto
	case source.Dockerfile:
		return v1alpha1.BuildStrategyDockerfile
	case source.Railpack:
		return v1alpha1.BuildStrategyRailpack
	}
	return v1alpha1.BuildStrategyAuto
}

// RunOf is the attempt's BuildRun, the owner of its Job and Secret.
func RunOf(attempt store.BuildAttempt, s Settings) v1alpha1.BuildRun {
	return v1alpha1.BuildRun{
		TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: v1alpha1.BuildRunKind},
		ObjectMeta: metav1.ObjectMeta{
			Name: Name(attempt), Namespace: s.Namespace, Labels: labels(attempt),
		},
		Spec: v1alpha1.BuildRunSpec{
			App:      attempt.Target.String(),
			Repo:     attempt.Repository.String(),
			GitRef:   attempt.Commit.String(),
			Path:     attempt.Recipe.Context.String(),
			Strategy: crdStrategy(attempt.Recipe.Strategy),
			Image:    attempt.PushReference(),
		},
	}
}

// ownerReferences is owner as the controller of an object, as kube-rs's
// controller_owner_ref wrote it; none while owner has no UID.
func ownerReferences(owner *v1alpha1.BuildRun) []metav1.OwnerReference {
	if owner.UID == "" {
		return nil
	}
	return []metav1.OwnerReference{{
		APIVersion: v1alpha1.SchemeGroupVersion.String(),
		Kind:       v1alpha1.BuildRunKind,
		Name:       owner.Name,
		UID:        owner.UID,
		Controller: new(true),
	}}
}

// SourceSecret is the Secret with the fetch token, owned by owner.
func SourceSecret(attempt store.BuildAttempt, s Settings, owner *v1alpha1.BuildRun, token string) *corev1.Secret {
	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:            SecretName(attempt),
			Namespace:       s.Namespace,
			Labels:          labels(attempt),
			OwnerReferences: ownerReferences(owner),
		},
		Type:      corev1.SecretTypeOpaque,
		Immutable: new(true),
		Data:      map[string][]byte{TokenKey: []byte(token)},
	}
}

// quantities parses the budget texts of a build; a text that is not a
// quantity makes the Job unrenderable (InvalidBudget).
type quantities struct{ err error }

func (q *quantities) parse(text string) resource.Quantity {
	v, err := resource.ParseQuantity(text)
	if err != nil && q.err == nil {
		q.err = fmt.Errorf("quantities: %q: %w", text, err)
	}
	return v
}

func env(pairs ...[2]string) []corev1.EnvVar {
	vars := make([]corev1.EnvVar, 0, len(pairs))
	for _, p := range pairs {
		vars = append(vars, corev1.EnvVar{Name: p[0], Value: p[1]})
	}
	return vars
}

func helperResources(q *quantities, ephemeral string) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:              q.parse(helperCPU),
			corev1.ResourceMemory:           q.parse(helperMemory),
			corev1.ResourceEphemeralStorage: q.parse("64Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory:           q.parse(helperMemory),
			corev1.ResourceEphemeralStorage: q.parse(ephemeral),
		},
	}
}

func restricted() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsNonRoot:             new(true),
		RunAsUser:                new(int64(user)),
		RunAsGroup:               new(int64(user)),
		AllowPrivilegeEscalation: new(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func podSecurity() *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		RunAsNonRoot: new(true), RunAsUser: new(int64(user)), RunAsGroup: new(int64(user)), FSGroup: new(int64(user)),
	}
}

func insecureText(s Settings) string {
	if s.InsecureRegistry {
		return "true"
	}
	return "false"
}

func workspaceMount() corev1.VolumeMount {
	return corev1.VolumeMount{Name: "workspace", MountPath: "/workspace"}
}

// workspaceVolumes are the workspace and, with a push secret, its Docker
// configuration.
func workspaceVolumes(q *quantities, s Settings) []corev1.Volume {
	size := q.parse(s.EphemeralStorage)
	volumes := []corev1.Volume{{
		Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &size}},
	}}
	if secret, ok := s.PushSecret.Get(); ok {
		volumes = append(volumes, corev1.Volume{Name: "push", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName:  secret,
			DefaultMode: new(int32(tokenMode)),
			Items:       []corev1.KeyToPath{{Key: ".dockerconfigjson", Path: "config.json"}},
		}}})
	}
	return volumes
}

// Job is the attempt's Job, owned by owner. It fails when a budget of s is
// not a quantity.
func Job(attempt store.BuildAttempt, s Settings, owner *v1alpha1.BuildRun, cloneURL string) (*batchv1.Job, error) {
	q := &quantities{}
	build := buildContainer(q, attempt, s)
	volumes := append(workspaceVolumes(q, s), corev1.Volume{Name: "source", VolumeSource: corev1.VolumeSource{
		Secret: &corev1.SecretVolumeSource{SecretName: SecretName(attempt), DefaultMode: new(int32(tokenMode))},
	}})
	var affinity *corev1.Affinity
	var tolerations []corev1.Toleration
	if pool, ok := s.NodePool.Get(); ok {
		affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key: pool.Key, Operator: corev1.NodeSelectorOpIn, Values: []string{pool.Value},
				}},
			}}},
		}}
		tolerations = []corev1.Toleration{{
			Key: pool.Key, Operator: corev1.TolerationOpEqual, Value: pool.Value, Effect: corev1.TaintEffectNoSchedule,
		}}
	}
	inits := initContainers(q, attempt, s, cloneURL)
	// With a scanner the build runs to its end first; the scan follows.
	main := []corev1.Container{build}
	if scan, ok := scanContainer(q, s, attempt.ImageRepository, opt.None[string]()).Get(); ok {
		inits = append(inits, build)
		main = []corev1.Container{scan}
	}
	job := &batchv1.Job{
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{
			Name: Name(attempt), Namespace: s.Namespace, Labels: labels(attempt), OwnerReferences: ownerReferences(owner),
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          new(int32(0)),
			ActiveDeadlineSeconds: new(int64(min(s.DeadlineSecs, math.MaxInt64))),
			PodFailurePolicy: &batchv1.PodFailurePolicy{Rules: []batchv1.PodFailurePolicyRule{{
				Action: batchv1.PodFailurePolicyActionFailJob,
				OnPodConditions: []batchv1.PodFailurePolicyOnPodConditionsPattern{{
					Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue,
				}},
			}}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels(attempt)},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: new(false),
					EnableServiceLinks:           new(false),
					SecurityContext:              podSecurity(),
					Affinity:                     affinity,
					Tolerations:                  tolerations,
					InitContainers:               inits,
					Containers:                   main,
					Volumes:                      volumes,
				},
			},
		},
	}
	if q.err != nil {
		return nil, q.err
	}
	return job, nil
}

// ScanContainer is the container that writes the SBOM of
// repository@digest and scans it; the digest the build wrote when digest is
// none. None without a scanner. It fails when a budget of s is not a
// quantity.
func ScanContainer(s Settings, repository string, digest opt.Val[string]) (opt.Val[corev1.Container], error) {
	q := &quantities{}
	c := scanContainer(q, s, repository, digest)
	if q.err != nil {
		return opt.None[corev1.Container](), q.err
	}
	return c, nil
}

func scanContainer(q *quantities, s Settings, repository string, digest opt.Val[string]) opt.Val[corev1.Container] {
	image, ok := s.ScannerImage.Get()
	if !ok {
		return opt.None[corev1.Container]()
	}
	vars := [][2]string{
		{"KUBEN_REPOSITORY", repository},
		{"KUBEN_INSECURE_REGISTRY", insecureText(s)},
		{"HOME", "/workspace/home"},
	}
	if d, ok := digest.Get(); ok {
		vars = append(vars, [2]string{"KUBEN_DIGEST", d})
	}
	mounts := []corev1.VolumeMount{workspaceMount()}
	if s.PushSecret.IsSome() {
		vars = append(vars, [2]string{"DOCKER_CONFIG", dockerDir})
		mounts = append(mounts, corev1.VolumeMount{Name: "push", MountPath: dockerDir, ReadOnly: true})
	}
	return opt.Some(corev1.Container{
		Name:            outcome.ScanContainer,
		Image:           image,
		Command:         []string{"sh", "-c", ScanScript},
		Env:             env(vars...),
		SecurityContext: restricted(),
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:              q.parse(helperCPU),
				corev1.ResourceMemory:           q.parse(s.ScannerMemory),
				corev1.ResourceEphemeralStorage: q.parse("256Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory:           q.parse(s.ScannerMemory),
				corev1.ResourceEphemeralStorage: q.parse(s.EphemeralStorage),
			},
		},
		// The log carries the SBOM; only the file is the report.
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
		VolumeMounts:             mounts,
	})
}

// initContainers are the fetch and plan containers.
func initContainers(q *quantities, attempt store.BuildAttempt, s Settings, cloneURL string) []corev1.Container {
	return []corev1.Container{
		{
			Name:    outcome.FetchContainer,
			Image:   s.FetchImage,
			Command: []string{"sh", "-c", fetchScript},
			Env: env(
				[2]string{"KUBEN_CLONE_URL", cloneURL},
				[2]string{"KUBEN_COMMIT", attempt.Commit.String()},
			),
			SecurityContext:          restricted(),
			Resources:                helperResources(q, s.EphemeralStorage),
			TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
			VolumeMounts: []corev1.VolumeMount{
				workspaceMount(),
				{Name: "source", MountPath: tokenDir, ReadOnly: true},
			},
		},
		{
			Name:    PlanContainer,
			Image:   s.RailpackImage.Or(s.FetchImage),
			Command: []string{"sh", "-c", planScript},
			Env: env(
				[2]string{"KUBEN_CONTEXT", attempt.Recipe.Context.String()},
				[2]string{"KUBEN_DOCKERFILE", attempt.Recipe.DockerfilePath().String()},
				[2]string{"KUBEN_STRATEGY", string(attempt.Recipe.Strategy.OrAuto())},
				[2]string{"HOME", "/workspace/home"},
			),
			SecurityContext:          restricted(),
			Resources:                helperResources(q, s.EphemeralStorage),
			TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
			VolumeMounts:             []corev1.VolumeMount{workspaceMount()},
		},
	}
}

// buildContainer is the BuildKit container.
func buildContainer(q *quantities, attempt store.BuildAttempt, s Settings) corev1.Container {
	vars := [][2]string{
		{"KUBEN_CONTEXT", attempt.Recipe.Context.String()},
		{"KUBEN_DOCKERFILE", attempt.Recipe.DockerfilePath().String()},
		{"KUBEN_IMAGE", attempt.PushReference()},
		{"KUBEN_RAILPACK_FRONTEND", s.RailpackFrontend.Or("")},
		{"KUBEN_INSECURE_REGISTRY", insecureText(s)},
		{"BUILDKITD_FLAGS", "--oci-worker-no-process-sandbox"},
	}
	mounts := []corev1.VolumeMount{
		workspaceMount(),
		{Name: "workspace", MountPath: "/home/user/.local/share/buildkit", SubPath: "buildkit"},
	}
	if s.PushSecret.IsSome() {
		vars = append(vars, [2]string{"DOCKER_CONFIG", dockerDir})
		mounts = append(mounts, corev1.VolumeMount{Name: "push", MountPath: dockerDir, ReadOnly: true})
	}
	return corev1.Container{
		Name:    outcome.BuildContainer,
		Image:   s.BuildkitImage,
		Command: []string{"sh", "-c", buildScript},
		Env:     env(vars...),
		// Rootless BuildKit needs its own user namespaces: unconfined for
		// this container only, never host-wide.
		SecurityContext: &corev1.SecurityContext{
			RunAsNonRoot:    new(true),
			RunAsUser:       new(int64(user)),
			RunAsGroup:      new(int64(user)),
			SeccompProfile:  &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
			AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:              q.parse(s.CPURequest),
				corev1.ResourceMemory:           q.parse(s.Memory),
				corev1.ResourceEphemeralStorage: q.parse(s.EphemeralStorage),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:              q.parse(s.CPULimit),
				corev1.ResourceMemory:           q.parse(s.Memory),
				corev1.ResourceEphemeralStorage: q.parse(s.EphemeralStorage),
			},
		},
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		VolumeMounts:             mounts,
	}
}
