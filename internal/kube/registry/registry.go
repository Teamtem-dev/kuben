// Package registry holds the clients of the clusters Kuben manages
// (crates/kuben-platform/src/registry.rs). It is immutable after
// construction and every operation names its cluster (Invariant I-11);
// there is one cluster, "primary", and the type is keyed so more are
// additive.
package registry

import (
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/Teamtem-dev/kuben/internal/core/config"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
)

// Primary is the id of the cluster Kuben runs against.
const Primary = "primary"

// podNamespaceFile is mounted into every pod with a service-account token.
const podNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// Cluster is one cluster's clients.
type Cluster struct {
	ID      string
	Config  *rest.Config
	Typed   kubernetes.Interface
	Dynamic dynamic.Interface
}

// Registry is the clusters by id.
type Registry struct {
	clusters map[string]Cluster
}

// FromConfig builds the registry for the server. Unless kube.required is
// set, a failure (no kubeconfig, bad credentials) yields no registry and
// no error, so the API and the console still start for setup and
// diagnosis.
func FromConfig(cfg config.KubeCfg, logger *slog.Logger) (opt.Val[*Registry], error) {
	r, err := Connect(cfg)
	if err != nil {
		if !cfg.Required {
			logger.Warn("kubernetes cluster unavailable; starting without cluster", "error", err)
			return opt.None[*Registry](), nil
		}
		return opt.None[*Registry](), err
	}
	return opt.Some(r), nil
}

// Connect builds the clients of the configured cluster: kube.kubeconfig
// and kube.context when set, else the usual kubeconfig search ($KUBECONFIG,
// ~/.kube/config), else the in-cluster service account. Error messages are
// redacted.
func Connect(cfg config.KubeCfg) (*Registry, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if path, ok := cfg.Kubeconfig.Get(); ok {
		rules.ExplicitPath = path
	}
	overrides := &clientcmd.ConfigOverrides{}
	if ctx, ok := cfg.Context.Get(); ok {
		overrides.CurrentContext = ctx
	}
	restCfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("%s", RedactCredentials(err.Error()))
	}
	c, err := NewCluster(Primary, restCfg)
	if err != nil {
		return nil, err
	}
	return Single(c), nil
}

// NewCluster builds the clients of one cluster from its REST config.
func NewCluster(id string, restCfg *rest.Config) (Cluster, error) {
	restCfg = rest.CopyConfig(restCfg)
	restCfg.UserAgent = "kuben"
	typed, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return Cluster{}, fmt.Errorf("%s", RedactCredentials(err.Error()))
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return Cluster{}, fmt.Errorf("%s", RedactCredentials(err.Error()))
	}
	return Cluster{ID: id, Config: restCfg, Typed: typed, Dynamic: dyn}, nil
}

// Single is a registry of exactly one cluster, the primary.
func Single(c Cluster) *Registry {
	c.ID = Primary
	return &Registry{clusters: map[string]Cluster{Primary: c}}
}

// Get is the cluster id.
func (r *Registry) Get(id string) (Cluster, bool) {
	c, ok := r.clusters[id]
	return c, ok
}

// Primary is the primary cluster; a registry always has one.
func (r *Registry) Primary() Cluster { return r.clusters[Primary] }

// OwnNamespace is the namespace Kuben runs in: configured (kube.namespace)
// when set, else the pod's service-account namespace, else none (a binary
// outside the cluster).
func OwnNamespace(configured opt.Val[string]) opt.Val[string] {
	if ns := strings.TrimSpace(configured.Or("")); ns != "" {
		return opt.Some(ns)
	}
	data, err := os.ReadFile(podNamespaceFile)
	if err != nil {
		return opt.None[string]()
	}
	if ns := strings.TrimSpace(string(data)); ns != "" {
		return opt.Some(ns)
	}
	return opt.None[string]()
}

var authority = regexp.MustCompile(`://[^/?#"'<>\s]*`)

// RedactCredentials replaces the `user:password@` part of every URL in
// text with `***@`: cluster errors quote proxy and server URLs, whose
// credentials must never reach logs, health details or a terminal.
func RedactCredentials(text string) string {
	return authority.ReplaceAllStringFunc(text, func(m string) string {
		at := strings.LastIndexByte(m, '@')
		if at < 0 {
			return m
		}
		return "://***@" + m[at+1:]
	})
}
