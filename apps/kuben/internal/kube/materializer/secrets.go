package materializer

// Managed secrets in the cluster (materializer/secrets.rs, M4.4, ADR-030).
//
// Before a run writes its App or envelope, every secret revision it is
// bound to is opened and written as an immutable Secret named after the
// revision (keyring.ObjectName); the rendered objects read from those. A
// rotation is therefore a new object and a new pod template, never a change
// under running pods. Once a run succeeds, the revision objects of its
// environment that no run needs any more are removed.
//
// Without a keyring a run bound to any secret fails with
// SecretsUnavailable, as the Rust worker without one did.

import (
	"context"
	"strconv"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/keyring"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/packages/api/v1alpha1"
)

// keepUnused is how long unused revision objects are kept: a run accepted
// a moment ago may be about to use one.
const keepUnused = 10 * time.Minute

// SecretObject is the immutable Secret of the bound revision b of m's run;
// the refusal code when it cannot be opened (SecretUnreadable).
func SecretObject(m *store.Materialization, b *store.BoundSecret, ring *keyring.Keyring) (corev1.Secret, string) {
	org, secret := m.Org.String(), b.Binding.Secret.String()
	who := keyring.Identity{Org: org, Secret: secret, Revision: b.Binding.Revision}
	values, err := ring.OpenValues(who, b.Sealed)
	if err != nil {
		return corev1.Secret{}, "SecretUnreadable"
	}
	typ, data := corev1.SecretTypeOpaque, make(map[string][]byte, len(values))
	if registry, ok := b.Binding.Registry.Get(); ok {
		login, ok := keyring.RegistryLoginFrom(values)
		if !ok {
			return corev1.Secret{}, "SecretUnreadable"
		}
		typ = corev1.SecretTypeDockerConfigJson
		data[corev1.DockerConfigJsonKey] = []byte(login.DockerConfig(registry))
	} else {
		for k, v := range values {
			data[k] = []byte(v)
		}
	}
	immutable := true
	return corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      keyring.ObjectName(b.Binding.Name, b.Binding.Revision),
			Namespace: m.Namespace,
			Labels: map[string]string{
				v1alpha1.LabelManagedBy:   v1alpha1.LabelManagerValue,
				v1alpha1.LabelOrg:         org,
				v1alpha1.LabelProject:     m.ProjectSlug,
				v1alpha1.LabelEnvironment: EnvironmentName(m.ProjectSlug, m.EnvironmentSlug),
				keyring.SecretID:          secret,
				keyring.SecretRevision:    strconv.FormatUint(b.Binding.Revision, 10),
			},
		},
		Immutable: &immutable,
		Type:      typ,
		Data:      data,
	}, ""
}

// IsRevision reports whether live is the revision b: Kuben wrote it for
// this organization, secret and revision.
func IsRevision(live *corev1.Secret, m *store.Materialization, b *store.BoundSecret) bool {
	l := live.Labels
	return BelongsTo(l, m.Org) &&
		l[keyring.SecretID] == b.Binding.Secret.String() &&
		l[keyring.SecretRevision] == strconv.FormatUint(b.Binding.Revision, 10)
}

// RevisionOf is the (secret, revision) a revision object carries.
func RevisionOf(s *corev1.Secret) (store.SecretRevisionKey, bool) {
	id, hasID := s.Labels[keyring.SecretID]
	revision, hasRevision := s.Labels[keyring.SecretRevision]
	if !hasID || !hasRevision {
		return store.SecretRevisionKey{}, false
	}
	secret, err := uuid.Parse(id)
	if err != nil {
		return store.SecretRevisionKey{}, false
	}
	n, err := strconv.ParseUint(revision, 10, 64)
	if err != nil {
		return store.SecretRevisionKey{}, false
	}
	return store.SecretRevisionKey{Secret: secret, Revision: n}, true
}

// writeSecrets writes the Secret of every revision m's run is bound to. A
// revoked revision fails the run.
func (w *Worker) writeSecrets(ctx context.Context, m *store.Materialization) stop {
	if len(m.Secrets) == 0 {
		return nil
	}
	ring, ok := w.d.Keyring.Get()
	if !ok || ring == nil {
		return refused("SecretsUnavailable")
	}
	bound, err := w.runSecrets(ctx, m)
	if err != nil {
		return retryStore(err)
	}
	api := w.d.Cluster.Typed.CoreV1().Secrets(m.Namespace)
	for i := range bound {
		b := &bound[i]
		if b.Revoked {
			return refused("SecretRevoked")
		}
		name := keyring.ObjectName(b.Binding.Name, b.Binding.Revision)
		live, err := api.Get(ctx, name, metav1.GetOptions{})
		switch {
		case err == nil && IsRevision(live, m, b):
			continue
		case err == nil:
			return refused("NameTaken")
		case !isNotFound(err):
			return retryKube(err)
		}
		desired, code := SecretObject(m, b, ring)
		if code != "" {
			w.d.Logger.Error("a secret revision does not open",
				"run", m.Run.String(), "secret", b.Binding.Name, "revision", b.Binding.Revision)
			return refused(code)
		}
		_, err = api.Create(ctx, &desired, metav1.CreateOptions{FieldManager: FieldManager})
		switch {
		case err == nil:
			w.d.Logger.Info("secret revision written", "run", m.Run.String(), "secret", name)
		case isConflict(err):
			// Written meanwhile: the next attempt checks what is there.
			return stopRetry{err: contended(name)}
		default:
			return retryKube(err)
		}
	}
	return nil
}

func (w *Worker) runSecrets(ctx context.Context, m *store.Materialization) ([]store.BoundSecret, error) {
	t, err := w.d.Store.Tenant(ctx, m.Org)
	if err != nil {
		return nil, err //nolint:wrapcheck // wrapped by retryStore
	}
	defer t.Rollback(ctx)           //nolint:errcheck // read only
	return t.RunSecrets(ctx, m.Run) //nolint:wrapcheck // wrapped by retryStore
}

// collectSecrets removes the revision objects of m's environment that no
// run needs any more; the number removed.
func (w *Worker) collectSecrets(ctx context.Context, m *store.Materialization) (int, *Error) {
	inUse, err := w.secretsInUse(ctx, m)
	if err != nil {
		return 0, storeError(err)
	}
	api := w.d.Cluster.Typed.CoreV1().Secrets(m.Namespace)
	selector := v1alpha1.ManagedSelector + "," + v1alpha1.LabelOrg + "=" + m.Org.String() + "," + keyring.SecretID
	listed, err := api.List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return 0, kubeError(err)
	}
	cutoff := clock.SaturatingSub(w.d.Clock.NowMs(), keepUnused.Milliseconds())
	removed := 0
	for i := range listed.Items {
		s := &listed.Items[i]
		revision, ok := RevisionOf(s)
		if !ok {
			continue
		}
		young := s.CreationTimestamp.IsZero() || s.CreationTimestamp.UnixMilli() > cutoff
		if _, used := inUse[revision]; used || young {
			continue
		}
		switch err := api.Delete(ctx, s.Name, metav1.DeleteOptions{}); {
		case err == nil:
			removed++
		case isNotFound(err):
		default:
			return removed, kubeError(err)
		}
	}
	if removed > 0 {
		w.d.Logger.Info("unused secret revisions removed", "namespace", m.Namespace, "removed", removed)
	}
	return removed, nil
}

func (w *Worker) secretsInUse(ctx context.Context, m *store.Materialization) (map[store.SecretRevisionKey]struct{}, error) {
	t, err := w.d.Store.Tenant(ctx, m.Org)
	if err != nil {
		return nil, err //nolint:wrapcheck // wrapped by storeError
	}
	defer t.Rollback(ctx)                             //nolint:errcheck // read only
	return t.SecretRevisionsInUse(ctx, m.Environment) //nolint:wrapcheck // wrapped by storeError
}
