// Package envtest is test support for code that talks to Kubernetes: a
// real API server (envtest's kube-apiserver and etcd; no controllers, no
// kubelet) shared by the tests of a package. It stands in for the Rust
// tests marked `#[ignore]` that ran against a kind cluster.
//
// The binaries come from KUBEBUILDER_ASSETS (`setup-envtest use -p path`).
// Without it a test that needs the server skips, unless KUBEN_REQUIRE_K8S=1
// (CI sets it), which turns the skip into a failure, as pgtest does for
// PostgreSQL.
package envtest

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/Teamtem-dev/kuben/internal/kube/controller"
)

// AssetsVar names the directory of the envtest binaries.
const AssetsVar = "KUBEBUILDER_ASSETS"

// RequireVar set to 1 makes a missing API server a failure instead of a skip.
const RequireVar = "KUBEN_REQUIRE_K8S"

// establishTimeout bounds waiting for the CRDs to be served.
const establishTimeout = 60 * time.Second

// Server is the API server of a test package; the zero value is "none".
type Server struct {
	cfg *rest.Config
}

// Main runs the tests of a package with an API server when the envtest
// binaries are there, and returns the exit code for os.Exit:
//
//	var apiserver envtest.Server
//	func TestMain(m *testing.M) { os.Exit(envtest.Main(m, &apiserver)) }
func Main(m *testing.M, s *Server) int {
	if os.Getenv(AssetsVar) == "" {
		return m.Run()
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "envtest: start the API server: %v\n", err)
		return 1
	}
	s.cfg = cfg
	code := m.Run()
	if err := env.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "envtest: stop the API server: %v\n", err)
	}
	return code
}

// Config is the API server's client configuration; the test skips (or,
// with KUBEN_REQUIRE_K8S=1, fails) when there is none.
func (s *Server) Config(t testing.TB) *rest.Config {
	t.Helper()
	if s.cfg == nil {
		if os.Getenv(RequireVar) == "1" {
			t.Fatalf("%s=1 but %s is not set: this Kubernetes test must run", RequireVar, AssetsVar)
		}
		t.Skipf("skipped %s: set %s (setup-envtest use -p path) to run it against an API server", t.Name(), AssetsVar)
	}
	return rest.CopyConfig(s.cfg)
}

// Clients are the clients of the server a test uses.
type Clients struct {
	Config  *rest.Config
	Typed   kubernetes.Interface
	Dynamic dynamic.Interface
	Runtime client.Client
}

// Connect is the server's clients, after installing Kuben's CRDs as
// `kuben serve` does (server-side apply) and waiting until they are served.
func (s *Server) Connect(t testing.TB) Clients {
	t.Helper()
	cfg := s.Config(t)
	typed, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("envtest: typed client: %v", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("envtest: dynamic client: %v", err)
	}
	rt, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatalf("envtest: runtime client: %v", err)
	}
	if err := controller.EnsureCRDs(t.Context(), rt, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("envtest: the API server refused Kuben's CRDs: %v", err)
	}
	waitEstablished(t, dyn)
	return Clients{Config: cfg, Typed: typed, Dynamic: dyn, Runtime: rt}
}

// crdResource is the CustomResourceDefinition resource.
var crdResource = schema.GroupVersionResource{ //nolint:gochecknoglobals // a constant
	Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions",
}

func waitEstablished(t testing.TB, dyn dynamic.Interface) {
	t.Helper()
	crds, err := controller.CRDs()
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(establishTimeout) //nolint:forbidigo // test support polling a real server
	for _, crd := range crds {
		for !established(t.Context(), dyn, crd.GetName()) {
			if time.Now().After(deadline) { //nolint:forbidigo // as above
				t.Fatalf("envtest: CRD %s is not established", crd.GetName())
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func established(ctx context.Context, dyn dynamic.Interface, name string) bool {
	live, err := dyn.Resource(crdResource).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false
	}
	conditions, _, err := unstructured.NestedSlice(live.Object, "status", "conditions")
	if err != nil {
		return false
	}
	for _, c := range conditions {
		if m, ok := c.(map[string]any); ok && m["type"] == "Established" && m["status"] == "True" {
			return true
		}
	}
	return false
}

// Namespace is a throwaway namespace named after prefix, deleted when the
// test ends.
func Namespace(t testing.TB, c Clients, prefix string) string {
	t.Helper()
	name := prefix + "-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if _, err := c.Typed.CoreV1().Namespaces().Create(t.Context(), ns, metav1.CreateOptions{}); err != nil {
		t.Fatalf("envtest: create namespace %s: %v", name, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err := c.Typed.CoreV1().Namespaces().Delete(ctx, name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("envtest: delete namespace %s: %v", name, err)
		}
	})
	return name
}

// IsInvalid reports whether err is the API server's 422.
func IsInvalid(err error) bool { return apierrors.IsInvalid(err) }
