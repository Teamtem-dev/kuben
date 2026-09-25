package serve

// Git sources and builds in `kuben serve` (serve.rs github_app,
// spawn_builds and ensure_build_namespace).

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/config"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/integrations/github"
	"github.com/Teamtem-dev/kuben/go/hub/internal/integrations/oci"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/build"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/health"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/leader"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/registry"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/supervise"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

// buildNamespace is the namespace of build Jobs unless build.namespace
// names one: never Kuben's own, whose Secrets build pods must not share.
const buildNamespace = "kuben-builds"

// githubApp is the GitHub App for Git sources; none when neither its id
// nor its key file is configured, and an error that stops the server when
// it is configured but unusable.
func githubApp(cfg config.Config, logger *slog.Logger) (opt.Val[*github.App], error) {
	git := cfg.Git
	if git.GithubAppID.IsNone() && git.GithubPrivateKeyFile.IsNone() {
		return opt.None[*github.App](), nil
	}
	app, err := github.New(git, clock.System{})
	if err != nil {
		return opt.None[*github.App](), fmt.Errorf("Git sources: %w", err) //nolint:staticcheck // Rust's text
	}
	logger.Info("GitHub App ready for Git sources", "api", git.GithubAPIURL)
	return opt.Some(app), nil
}

// startBuilds starts the build worker (M3, ADR-028) and the rescans, when
// builds are enabled on a controller replica with a cluster and the GitHub
// App is configured. The build namespace is created when missing; rootless
// BuildKit needs the `privileged` Pod Security level there (for its own
// seccomp and AppArmor profile, never a privileged container).
func startBuilds(ctx context.Context, cfg config.Config, st *store.Store, cluster opt.Val[*registry.Registry],
	app opt.Val[*github.App], h *health.Health, logger *slog.Logger,
) ([]<-chan struct{}, error) {
	b := cfg.Build
	r, inCluster := cluster.Get()
	if !b.Enabled || !cfg.HasRole(config.RoleController) || !inCluster {
		return nil, nil
	}
	provider, ok := app.Get()
	if !ok {
		logger.Warn("build.enabled is set, but Git sources are not configured (git.github_app_id): no builds run")
		return nil, nil
	}
	for _, image := range b.UnpinnedImages() {
		logger.Warn("a build image is not pinned by digest; pin it as image@sha256:… in production", "image", image)
	}
	namespace := b.Namespace.Or("")
	if namespace == "" {
		namespace = buildNamespace
	}
	primary := r.Primary()
	if err := ensureBuildNamespace(ctx, primary.Typed, namespace); err != nil {
		return nil, fmt.Errorf("preparing the build namespace %s: %w", namespace, err)
	}
	credentials := opt.None[string]()
	if file := b.RegistryAuthFile.Or(""); file != "" {
		text, err := os.ReadFile(file) //nolint:gosec // the operator configures the file
		if err != nil {
			return nil, fmt.Errorf("reading build.registry_auth_file %s: %w", file, err)
		}
		credentials = opt.Some(string(text))
	}
	settings := build.SettingsFromConfig(b, namespace)
	var done []<-chan struct{}
	if settings.ScannerImage.IsSome() {
		rescanner := &build.Rescanner{
			Store: st, Client: primary.Typed, Settings: settings,
			Interval: clock.Seconds(uint64(max(b.RescanHours, 1)) * 3600), Clock: clock.System{}, Logger: logger,
		}
		done = append(done, supervise.Go(ctx, build.RescanSubsystem, h, logger, func(ctx context.Context) error {
			return build.RunRescans(ctx, rescanner, h)
		}))
	} else {
		logger.Warn("build.scanner_image is empty: images are not scanned and scans are unavailable")
	}
	worker := build.NewWorker(build.Deps{
		Store: st, Client: primary.Typed, Dynamic: primary.Dynamic, ID: leader.Identity(), Provider: provider,
		Verifier: oci.NewVerifier(b.InsecureRegistry, credentials), Settings: settings,
		Limits: store.SlotLimits{Total: b.MaxConcurrent, PerOrg: b.MaxConcurrentPerOrg}, Clock: clock.System{}, Logger: logger,
	})
	logger.Info("build worker ready", "namespace", namespace, "max_concurrent", b.MaxConcurrent)
	return append(done, supervise.Go(ctx, build.Subsystem, h, logger, func(ctx context.Context) error {
		return build.Run(ctx, worker, h)
	})), nil
}

// ensureBuildNamespace creates the namespace name when it is missing.
func ensureBuildNamespace(ctx context.Context, client kubernetes.Interface, name string) error {
	namespaces := client.CoreV1().Namespaces()
	if _, err := namespaces.Get(ctx, name, metav1.GetOptions{}); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("reading the namespace: %w", err)
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{
		v1alpha1.LabelManagedBy:              v1alpha1.LabelManagerValue,
		"pod-security.kubernetes.io/enforce": "privileged",
		"pod-security.kubernetes.io/warn":    "baseline",
	}}}
	_, err := namespaces.Create(ctx, ns, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) && !apierrors.IsConflict(err) {
		return fmt.Errorf("creating the namespace: %w", err)
	}
	return nil
}
