package materializer_test

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"

	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/keyring"
	"github.com/Teamtem-dev/kuben/internal/kube/materializer"
	"github.com/Teamtem-dev/kuben/internal/store"
)

// Ported from materializer/secrets.rs.

func keyringOf(b byte) *keyring.Keyring {
	var key [32]byte
	for i := range key {
		key[i] = b
	}
	return keyring.FromKeys(map[uint32][32]byte{1: key})
}

func sealedBinding(
	t *testing.T, m *store.Materialization, ring *keyring.Keyring, revision uint64, values map[string]string, registry opt.Val[string],
) store.BoundSecret {
	t.Helper()
	secret := uuid.Must(uuid.NewV7())
	sealed, err := ring.SealValues(keyring.Identity{Org: m.Org.String(), Secret: secret.String(), Revision: revision}, values)
	if err != nil {
		t.Fatal(err)
	}
	return store.BoundSecret{
		Binding: store.SecretBinding{Name: "db", Secret: secret, Revision: revision, Registry: registry},
		Keys:    []string{"url"},
		Sealed:  sealed,
	}
}

func boundSecret(t *testing.T, m *store.Materialization, keyring *keyring.Keyring, revision uint64) store.BoundSecret {
	t.Helper()
	return sealedBinding(t, m, keyring, revision, map[string]string{"url": "postgres://db"}, opt.None[string]())
}

func secretSample(t *testing.T) store.Materialization {
	t.Helper()
	m := sample(t)
	m.ProjectSlug, m.EnvironmentSlug, m.Namespace = "shop", "prod", "kb-shop-prod"
	return m
}

func TestARevisionBecomesAnImmutableLabelledSecret(t *testing.T) {
	keyring := keyringOf(3)
	m := secretSample(t)
	b := boundSecret(t, &m, keyring, 2)
	s, code := materializer.SecretObject(&m, &b, keyring)
	if code != "" {
		t.Fatal(code)
	}
	if s.Name != "db.r2" || s.Namespace != "kb-shop-prod" || s.Immutable == nil || !*s.Immutable {
		t.Fatalf("%+v", s.ObjectMeta)
	}
	if s.Type != corev1.SecretTypeOpaque || string(s.Data["url"]) != "postgres://db" {
		t.Fatalf("%s %q", s.Type, s.Data)
	}
	if !materializer.IsRevision(&s, &m, &b) {
		t.Fatal("its own revision")
	}
	if got, ok := materializer.RevisionOf(&s); !ok || got != (store.SecretRevisionKey{Secret: b.Binding.Secret, Revision: 2}) {
		t.Fatalf("%v %v", got, ok)
	}
	other := boundSecret(t, &m, keyring, 2)
	if materializer.IsRevision(&s, &m, &other) {
		t.Fatal("another secret of the same name")
	}
	foreign := secretSample(t)
	foreign.Org = ids.New[ids.Org]()
	if materializer.IsRevision(&s, &foreign, &b) {
		t.Fatal("another organization")
	}
	if s.Labels["kuben.dev/environment"] != materializer.EnvironmentName("shop", "prod") ||
		s.Labels["kuben.dev/project"] != "shop" || s.Labels["kuben.dev/secret-revision"] != "2" {
		t.Fatalf("labels %v", s.Labels)
	}
}

func TestARegistryLoginBecomesAPullSecret(t *testing.T) {
	ring := keyringOf(3)
	m := secretSample(t)
	login := keyring.RegistryLogin{Username: "bot", Password: "token"}
	b := sealedBinding(t, &m, ring, 1, login.Values(), opt.Some("ghcr.io"))
	s, code := materializer.SecretObject(&m, &b, ring)
	if code != "" {
		t.Fatal(code)
	}
	if s.Type != corev1.SecretTypeDockerConfigJson {
		t.Fatal(s.Type)
	}
	var config struct {
		Auths map[string]struct {
			Username string `json:"username"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(s.Data[".dockerconfigjson"], &config); err != nil {
		t.Fatal(err)
	}
	if config.Auths["ghcr.io"].Username != "bot" {
		t.Fatalf("%+v", config)
	}
	broken := sealedBinding(t, &m, ring, 1, map[string]string{}, opt.Some("ghcr.io"))
	if _, code := materializer.SecretObject(&m, &broken, ring); code == "" {
		t.Fatal("not a login")
	}
}

func TestARevisionSealedForAnotherRunDoesNotOpen(t *testing.T) {
	keyring := keyringOf(3)
	m := secretSample(t)
	b := boundSecret(t, &m, keyring, 2)
	b.Binding.Revision = 3
	if _, code := materializer.SecretObject(&m, &b, keyring); code != "SecretUnreadable" {
		t.Fatalf("code %q", code)
	}
	b = boundSecret(t, &m, keyring, 2)
	if _, code := materializer.SecretObject(&m, &b, keyringOf(4)); code == "" {
		t.Fatal("another installation's keyring opened it")
	}
}
