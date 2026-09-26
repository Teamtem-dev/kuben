package build_test

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/build"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	opbuild "github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ops/build"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ops/outcome"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ops/target"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/source"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

// Ported from job.rs.

// checked is a value and the error that came with it.
type checked[T any] struct {
	v   T
	err error
}

func check[T any](v T, err error) checked[T] { return checked[T]{v, err} }

// must is the value; the test fails on the error.
func (c checked[T]) must(t *testing.T) T {
	t.Helper()
	if c.err != nil {
		t.Fatal(c.err)
	}
	return c.v
}

func id[K ids.Kind](n byte) ids.ID[K] {
	var u uuid.UUID
	u[15] = n
	return ids.From[K](u)
}

// attempt is the Rust job::tests::attempt.
func attempt(t *testing.T, recipe source.BuildRecipe) store.BuildAttempt {
	t.Helper()
	return store.BuildAttempt{
		ID:              ids.From[ids.BuildAttempt](uuid.MustParse("0192f3a1-0000-7000-8000-000000000001")),
		Org:             id[ids.Org](1),
		Project:         id[ids.Project](2),
		Application:     id[ids.Application](3),
		Target:          id[ids.Target](4),
		Binding:         id[ids.SourceBinding](5),
		AttemptNo:       2,
		Commit:          check(source.ParseCommitSha("0123456789abcdef0123456789abcdef01234567")).must(t),
		SourceEpoch:     target.SourceEpoch(3),
		LifecycleUID:    uuid.UUID{15: 6},
		Repository:      check(source.ParseRepoName("acme/shop")).must(t),
		Recipe:          recipe,
		ImageRepository: "registry.local/acme/shop",
		InstallationID:  7,
		Branch:          check(source.ParseBranchName("main")).must(t),
		Phase:           opbuild.Queued,
		Operation:       id[ids.Operation](8),
	}
}

// settings is the Rust job::tests::settings.
func settings() build.Settings {
	cfg := config.DefaultBuildCfg()
	cfg.PushSecret = opt.Some("push")
	cfg.NodePool = opt.Some("kuben.dev/pool = build")
	cfg.DeadlineSecs = 900
	cfg.RailpackImage = opt.Some("")
	return build.SettingsFromConfig(cfg, "kuben-builds")
}

func rendered(t *testing.T, recipe source.BuildRecipe, s build.Settings) corev1.PodSpec {
	t.Helper()
	a := attempt(t, recipe)
	owner := build.RunOf(a, s)
	owner.UID = types.UID("uid-1")
	job := check(build.Job(a, s, &owner, "https://github.com/acme/shop.git")).must(t)
	return job.Spec.Template.Spec
}

func container(t *testing.T, list []corev1.Container, name string) corev1.Container {
	t.Helper()
	i := slices.IndexFunc(list, func(c corev1.Container) bool { return c.Name == name })
	if i < 0 {
		t.Fatalf("no container %s", name)
	}
	return list[i]
}

func hasEnv(c corev1.Container, name, value string) bool {
	return slices.ContainsFunc(c.Env, func(e corev1.EnvVar) bool {
		return e.Name == name && (value == "" || e.Value == value)
	})
}

func TestNamesAreDeterministicAndShort(t *testing.T) {
	a := attempt(t, source.BuildRecipe{})
	if got := build.Name(a); got != "kbuild-0192f3a1000070008000000000000001" {
		t.Errorf("name %s", got)
	}
	if len(build.SecretName(a)) > 63 {
		t.Errorf("secret name %s", build.SecretName(a))
	}
	if got := a.PushReference(); got != "registry.local/acme/shop:0123456789ab-2" {
		t.Errorf("push reference %s", got)
	}
}

func TestBuildPodsGetNoServiceAccountToken(t *testing.T) {
	a := attempt(t, source.BuildRecipe{})
	s := settings()
	owner := build.RunOf(a, s)
	job := check(build.Job(a, s, &owner, "u")).must(t)
	pod := job.Spec.Template.Spec
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("the service-account token is mounted")
	}
	if pod.EnableServiceLinks == nil || *pod.EnableServiceLinks || pod.ServiceAccountName != "" {
		t.Error("service links or a service account")
	}
	if pod.SecurityContext == nil || pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot {
		t.Error("the pod may run as root")
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Error("a retry is a new attempt, never the Job's")
	}
	if d := job.Spec.ActiveDeadlineSeconds; d == nil || *d != 900 {
		t.Errorf("deadline %v", d)
	}
}

func mountsSource(c corev1.Container) bool {
	return slices.ContainsFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == "source" })
}

func TestOnlyTheFetchContainerSeesTheToken(t *testing.T) {
	pod := rendered(t, source.BuildRecipe{}, settings())
	fetch := container(t, pod.InitContainers, outcome.FetchContainer)
	if !mountsSource(fetch) {
		t.Error("fetch has no token")
	}
	for _, name := range []string{build.PlanContainer, outcome.BuildContainer} {
		if mountsSource(container(t, pod.InitContainers, name)) {
			t.Errorf("%s sees the token", name)
		}
	}
	sc := fetch.SecurityContext
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("fetch is not restricted: %+v", sc)
	}
}

func TestBudgetsAreMandatoryAndMemoryIsGuaranteed(t *testing.T) {
	pod := rendered(t, source.BuildRecipe{}, settings())
	res := container(t, pod.InitContainers, outcome.BuildContainer).Resources
	if !res.Requests.Memory().Equal(*res.Limits.Memory()) {
		t.Error("memory request and limit differ")
	}
	for _, key := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage} {
		if _, ok := res.Limits[key]; !ok {
			t.Errorf("no limit %s", key)
		}
		if _, ok := res.Requests[key]; !ok {
			t.Errorf("no request %s", key)
		}
	}
	if got := pod.Volumes[0].EmptyDir.SizeLimit.String(); got != "10Gi" {
		t.Errorf("workspace size %s", got)
	}
	for _, init := range pod.InitContainers {
		if _, ok := init.Resources.Limits[corev1.ResourceMemory]; !ok {
			t.Errorf("%s has no memory limit", init.Name)
		}
	}
}

func TestUntrustedValuesTravelAsEnvironmentOnly(t *testing.T) {
	recipe := source.BuildRecipe{Context: check(source.ParseRepoPath("apps/$(rm -rf ~)")).must(t)}
	pod := rendered(t, recipe, settings())
	all := append(slices.Clone(pod.InitContainers), pod.Containers...)
	for _, name := range []string{outcome.FetchContainer, build.PlanContainer, outcome.BuildContainer, outcome.ScanContainer} {
		script := container(t, all, name).Command[2]
		if strings.Contains(script, "rm -rf ~") || strings.Contains(script, "acme") {
			t.Errorf("%s embeds an input", name)
		}
	}
	if !hasEnv(container(t, pod.InitContainers, build.PlanContainer), "KUBEN_CONTEXT", "apps/$(rm -rf ~)") {
		t.Error("the context is not in the plan's environment")
	}
	if !hasEnv(container(t, pod.Containers, outcome.ScanContainer), "KUBEN_REPOSITORY", "registry.local/acme/shop") {
		t.Error("the repository is not in the scan's environment")
	}
	script := container(t, pod.InitContainers, outcome.BuildContainer).Command[2]
	if !strings.Contains(script, `inside "$ctx"`) || !strings.Contains(script, `inside "$dir"`) {
		t.Error("symlinked contexts and Dockerfiles are not refused")
	}
}

func TestTheScanFollowsTheBuildAndNeverFailsIt(t *testing.T) {
	pod := rendered(t, source.BuildRecipe{}, settings())
	var inits []string
	for _, c := range pod.InitContainers {
		inits = append(inits, c.Name)
	}
	if !slices.Equal(inits, []string{outcome.FetchContainer, build.PlanContainer, outcome.BuildContainer}) {
		t.Errorf("init containers %v", inits)
	}
	scan := container(t, pod.Containers, outcome.ScanContainer)
	if scan.Image != "aquasec/trivy:0.74.0" || scan.TerminationMessagePolicy != corev1.TerminationMessageReadFile {
		t.Errorf("scan %s %s: the log carries the SBOM", scan.Image, scan.TerminationMessagePolicy)
	}
	if !scan.Resources.Requests.Memory().Equal(*scan.Resources.Limits.Memory()) {
		t.Error("scan memory is not guaranteed")
	}
	if p := scan.SecurityContext.AllowPrivilegeEscalation; p == nil || *p || mountsSource(scan) {
		t.Error("the scan is not restricted or sees the token")
	}
	for line := range strings.SplitSeq(build.ScanScript, "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "exit 1") {
			t.Errorf("the scan fails: %s", line)
		}
	}
	if !strings.Contains(build.ScanScript, `"status":"unavailable"`) {
		t.Error("an unavailable scan says so")
	}
	pinned := check(build.ScanContainer(settings(), "registry.local/acme/shop", opt.Some("sha256:abc"))).must(t)
	if c, ok := pinned.Get(); !ok || !hasEnv(c, "KUBEN_DIGEST", "") {
		t.Error("a pinned scan has no digest")
	}

	unscanned := settings()
	unscanned.ScannerImage = opt.None[string]()
	pod = rendered(t, source.BuildRecipe{}, unscanned)
	if container(t, pod.Containers, outcome.BuildContainer).Name != outcome.BuildContainer || len(pod.InitContainers) != 2 {
		t.Errorf("without a scanner the build is the main container: %d inits", len(pod.InitContainers))
	}
}

func TestProductionPoolsAreRequiredNotPreferred(t *testing.T) {
	pod := rendered(t, source.BuildRecipe{}, settings())
	term := pod.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0]
	if e := term.MatchExpressions[0]; e.Key != "kuben.dev/pool" || e.Values[0] != "build" {
		t.Errorf("pool %+v", e)
	}
	if pod.Tolerations[0].Effect != corev1.TaintEffectNoSchedule {
		t.Errorf("toleration %+v", pod.Tolerations[0])
	}
	unpinned := settings()
	unpinned.NodePool = opt.None[build.NodePool]()
	if pod := rendered(t, source.BuildRecipe{}, unpinned); pod.Affinity != nil {
		t.Error("an affinity without a pool")
	}
}

func TestPushCredentialsAndRailpackAreOptional(t *testing.T) {
	pod := rendered(t, source.BuildRecipe{}, settings())
	if !hasEnv(container(t, pod.InitContainers, outcome.BuildContainer), "DOCKER_CONFIG", "") {
		t.Error("no push credentials")
	}
	if got := container(t, pod.InitContainers, build.PlanContainer).Image; got != "alpine/git:2.49.1" {
		t.Errorf("plan image %s: an empty Railpack image is no Railpack", got)
	}
	bare := settings()
	bare.PushSecret = opt.None[string]()
	bare.RailpackImage = opt.Some("railpack:1")
	pod = rendered(t, source.BuildRecipe{}, bare)
	if len(pod.Volumes) != 2 {
		t.Errorf("%d volumes", len(pod.Volumes))
	}
	if got := container(t, pod.InitContainers, build.PlanContainer).Image; got != "railpack:1" {
		t.Errorf("plan image %s", got)
	}
}

func TestEverythingIsOwnedByTheBuildRun(t *testing.T) {
	a := attempt(t, source.BuildRecipe{})
	s := settings()
	owner := build.RunOf(a, s)
	owner.UID = types.UID("uid-1")
	job := check(build.Job(a, s, &owner, "u")).must(t)
	ref := job.OwnerReferences[0]
	if ref.Kind != "BuildRun" || ref.Controller == nil || !*ref.Controller {
		t.Errorf("owner %+v", ref)
	}
	secret := build.SourceSecret(a, s, &owner, "ghs_x")
	if secret.Immutable == nil || !*secret.Immutable || secret.OwnerReferences[0].UID != "uid-1" {
		t.Errorf("secret %+v", secret.ObjectMeta)
	}
	if secret.Labels[build.AttemptLabel] != a.ID.String() {
		t.Errorf("labels %v", secret.Labels)
	}
	run := build.RunOf(a, s)
	if run.Spec.GitRef != a.Commit.String() || run.Namespace != "kuben-builds" {
		t.Errorf("run %+v", run)
	}
}

// The build scripts are pinned: a change to what the pods run must be
// deliberate. They were Rust's bytes until 2.0, except SCAN_SCRIPT, whose
// database-date sed was fixed (TestTheScanReportCarriesTheDatabaseDate).
func TestTheScriptsArePinned(t *testing.T) {
	want := map[string]string{
		"FETCH_SCRIPT": "0a7c532321f25903f7b7d79e3931aec4595291187d975771484ecee09695059c",
		"PLAN_SCRIPT":  "999e1e5057fe636fbac9deb7fee06c4f52987a0b2c1dd1205bb47ace634b7185",
		"BUILD_SCRIPT": "a23464ba28932a1417f16ff3293c703b59649af441408d6ac4e11f304f5fc23d",
		"SCAN_SCRIPT":  "61624fcbbeefceb6628a625e6fb1ba0b54f34eea8fbaa1869a787363133e4bb1",
	}
	pod := rendered(t, source.BuildRecipe{}, settings())
	all := append(slices.Clone(pod.InitContainers), pod.Containers...)
	got := map[string]string{
		"FETCH_SCRIPT": container(t, all, outcome.FetchContainer).Command[2],
		"PLAN_SCRIPT":  container(t, all, build.PlanContainer).Command[2],
		"BUILD_SCRIPT": container(t, all, outcome.BuildContainer).Command[2],
		"SCAN_SCRIPT":  build.ScanScript,
	}
	for name, script := range got {
		sum := sha256.Sum256([]byte(script))
		if hex.EncodeToString(sum[:]) != want[name] {
			t.Errorf("%s changed", name)
		}
	}
}
