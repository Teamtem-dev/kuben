package server_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/health"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/integrations/github"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/registry"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/server"
)

func TestTheBuildNamespaceIsCreatedForRootlessBuildKit(t *testing.T) {
	client := fake.NewClientset()
	ctx := t.Context()
	for range 2 {
		if err := server.EnsureBuildNamespace(ctx, client, "kuben-builds"); err != nil {
			t.Fatal(err)
		}
	}
	ns, err := client.CoreV1().Namespaces().Get(ctx, "kuben-builds", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"app.kubernetes.io/managed-by":       "kuben",
		"pod-security.kubernetes.io/enforce": "privileged",
		"pod-security.kubernetes.io/warn":    "baseline",
	} {
		if got := ns.Labels[key]; got != want {
			t.Errorf("label %s %q, want %q", key, got, want)
		}
	}

	// A namespace the operator made is used as it is.
	own := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "builds", Labels: map[string]string{"a": "b"}}})
	if err := server.EnsureBuildNamespace(ctx, own, "builds"); err != nil {
		t.Fatal(err)
	}
	ns, err = own.CoreV1().Namespaces().Get(ctx, "builds", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ns.Labels) != 1 {
		t.Errorf("the operator's namespace changed: %v", ns.Labels)
	}
}

// app is a GitHub App with a fresh key.
func app(t *testing.T) (config.GitCfg, *github.App) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "app.pem")
	block := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(path, block, 0o600); err != nil {
		t.Fatal(err)
	}
	git := config.DefaultGitCfg()
	git.GithubAppID = opt.Some[uint64](7)
	git.GithubPrivateKeyFile = opt.Some(path)
	git.GithubWebhookSecret = config.Secret("hook")
	var cfg config.Config
	cfg.Git = git
	got, err := server.GithubApp(cfg, quiet())
	a, ok := got.Get()
	if err != nil || !ok {
		t.Fatalf("app: %v", err)
	}
	return git, a
}

func TestGitSourcesAreOffUnlessConfiguredAndStopTheServerWhenBroken(t *testing.T) {
	var cfg config.Config
	cfg.Git = config.DefaultGitCfg()
	if got, err := server.GithubApp(cfg, quiet()); err != nil || got.IsSome() {
		t.Errorf("unconfigured: %v %v", got, err)
	}
	cfg.Git.GithubAppID = opt.Some[uint64](7)
	cfg.Git.GithubPrivateKeyFile = opt.Some(filepath.Join(t.TempDir(), "missing.pem"))
	cfg.Git.GithubWebhookSecret = config.Secret("hook")
	if _, err := server.GithubApp(cfg, quiet()); err == nil || !strings.HasPrefix(err.Error(), "Git sources: ") {
		t.Errorf("an unreadable key: %v", err)
	}
}

func TestBuildsRunOnlyOnAControllerWithAClusterAndGitSources(t *testing.T) {
	git, a := app(t)
	h := health.New(clock.System{})
	enabled := func(roles ...config.Role) config.Config {
		var cfg config.Config
		cfg.Git = git
		cfg.Build = config.DefaultBuildCfg()
		cfg.Build.Enabled = true
		cfg.Server.Roles = roles
		return cfg
	}
	disabled := enabled(config.RoleController)
	disabled.Build.Enabled = false
	cases := []struct {
		name    string
		cfg     config.Config
		cluster bool
		app     opt.Val[*github.App]
	}{
		{"disabled", disabled, true, opt.Some(a)},
		{"api only", enabled(config.RoleAPI), true, opt.Some(a)},
		{"no cluster", enabled(config.RoleController), false, opt.Some(a)},
		{"no Git sources", enabled(config.RoleController), true, opt.None[*github.App]()},
	}
	for _, c := range cases {
		r := fakeRegistry()
		cluster := opt.None[*registry.Registry]()
		if c.cluster {
			cluster = opt.Some(r)
		}
		done, err := server.StartBuilds(t.Context(), c.cfg, nil, cluster, c.app, h, quiet())
		if err != nil || len(done) != 0 {
			t.Errorf("%s: %d started, %v", c.name, len(done), err)
		}
		list, listErr := r.Primary().Typed.CoreV1().Namespaces().List(t.Context(), metav1.ListOptions{})
		if listErr != nil {
			t.Fatal(listErr)
		}
		if len(list.Items) != 0 {
			t.Errorf("%s: a namespace was created", c.name)
		}
	}

	// Unreadable registry credentials stop the server; the namespace is
	// ready by then.
	cfg := enabled(config.RoleController)
	cfg.Build.Namespace = opt.Some("builds")
	cfg.Build.RegistryAuthFile = opt.Some(filepath.Join(t.TempDir(), "missing"))
	r := fakeRegistry()
	_, err := server.StartBuilds(t.Context(), cfg, nil, opt.Some(r), opt.Some(a), h, quiet())
	if err == nil || !strings.HasPrefix(err.Error(), "reading build.registry_auth_file ") {
		t.Errorf("unreadable credentials: %v", err)
	}
	if _, err := r.Primary().Typed.CoreV1().Namespaces().Get(t.Context(), "builds", metav1.GetOptions{}); err != nil {
		t.Errorf("the build namespace: %v", err)
	}
}
