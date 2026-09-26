package setup

// The managed path (platform.rs, M2.7, ADR-031): a pinned k3s, installed
// with a verified installer and with reservations, and on it what makes
// apps reachable over HTTPS: k3s's Traefik as the Gateway API provider, the
// Gateway API CRDs (standard channel), cert-manager with Gateway API
// support, a Let's Encrypt ClusterIssuer when an email is given, and the
// KubenConfig that has Kuben create and own its Gateway.
//
// Every object is created only when it is missing and recorded in the
// journal; what is there already is used and never upgraded or taken over.
// A cluster brought with --kubeconfig gets nothing: `kuben doctor` reports
// what it lacks.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/bundlelock"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/install/journal"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/render"
	"github.com/Teamtem-dev/kuben/packages/api/v1alpha1"
)

// Names in the cluster.
const (
	// GatewayClass is the GatewayClass of k3s's Traefik.
	GatewayClass = "traefik"
	// Issuer is the ClusterIssuer setup creates.
	Issuer = "letsencrypt"
	// GatewayNamespace is Kuben's own namespace (its Gateway, the agent).
	GatewayNamespace = "kuben-system"
	// ConsoleNamespace is where the console's route lives: a namespace of
	// Kuben's own label, which the listeners of Kuben's Gateway accept
	// routes from.
	ConsoleNamespace = "kuben-console"

	// Journal names of the cluster objects setup may create.
	CRDs          = "crd/gateway-api"
	TraefikConfig = "helmchartconfig/kube-system/traefik"
	CertManager   = "helmchart/kube-system/kuben-cert-manager"
	Namespace     = "namespace/kuben-system"
	ClusterIssuer = "clusterissuer/letsencrypt"
	KubenConfig   = "kubenconfig/kuben"
	Agent         = "agent/kuben-system/kuben-agent"
	Console       = "console/kuben-console/kuben-console"

	// The entry points the listeners of Traefik use.
	traefikHTTPPort  int64 = 8000
	traefikHTTPSPort int64 = 8443
	// fieldManager is the field manager and label value of what setup
	// writes into the cluster.
	fieldManager = "kuben-setup"
	managedBy    = v1alpha1.LabelManagedBy
	// k3sTraefikManifest is k3s's manifest of its bundled Traefik; its
	// HelmCharts follow from it.
	k3sTraefikManifest = "/var/lib/rancher/k3s/server/manifests/traefik.yaml"
)

// agentManifest is the agent's manifest, a byte-identical copy of the Helm
// chart's charts/kuben/files/agent.yaml (a test holds them together).
//
//go:embed agent.yaml
var agentManifest string

// Datastore is how k3s keeps its own state.
type Datastore string //nolint:recvcheck // Set mutates flag, accessors are value receivers

// The datastores, with their command-line names.
const (
	// DatastoreSqlite is SQLite: one server.
	DatastoreSqlite Datastore = "sqlite"
	// DatastoreEtcd is embedded etcd: a server that will grow to several.
	DatastoreEtcd Datastore = "etcd"
)

// String is the command-line name; the zero value is the default, sqlite.
func (d Datastore) String() string {
	if d == "" {
		return string(DatastoreSqlite)
	}
	return string(d)
}

// Set reads a command-line value as clap's ValueEnum did.
func (d *Datastore) Set(value string) error {
	switch v := Datastore(value); v {
	case DatastoreSqlite, DatastoreEtcd:
		*d = v
		return nil
	}
	return fmt.Errorf("invalid value '%s' for '--datastore <DATASTORE>'\n  [possible values: sqlite, etcd]", value)
}

// Type names the value in help.
func (Datastore) Type() string { return "DATASTORE" }

// debugName is the datastore as Rust's `{:?}` printed it.
func (d Datastore) debugName() string {
	if d == DatastoreEtcd {
		return "Etcd"
	}
	return "Sqlite"
}

// Wanted is what the platform step is asked for.
type Wanted struct {
	Domain      opt.Val[string]
	AcmeEmail   opt.Val[string]
	AcmeStaging bool
}

// k3sExec is INSTALL_K3S_EXEC for datastore: reservations for the system
// and Kubernetes, and eviction before the node runs out.
func k3sExec(datastore Datastore) string {
	exec := []string{
		"server",
		"--kubelet-arg=system-reserved=cpu=100m,memory=256Mi",
		"--kubelet-arg=kube-reserved=cpu=100m,memory=256Mi",
		"--kubelet-arg=eviction-hard=memory.available<100Mi,nodefs.available<10%",
	}
	if datastore == DatastoreEtcd {
		exec = append(exec, "--cluster-init")
	}
	return strings.Join(exec, " ")
}

// downloadVerified downloads url to to with curl and checks its SHA-256;
// its content.
func (m *machine) downloadVerified(ctx context.Context, url, sum, to string) ([]byte, error) {
	if m.which("curl").IsNone() {
		return nil, errors.New("install curl first (apt-get install -y curl), then run kuben setup again")
	}
	if _, err := m.run(ctx, "curl", "-fsSL", "--proto", "=https", "--tlsv1.2", "-o", to, url); err != nil {
		return nil, err
	}
	body, err := os.ReadFile(to) //nolint:gosec // our own download
	if err != nil {
		return nil, err //nolint:wrapcheck // names the path
	}
	got := sha256Hex(body)
	if got != sum {
		_ = os.Remove(to) //nolint:errcheck // refused either way
		return nil, fmt.Errorf("%s does not match its pinned checksum (sha256 %s, expected %s); nothing was run", url, got, sum)
	}
	return body, nil
}

func sha256Hex(body []byte) string {
	h := sha256.Sum256(body)
	return hex.EncodeToString(h[:])
}

// tempFile is a file name in the temporary directory for this process.
func tempFile(prefix, suffix string) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("%s-%d%s", prefix, os.Getpid(), suffix))
}

// installK3s installs the locked k3s with its verified installer.
func (m *machine) installK3s(ctx context.Context, datastore Datastore) error {
	b, err := bundlelock.Get()
	if err != nil {
		return err //nolint:wrapcheck // explains itself
	}
	k3s := b.K3s
	st := m.ui.Step("Installing k3s " + k3s.Version)
	defer st.Close()
	script := tempFile("kuben-k3s-install", ".sh")
	if _, err := m.downloadVerified(ctx, k3s.Installer.URL, k3s.Installer.SHA256, script); err != nil {
		st.Fail("the installer could not be verified")
		return err
	}
	exec := k3sExec(datastore)
	st.Command(fmt.Sprintf("INSTALL_K3S_VERSION=%s INSTALL_K3S_EXEC=\"%s\" sh install.sh", k3s.Version, exec))
	out, err := m.runner.output(ctx, []string{"INSTALL_K3S_VERSION=" + k3s.Version, "INSTALL_K3S_EXEC=" + exec}, "sh", script)
	_ = os.Remove(script) //nolint:errcheck // a temporary file
	if err != nil {
		return fmt.Errorf("running the k3s installer: %w", err)
	}
	if !out.ok {
		st.Fail("the k3s installer failed")
		m.ui.Note(tail(out.stdout, out.stderr, 12))
		return errors.New("k3s did not install; the lines above are its last output (journalctl -u k3s has more)")
	}
	st.Done(fmt.Sprintf("%s, %s datastore", k3s.Version, datastore.debugName()))
	return nil
}

// eventually polls check every two seconds until it says yes or timeout
// passes.
func (m *machine) eventually(ctx context.Context, timeout time.Duration, check func() bool) bool {
	deadline := m.clock.NowMs() + timeout.Milliseconds()
	for {
		if check() {
			return true
		}
		if m.clock.NowMs() >= deadline {
			return false
		}
		if m.sleep(ctx, 2*time.Second) != nil {
			return false
		}
	}
}

func labelled(value map[string]any) map[string]any {
	meta, ok := value["metadata"].(map[string]any)
	if !ok {
		meta = map[string]any{}
		value["metadata"] = meta
	}
	meta["labels"] = map[string]any{managedBy: fieldManager}
	return value
}

func traefikConfig() map[string]any {
	return labelled(map[string]any{
		"apiVersion": "helm.cattle.io/v1",
		"kind":       "HelmChartConfig",
		"metadata":   map[string]any{"name": "traefik", "namespace": "kube-system"},
		"spec": map[string]any{
			"valuesContent": "providers:\n  kubernetesGateway:\n    enabled: true\ngateway:\n  enabled: false\n",
		},
	})
}

// certManagerValues are cert-manager's values: its CRDs, Gateway API
// support, and every image pinned by the digest of the bundle lock.
func certManagerValues(images map[string]string) string {
	values := "crds:\n  enabled: true\n  keep: true\nstartupapicheck:\n  enabled: false\n  image:\n    digest: STARTUP\n" +
		"config:\n  apiVersion: controller.config.cert-manager.io/v1alpha1\n  kind: ControllerConfiguration\n  enableGatewayAPI: true\n" +
		"image:\n  digest: CONTROLLER\n"
	for _, component := range []string{"webhook", "cainjector", "acmesolver"} {
		values += fmt.Sprintf("%s:\n  image:\n    digest: %s\n", component, images[component])
	}
	values = strings.ReplaceAll(values, "STARTUP", images["startupapicheck"])
	return strings.ReplaceAll(values, "CONTROLLER", images["controller"])
}

// certManagerChart is cert-manager from the chart archive setup downloaded
// and verified (chart, base64): k3s installs exactly those bytes.
func certManagerChart(cm bundlelock.CertManager, chart string) map[string]any {
	return labelled(map[string]any{
		"apiVersion": "helm.cattle.io/v1",
		"kind":       "HelmChart",
		"metadata":   map[string]any{"name": "kuben-cert-manager", "namespace": "kube-system"},
		"spec": map[string]any{
			"chart":           "cert-manager",
			"version":         cm.Version,
			"chartContent":    chart,
			"targetNamespace": "cert-manager",
			"createNamespace": true,
			"valuesContent":   certManagerValues(cm.Images),
		},
	})
}

// certManagerArchive is the pinned cert-manager chart archive, base64.
func (m *machine) certManagerArchive(ctx context.Context, cm bundlelock.CertManager) (string, error) {
	file := tempFile("kuben-cert-manager", ".tgz")
	body, err := m.downloadVerified(ctx, cm.Chart.URL, cm.Chart.SHA256, file)
	_ = os.Remove(file) //nolint:errcheck // a temporary file
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(body), nil
}

func namespaceObject() map[string]any {
	return labelled(map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata":   map[string]any{"name": GatewayNamespace},
	})
}

// clusterIssuer is the Let's Encrypt ClusterIssuer for email.
func clusterIssuer(email string, staging bool) map[string]any {
	server := "https://acme-v02.api.letsencrypt.org/directory"
	if staging {
		server = "https://acme-staging-v02.api.letsencrypt.org/directory"
	}
	return labelled(map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "ClusterIssuer",
		"metadata":   map[string]any{"name": Issuer},
		"spec": map[string]any{"acme": map[string]any{
			"server":              server,
			"email":               email,
			"privateKeySecretRef": map[string]any{"name": Issuer + "-account"},
			// HTTP-01 through Kuben's own Gateway: the solver route attaches
			// to its plain-HTTP listener.
			"solvers": []any{map[string]any{"http01": map[string]any{"gatewayHTTPRoute": map[string]any{
				"parentRefs": []any{map[string]any{
					"group":       "gateway.networking.k8s.io",
					"kind":        "Gateway",
					"name":        "kuben",
					"namespace":   GatewayNamespace,
					"sectionName": "http",
				}},
			}}}},
		}},
	})
}

// kubenConfig is the KubenConfig fields setup owns.
func kubenConfig(wanted Wanted, issuer bool) map[string]any {
	spec := map[string]any{
		"gatewayClassName": GatewayClass,
		"gatewayPorts":     map[string]any{"http": traefikHTTPPort, "https": traefikHTTPSPort},
	}
	if issuer {
		spec["clusterIssuer"] = Issuer
	}
	if domain, ok := wanted.Domain.Get(); ok {
		spec["baseDomain"] = domain
	}
	return labelled(map[string]any{
		"apiVersion": v1alpha1.SchemeGroupVersion.String(),
		"kind":       v1alpha1.KubenConfigKind,
		"metadata":   map[string]any{"name": "kuben"},
		"spec":       spec,
	})
}

func object(value map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: value}
}

// existing says whether obj exists, and whether setup wrote it (its field
// manager).
func existing(ctx context.Context, c cluster, obj *unstructured.Unstructured) (opt.Val[bool], error) {
	live, found, err := c.get(ctx, obj.GetAPIVersion(), obj.GetKind(), obj.GetNamespace(), obj.GetName())
	if err != nil || !found {
		return opt.None[bool](), err
	}
	ours := slices.ContainsFunc(live.GetManagedFields(), func(f metav1.ManagedFieldsEntry) bool { return f.Manager == fieldManager })
	return opt.Some(ours), nil
}

// revision tells whether an object was changed: its generation where it
// has one (status updates leave it alone), else its resource version.
type revision struct {
	generation    int64
	hasGeneration bool
	version       string
}

func revisionOf(u *unstructured.Unstructured) revision {
	if u == nil {
		return revision{}
	}
	if g, found, err := unstructured.NestedInt64(u.Object, "metadata", "generation"); err == nil && found {
		return revision{generation: g, hasGeneration: true}
	}
	return revision{version: u.GetResourceVersion()}
}

// applyObject applies obj as setup; force takes fields over from other
// managers (only for objects setup created). Whether it changed anything:
// created it, or changed what it holds.
func applyObject(ctx context.Context, c cluster, obj *unstructured.Unstructured, force bool) (bool, error) {
	live, found, err := c.get(ctx, obj.GetAPIVersion(), obj.GetKind(), obj.GetNamespace(), obj.GetName())
	if err != nil {
		return false, err
	}
	after, err := c.apply(ctx, obj, force)
	if err != nil {
		return false, err
	}
	if !found || live == nil {
		return true, nil
	}
	return revisionOf(live) != revisionOf(after), nil
}

// ensureObject creates value when it is missing and records who owns it;
// true when setup wrote it now.
func ensureObject(ctx context.Context, c cluster, book *journal.Book, name string, value map[string]any) (bool, error) {
	obj := object(value)
	present, err := existing(ctx, c, obj)
	if err != nil {
		return false, err
	}
	created := true
	if ours, ok := present.Get(); ok {
		// Setup's own object: kept in step with this version.
		created = ours && book.Journal().Owns(journal.KindKubernetesObject, name)
	}
	if _, err := book.Claim(journal.KindKubernetesObject, name, created); err != nil {
		return false, err
	}
	if book.Journal().Owns(journal.KindKubernetesObject, name) {
		return applyObject(ctx, c, obj, true)
	}
	return false, nil
}

func crdEstablished(ctx context.Context, c cluster, name string) bool {
	crd, found, err := c.get(ctx, "apiextensions.k8s.io/v1", "CustomResourceDefinition", "", name)
	if err != nil || !found || crd == nil {
		return false
	}
	return conditionTrue(crd.Object, "Established", "status", "conditions")
}

// documents are the YAML documents of text as JSON objects (integers as
// int64); empty documents are skipped.
func documents(text string) ([]map[string]any, error) {
	reader := utilyaml.NewYAMLReader(bufio.NewReader(strings.NewReader(text)))
	out := []map[string]any{}
	for {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err //nolint:wrapcheck // a YAML error
		}
		data, err := yaml.YAMLToJSON(doc)
		if err != nil {
			return nil, err //nolint:wrapcheck // a YAML error
		}
		if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
			continue
		}
		var value map[string]any
		if err := utiljson.Unmarshal(data, &value); err != nil {
			return nil, err //nolint:wrapcheck // a JSON error
		}
		if value != nil {
			out = append(out, value)
		}
	}
}

// ensureGatewayAPI installs the Gateway API CRDs when the cluster has
// none; never upgrades them.
func (m *machine) ensureGatewayAPI(ctx context.Context, c cluster, book *journal.Book, pinned bundlelock.GatewayAPI) (string, error) {
	if crdEstablished(ctx, c, "gateways.gateway.networking.k8s.io") {
		if _, err := book.Claim(journal.KindKubernetesObject, CRDs, false); err != nil {
			return "", err
		}
		return "Gateway API CRDs already installed (kept as they are)", nil
	}
	file := tempFile("kuben-gateway-api", ".yaml")
	body, err := m.downloadVerified(ctx, pinned.URL, pinned.SHA256, file)
	if err != nil {
		return "", err
	}
	_ = os.Remove(file) //nolint:errcheck // a temporary file
	if _, err := book.Claim(journal.KindKubernetesObject, CRDs, true); err != nil {
		return "", err
	}
	docs, err := documents(string(body))
	if err != nil {
		return "", err
	}
	for _, d := range docs {
		if _, err := applyObject(ctx, c, object(d), true); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("Gateway API %s CRDs installed", pinned.Version), nil
}

// ensurePlatform is everything before the service starts: Gateway API
// CRDs, Traefik as the Gateway provider, cert-manager, Kuben's namespace,
// and the issuer.
func (m *machine) ensurePlatform(ctx context.Context, kubeconfig string, wanted Wanted, agent bool, book *journal.Book) error {
	st := m.ui.Step("Gateway, TLS and platform components")
	defer st.Close()
	b, err := bundlelock.Get()
	if err != nil {
		return err //nolint:wrapcheck // explains itself
	}
	c, err := m.connect(kubeconfig)
	if err != nil {
		return err
	}
	changed, notes, err := m.platformObjects(ctx, c, wanted, agent, book, b)
	if err != nil {
		st.Fail("could not be set up")
		return err
	}
	detail := strings.Join(notes, "; ")
	if len(notes) == 0 {
		detail = fmt.Sprintf("Traefik Gateway, Gateway API %s, cert-manager %s", b.GatewayAPI.Version, b.CertManager.Version)
	}
	if slices.ContainsFunc(notes, func(n string) bool { return strings.Contains(n, "not ") }) {
		st.Warn(detail)
	} else {
		st.Done(detail)
	}
	return book.Done(changed, detail)
}

func (m *machine) ensureCertManager(ctx context.Context, c cluster, book *journal.Book, cm bundlelock.CertManager, track func(bool, error) error) (opt.Val[string], error) {
	if crdEstablished(ctx, c, "clusterissuers.cert-manager.io") && !book.Journal().Owns(journal.KindKubernetesObject, CertManager) {
		if _, err := book.Claim(journal.KindKubernetesObject, CertManager, false); err != nil {
			return opt.None[string](), err
		}
		return opt.Some("cert-manager already installed (kept; it needs Gateway API support enabled)"), nil
	}
	archive, err := m.certManagerArchive(ctx, cm)
	if err != nil {
		return opt.None[string](), err
	}
	if err := track(ensureObject(ctx, c, book, CertManager, certManagerChart(cm, archive))); err != nil {
		return opt.None[string](), err
	}
	return opt.None[string](), nil
}

func (m *machine) ensureAcmeIssuer(ctx context.Context, c cluster, book *journal.Book, wanted Wanted, webhook bool) (bool, opt.Val[string], error) {
	email, ok := wanted.AcmeEmail.Get()
	if !ok {
		return false, opt.None[string](), nil
	}
	if !webhook {
		return false, opt.Some("cert-manager is not ready yet: run kuben setup again to add the ClusterIssuer"), nil
	}
	// The webhook answers a moment after it is Available.
	applied, err := false, errors.New("not tried")
	for range 30 {
		applied, err = ensureObject(ctx, c, book, ClusterIssuer, clusterIssuer(email, wanted.AcmeStaging))
		if err == nil {
			break
		}
		if m.sleep(ctx, 2*time.Second) != nil {
			break
		}
	}
	if err != nil {
		return false, opt.None[string](), err
	}
	return applied, opt.None[string](), nil
}

func (m *machine) platformObjects(ctx context.Context, c cluster, wanted Wanted, agent bool, book *journal.Book,
	b bundlelock.Bundle,
) (bool, []string, error) {
	changed, notes := false, []string{}
	track := func(did bool, err error) error {
		changed = changed || did
		return err
	}
	// k3s writes its bundled charts a few seconds after it starts.
	bundled := exists(k3sTraefikManifest)
	if m.eventually(ctx, 5*time.Minute, func() bool { return k3sChartDone(ctx, c, "traefik-crd", bundled) }) {
		crds, err := m.ensureGatewayAPI(ctx, c, book, b.GatewayAPI)
		if err != nil {
			return false, nil, err
		}
		changed = changed || strings.HasSuffix(crds, "installed")
		notes = append(notes, crds)
	} else {
		notes = append(notes, "k3s's traefik-crd chart has not finished yet: run kuben setup again to add the Gateway API CRDs")
	}
	if err := track(ensureObject(ctx, c, book, TraefikConfig, traefikConfig())); err != nil {
		return false, nil, err
	}
	if !book.Journal().Owns(journal.KindKubernetesObject, TraefikConfig) {
		notes = append(notes, "Traefik's HelmChartConfig is someone else's: check providers.kubernetesGateway.enabled")
	}
	if err := track(ensureObject(ctx, c, book, Namespace, namespaceObject())); err != nil {
		return false, nil, err
	}
	if agent {
		objects, err := agentObjects(m.getenv("KUBEN_AGENT_IMAGE"), m.version)
		if err != nil {
			return false, nil, err
		}
		if err := track(ensureSet(ctx, c, book, Agent, "Deployment", objects)); err != nil {
			return false, nil, err
		}
	}
	cmNote, err := m.ensureCertManager(ctx, c, book, b.CertManager, track)
	if err != nil {
		return false, nil, err
	}
	if note, ok := cmNote.Get(); ok {
		notes = append(notes, note)
	}
	if !m.eventually(ctx, 5*time.Minute, func() bool { return gatewayClassAccepted(ctx, c) }) {
		notes = append(notes, fmt.Sprintf("GatewayClass %s is not accepted yet", GatewayClass))
	}
	webhook := m.eventually(ctx, 5*time.Minute, func() bool {
		return deploymentAvailable(ctx, c, "cert-manager", "app.kubernetes.io/component=webhook")
	})
	acmeApplied, acmeNote, err := m.ensureAcmeIssuer(ctx, c, book, wanted, webhook)
	if err != nil {
		return false, nil, err
	}
	if note, ok := acmeNote.Get(); ok {
		notes = append(notes, note)
	}
	changed = changed || acmeApplied
	return changed, notes, nil
}

// ensureKubenConfig: after the service applied its CRDs, the KubenConfig
// setup owns fields of. Fields someone else set are kept; setup says so.
func (m *machine) ensureKubenConfig(ctx context.Context, kubeconfig string, wanted Wanted, book *journal.Book) error {
	st := m.ui.Step("Kuben's Gateway (KubenConfig)")
	defer st.Close()
	c, err := m.connect(kubeconfig)
	if err != nil {
		return err
	}
	_, issuer := book.Journal().OwnerOf(journal.KindKubernetesObject, ClusterIssuer)
	changed, err := m.applyKubenConfig(ctx, c, wanted, issuer, book)
	var someoneElse *someoneElsesFields
	switch {
	case errors.As(err, &someoneElse):
		st.Warn(err.Error())
		return book.Done(false, err.Error())
	case err != nil:
		st.Fail("not written")
		return err
	}
	detail := "Gateway class " + GatewayClass
	if issuer {
		detail += ", HTTPS through Let's Encrypt"
	} else {
		detail += ", plain HTTP until --acme-email is given"
	}
	if domain, ok := wanted.Domain.Get(); ok {
		detail += ", apps under " + domain
	}
	st.Done(detail)
	return book.Done(changed, detail)
}

// someoneElsesFields: someone else set KubenConfig fields (kubectl, the
// chart); they stay theirs.
type someoneElsesFields struct{ cause error }

func (e *someoneElsesFields) Error() string {
	return "KubenConfig fields are set by someone else; kept: " + e.cause.Error()
}

func (m *machine) applyKubenConfig(ctx context.Context, c cluster, wanted Wanted, issuer bool, book *journal.Book) (bool, error) {
	if !m.eventually(ctx, 2*time.Minute, func() bool { return crdEstablished(ctx, c, "kubenconfigs.kuben.dev") }) {
		return false, errors.New("the KubenConfig CRD is not established; is kuben.service running?")
	}
	obj := object(kubenConfig(wanted, issuer))
	before, err := existing(ctx, c, obj)
	if err != nil {
		return false, err
	}
	if _, err := book.Claim(journal.KindKubernetesObject, KubenConfig, before.IsNone()); err != nil {
		return false, err
	}
	changed, err := applyObject(ctx, c, obj, false)
	if err != nil && isConflict(err) {
		return false, &someoneElsesFields{cause: err}
	}
	return changed, err
}

// agentObjects are the agent's objects, as the chart renders them, in
// GatewayNamespace. image (KUBEN_AGENT_IMAGE) replaces the release image
// when set (tests, mirrors).
func agentObjects(image, version string) ([]map[string]any, error) {
	if image == "" {
		image = "ghcr.io/teamtem-dev/kuben:" + version
	}
	manifest := strings.NewReplacer(
		"__NAME__", "kuben-agent",
		"__NAMESPACE__", GatewayNamespace,
		"__INSTANCE__", "kuben",
		"__IMAGE__", image,
		"__PULL_POLICY__", "IfNotPresent",
		"__MANAGED_BY__", fieldManager,
	).Replace(agentManifest)
	objects, err := documents(manifest)
	if err != nil {
		return nil, err
	}
	for _, value := range objects {
		if kind := value["kind"]; kind != "ClusterRole" && kind != "ClusterRoleBinding" {
			meta, ok := value["metadata"].(map[string]any)
			if !ok {
				meta = map[string]any{}
				value["metadata"] = meta
			}
			meta["namespace"] = GatewayNamespace
		}
	}
	return objects, nil
}

// ensureSet applies objects, recorded together as name, when this cluster
// has no object of anchor's kind and name yet or setup made it; one someone
// else made is left alone. True when they changed now.
func ensureSet(ctx context.Context, c cluster, book *journal.Book, name, anchor string, objects []map[string]any) (bool, error) {
	i := slices.IndexFunc(objects, func(o map[string]any) bool { return o["kind"] == anchor })
	if i < 0 {
		return false, fmt.Errorf("%s has no %s", name, anchor)
	}
	present, err := existing(ctx, c, object(objects[i]))
	if err != nil {
		return false, err
	}
	created := true
	if ours, ok := present.Get(); ok {
		created = ours && book.Journal().Owns(journal.KindKubernetesObject, name)
	}
	if _, err := book.Claim(journal.KindKubernetesObject, name, created); err != nil {
		return false, err
	}
	if !book.Journal().Owns(journal.KindKubernetesObject, name) {
		return false, nil
	}
	changed := false
	for _, value := range objects {
		did, err := applyObject(ctx, c, object(value), true)
		if err != nil {
			return false, err
		}
		changed = changed || did
	}
	return changed, nil
}

// consoleObjects is the console behind Kuben's Gateway at https://host: a
// route in ConsoleNamespace to a Service whose one endpoint is this server
// (hub, port), where `kuben serve` listens outside the cluster. The Gateway
// gives host a listener and a certificate like any app's domain.
func consoleObjects(host, hub string, port uint16) []map[string]any {
	kuben := func() map[string]any { return map[string]any{managedBy: v1alpha1.LabelManagerValue} }
	gateway := render.OwnedDefaultGateway()
	family := "IPv4"
	if strings.Contains(hub, ":") {
		family = "IPv6"
	}
	domains, _ := json.Marshal([]render.DomainClaim{{Host: host, TLS: "auto"}}) //nolint:errcheck // plain strings
	return []map[string]any{
		{
			"apiVersion": "v1",
			"kind":       "Namespace",
			"metadata":   map[string]any{"name": ConsoleNamespace, "labels": kuben()},
		},
		{
			"apiVersion": "v1",
			"kind":       "Service",
			"metadata":   map[string]any{"name": "kuben-console", "namespace": ConsoleNamespace, "labels": kuben()},
			"spec": map[string]any{"ports": []any{
				map[string]any{"name": "http", "port": int64(80), "protocol": "TCP"},
			}},
		},
		{
			"apiVersion": "discovery.k8s.io/v1",
			"kind":       "EndpointSlice",
			"metadata": map[string]any{
				"name":      "kuben-console-host",
				"namespace": ConsoleNamespace,
				"labels": map[string]any{
					"kubernetes.io/service-name":             "kuben-console",
					"endpointslice.kubernetes.io/managed-by": fieldManager,
					managedBy:                                v1alpha1.LabelManagerValue,
				},
			},
			"addressType": family,
			"endpoints": []any{map[string]any{
				"addresses":  []any{hub},
				"conditions": map[string]any{"ready": true},
			}},
			"ports": []any{map[string]any{"name": "http", "port": int64(port), "protocol": "TCP"}},
		},
		{
			"apiVersion": "gateway.networking.k8s.io/v1",
			"kind":       "HTTPRoute",
			"metadata": map[string]any{
				"name":        "kuben-console",
				"namespace":   ConsoleNamespace,
				"labels":      kuben(),
				"annotations": map[string]any{render.DomainsAnnotation: string(domains)},
			},
			"spec": map[string]any{
				"parentRefs": []any{map[string]any{
					"name":        gateway.Name,
					"namespace":   gateway.Namespace,
					"sectionName": render.HostListenerName(host),
				}},
				"hostnames": []any{host},
				"rules": []any{map[string]any{
					"matches":     []any{map[string]any{"path": map[string]any{"type": "PathPrefix", "value": "/"}}},
					"backendRefs": []any{map[string]any{"name": "kuben-console", "port": int64(80)}},
				}},
			},
		},
	}
}

// ensureConsole: after the Gateway exists, the console's HTTPS address
// (M2.11).
func (m *machine) ensureConsole(ctx context.Context, kubeconfig, host, hub string, port uint16, book *journal.Book) error {
	st := m.ui.Step("The console at https://" + host)
	defer st.Close()
	c, err := m.connect(kubeconfig)
	if err != nil {
		return err
	}
	changed, err := ensureSet(ctx, c, book, Console, "HTTPRoute", consoleObjects(host, hub, port))
	if err != nil {
		st.Fail("not written")
		return err
	}
	if book.Journal().Owns(journal.KindKubernetesObject, Console) {
		detail := fmt.Sprintf("point DNS for %s at this server; the certificate follows", host)
		st.Done(detail)
		return book.Done(changed, detail)
	}
	detail := fmt.Sprintf("a route %s/kuben-console is someone else's; kept", ConsoleNamespace)
	st.Warn(detail)
	return book.Done(false, detail)
}

func gatewayClassAccepted(ctx context.Context, c cluster) bool {
	class, found, err := c.get(ctx, "gateway.networking.k8s.io/v1", "GatewayClass", "", GatewayClass)
	if err != nil || !found || class == nil {
		return false
	}
	return conditionTrue(class.Object, "Accepted", "status", "conditions")
}

// deploymentAvailable reports whether a Deployment matching selector in
// namespace is Available (the release prefixes cert-manager's names, so
// they are found by label).
func deploymentAvailable(ctx context.Context, c cluster, namespace, selector string) bool {
	items, err := c.list(ctx, "apps/v1", "Deployment", namespace, selector)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(items, func(d unstructured.Unstructured) bool {
		return conditionTrue(d.Object, "Available", "status", "conditions")
	})
}

// k3sChartDone: k3s installs its bundled charts with Jobs; `traefik-crd`
// brings CRDs of its own (the Gateway API's among them on some releases)
// and fails on any it did not create. So setup waits for it before it adds
// what is missing. True when the chart's Job completed, or when there is no
// such chart and none is expected (a k3s without Traefik, or not k3s).
func k3sChartDone(ctx context.Context, c cluster, chart string, expected bool) bool {
	// Until k3s registered its HelmChart kind, or wrote the chart.
	live, found, err := c.get(ctx, "helm.cattle.io/v1", "HelmChart", "kube-system", chart)
	if err != nil || !found || live == nil {
		return !expected
	}
	job, ok := member(live.Object, "status", "jobName").(string)
	if !ok {
		return false // Not started yet.
	}
	j, found, err := c.get(ctx, "batch/v1", "Job", "kube-system", job)
	if err != nil || !found || j == nil {
		return false
	}
	succeeded, found, err := unstructured.NestedInt64(j.Object, "status", "succeeded")
	return err == nil && found && succeeded > 0
}

// purgeObjects is `kuben uninstall --purge` on a cluster setup did not
// install: it removes the objects setup created that belong only to Kuben.
// The Gateway API CRDs and cert-manager stay: other workloads may use them
// by now (I12). The result is what was kept.
//
// With keepApps (M4.11) the cluster stays whoever installed it. The apps
// keep what they need to be served: the namespace with the Gateway, the
// Traefik settings and the ClusterIssuer that renews their certificates.
func (m *machine) purgeObjects(ctx context.Context, kubeconfig string, j *journal.Journal, keepApps bool) []string {
	owns := func(name string) bool { return j.Owns(journal.KindKubernetesObject, name) }
	kept := []string{}
	serving := []string{Namespace, TraefikConfig, ClusterIssuer}
	if keepApps {
		for _, s := range []struct{ name, what string }{
			{Namespace, "namespace kuben-system with the Gateway"},
			{TraefikConfig, "the Traefik settings"},
			{ClusterIssuer, "ClusterIssuer letsencrypt"},
		} {
			if owns(s.name) {
				kept = append(kept, s.what)
			}
		}
	}
	if owns(CRDs) {
		kept = append(kept, "the Gateway API CRDs")
	}
	if owns(CertManager) {
		kept = append(kept, "cert-manager (HelmChart kube-system/kuben-cert-manager)")
	}
	type removable struct {
		name  string
		value map[string]any
	}
	remove := []removable{}
	if owns(Agent) {
		objects, err := agentObjects(m.getenv("KUBEN_AGENT_IMAGE"), m.version)
		if err != nil {
			objects = nil
		}
		for _, o := range objects {
			remove = append(remove, removable{Agent, o})
		}
	}
	for _, r := range []removable{
		// The namespace takes the console's route and Service with it.
		{Console, consoleObjects("", "", 0)[0]},
		{KubenConfig, kubenConfig(Wanted{}, false)},
		{ClusterIssuer, clusterIssuer("", false)},
		{TraefikConfig, traefikConfig()},
		{Namespace, namespaceObject()},
	} {
		if owns(r.name) && (!keepApps || !slices.Contains(serving, r.name)) {
			remove = append(remove, r)
		}
	}
	if len(remove) == 0 {
		return kept
	}
	st := m.ui.Step("Removing the cluster objects kuben setup created")
	defer st.Close()
	err := func() error {
		c, err := m.connect(kubeconfig)
		if err != nil {
			return err
		}
		for _, r := range remove {
			obj := object(r.value)
			if err := c.remove(ctx, obj.GetAPIVersion(), obj.GetKind(), obj.GetNamespace(), obj.GetName()); err != nil {
				return err
			}
		}
		return nil
	}()
	if err != nil {
		st.Warn(err.Error())
		return kept
	}
	names := []string{}
	for _, r := range remove {
		names = append(names, r.name)
	}
	st.Done(strings.Join(slices.Compact(names), ", "))
	return kept
}

// retainedInventory is what stays in the cluster for the apps after a
// retaining uninstall (M4.11), one line per kind; empty when nothing is
// left.
func (m *machine) retainedInventory(ctx context.Context, kubeconfig string) ([]string, error) {
	c, err := m.connect(kubeconfig)
	if err != nil {
		return nil, err
	}
	names := func(items []string) string {
		if len(items) > 10 {
			return fmt.Sprintf("%s and %d more", strings.Join(items[:10], ", "), len(items)-10)
		}
		return strings.Join(items, ", ")
	}
	qualified := func(items []unstructured.Unstructured) []string {
		out := make([]string, 0, len(items))
		for _, i := range items {
			out = append(out, i.GetNamespace()+"/"+i.GetName())
		}
		return out
	}
	out := []string{}
	namespaces, err := c.list(ctx, "v1", "Namespace", "", v1alpha1.ManagedSelector)
	if err != nil {
		return nil, err
	}
	if len(namespaces) > 0 {
		ns := []string{}
		for _, n := range namespaces {
			ns = append(ns, n.GetName())
		}
		out = append(out, "app namespaces: "+names(ns))
	}
	group := v1alpha1.SchemeGroupVersion.String()
	apps, err := c.list(ctx, group, v1alpha1.AppKind, "", "")
	if err != nil {
		return nil, err
	}
	runtimes, err := c.list(ctx, group, v1alpha1.ApplicationRuntimeKind, "", "")
	if err != nil {
		return nil, err
	}
	if stillKuben := append(qualified(apps), qualified(runtimes)...); len(stillKuben) > 0 {
		out = append(out, "apps not detached (they keep running as they are, and nobody updates them; a new Kuben takes them "+
			"over again): "+names(stillKuben))
	}
	volumes, err := c.list(ctx, "v1", "PersistentVolumeClaim", "", v1alpha1.ManagedSelector)
	if err != nil {
		return nil, err
	}
	if len(volumes) > 0 {
		out = append(out, "volumes: "+names(qualified(volumes)))
	}
	return out, nil
}
