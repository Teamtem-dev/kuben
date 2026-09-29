package materializer_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/keyring"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/materializer"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/kube/registry"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/packages/api/v1alpha1"
)

// orgLogin is the organization's login for ghcr.io of m's organization,
// its password sealed with ring.
func orgLogin(t *testing.T, m *store.Materialization, ring *keyring.Keyring, password string) store.OrgRegistry {
	t.Helper()
	id := ids.New[ids.OrgRegistry]()
	sealed, err := ring.Seal(keyring.OrgRegistryIdentity(m.Org, id), []byte(password))
	if err != nil {
		t.Fatal(err)
	}
	return store.OrgRegistry{
		ID: id, Org: m.Org, Name: "github", Preset: store.PresetGHCR, Server: "ghcr.io",
		Username: "bot", Password: sealed,
	}
}

// pullConfig is the username and password a pull Secret holds for server.
func pullConfig(t *testing.T, s *corev1.Secret, server string) (string, string) {
	t.Helper()
	var config struct {
		Auths map[string]struct {
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(s.Data[corev1.DockerConfigJsonKey], &config); err != nil {
		t.Fatal(err)
	}
	auth := config.Auths[server]
	return auth.Username, auth.Password
}

func TestRunsWithoutTheirOwnLoginPullWithTheOrganizations(t *testing.T) {
	ring := keyringOf(3)
	m := withConfig(t, `{"runtime": { "processes": { "web": { "port": 8080 } } }}`)
	m.Secrets = []store.SecretBinding{{Name: "quay", Secret: uuid.Must(uuid.NewV7()), Revision: 2, Registry: opt.Some("quay.io")}}
	m.OrgRegistry = opt.Some(orgLogin(t, &m, ring, "token"))
	app := mustRender(t, m).App
	if diff := cmp.Diff([]string{"quay.r2", "org-registry.github"}, app.Spec.ImagePullSecrets); diff != "" {
		t.Fatalf("bound logins first: %s", diff)
	}
}

func TestAnOrganizationLoginBecomesAPullSecret(t *testing.T) {
	ring := keyringOf(3)
	m := secretSample(t)
	r := orgLogin(t, &m, ring, "token")
	s, code := materializer.OrgRegistryObject(&m, &r, ring)
	if code != "" {
		t.Fatal(code)
	}
	if s.Name != "org-registry.github" || s.Namespace != "kb-shop-prod" || s.Type != corev1.SecretTypeDockerConfigJson {
		t.Fatalf("%+v %s", s.ObjectMeta, s.Type)
	}
	if user, password := pullConfig(t, &s, "ghcr.io"); user != "bot" || password != "token" {
		t.Fatalf("%s %s", user, password)
	}
	if !materializer.IsOrgRegistryObject(&s, &m, &r) || s.Labels[keyring.SecretID] != "" {
		t.Fatalf("labels %v: an organization login is no secret revision", s.Labels)
	}
	other := keyringOf(4)
	if _, code := materializer.OrgRegistryObject(&m, &r, other); code != "SecretUnreadable" {
		t.Fatalf("another keyring: %q", code)
	}
}

func TestOrganizationLoginsAreWrittenAndRotatedInPlace(t *testing.T) {
	ring := keyringOf(3)
	m := secretSample(t)
	r := orgLogin(t, &m, ring, "token")
	m.OrgRegistry = opt.Some(r)
	client := fake.NewClientset()
	w := materializer.New(materializer.Deps{Cluster: registry.Cluster{Typed: client}})
	ctx := t.Context()
	if st := materializer.WriteOrgRegistry(ctx, w, &m, ring); st != "" {
		t.Fatal(st)
	}
	secrets := client.CoreV1().Secrets(m.Namespace)
	written, err := secrets.Get(ctx, "org-registry.github", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, password := pullConfig(t, written, "ghcr.io"); password != "token" {
		t.Fatal(password)
	}
	if st := materializer.WriteOrgRegistry(ctx, w, &m, ring); st != "" {
		t.Fatalf("unchanged: %s", st)
	}

	rotated := orgLogin(t, &m, ring, "new-token")
	rotated.ID, rotated.Password = r.ID, sealFor(t, ring, &m, r.ID, "new-token")
	m.OrgRegistry = opt.Some(rotated)
	if st := materializer.WriteOrgRegistry(ctx, w, &m, ring); st != "" {
		t.Fatal(st)
	}
	written, err = secrets.Get(ctx, "org-registry.github", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, password := pullConfig(t, written, "ghcr.io"); password != "new-token" {
		t.Fatalf("rotated in place: %s", password)
	}
}

func sealFor(t *testing.T, ring *keyring.Keyring, m *store.Materialization, id ids.OrgRegistryID, password string) store.SealedBytes {
	t.Helper()
	sealed, err := ring.Seal(keyring.OrgRegistryIdentity(m.Org, id), []byte(password))
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

func TestAnotherSecretOfTheNameIsNeverTaken(t *testing.T) {
	ring := keyringOf(3)
	m := secretSample(t)
	m.OrgRegistry = opt.Some(orgLogin(t, &m, ring, "token"))
	client := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "org-registry.github", Namespace: m.Namespace},
		Data:       map[string][]byte{"mine": []byte("x")},
	})
	w := materializer.New(materializer.Deps{Cluster: registry.Cluster{Typed: client}})
	if st := materializer.WriteOrgRegistry(t.Context(), w, &m, ring); !strings.Contains(st, "NameTaken") {
		t.Fatalf("got %q", st)
	}
}

func TestDriftIgnoresWhichOrganizationLoginsTheLiveAppPullsWith(t *testing.T) {
	desired := &v1alpha1.App{Spec: v1alpha1.AppSpec{ImagePullSecrets: []string{"quay.r2", "org-registry.new"}}}
	live := &v1alpha1.App{Spec: v1alpha1.AppSpec{ImagePullSecrets: []string{"quay.r1", "org-registry.old"}}}
	materializer.KeepOrgPull(desired, opt.Some(live))
	if diff := cmp.Diff([]string{"quay.r2", "org-registry.old"}, desired.Spec.ImagePullSecrets); diff != "" {
		t.Fatal(diff)
	}
	none := &v1alpha1.App{Spec: v1alpha1.AppSpec{ImagePullSecrets: []string{"org-registry.new"}}}
	materializer.KeepOrgPull(none, opt.Some(&v1alpha1.App{}))
	if none.Spec.ImagePullSecrets != nil {
		t.Fatal(none.Spec.ImagePullSecrets)
	}
	deleted := &v1alpha1.App{Spec: v1alpha1.AppSpec{ImagePullSecrets: []string{"org-registry.new"}}}
	materializer.KeepOrgPull(deleted, opt.None[*v1alpha1.App]())
	if len(deleted.Spec.ImagePullSecrets) != 1 {
		t.Fatal("a deleted App is written as rendered")
	}
}
