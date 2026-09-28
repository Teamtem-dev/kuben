package materializer

// Organization registry logins in the cluster (2.1).
//
// A run whose environment has no login of its own for the registry of its
// image pulls with the organization's login for that registry, when there
// is one (store.Materialization.OrgRegistry). Like an environment's login it
// becomes a `kubernetes.io/dockerconfigjson` Secret in the run's namespace,
// written before the App, its password opened with the keyring only then;
// the App lists it in imagePullSecrets after the bound logins.
//
// An organization login has no revisions: its Secret is named after the
// login (`org-registry.<name>`, a name no revision object can have) and is
// rewritten in place when the login is rotated, so every pod pulls with the
// current password, and a rotation never changes a rendered App. Secrets
// of logins the organization deleted are removed once a run of the
// environment succeeds; the Apps delivered before keep naming them until
// their next run, and the kubelet pulls without them meanwhile.

import (
	"bytes"
	"context"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/clock"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/keyring"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
	"github.com/Teamtem-dev/kuben/packages/api/v1alpha1"
)

const (
	// OrgRegistryLabel names the organization login a pull Secret holds.
	OrgRegistryLabel = "kuben.dev/org-registry"
	// orgRegistryPrefix starts the name of an organization login's Secret.
	orgRegistryPrefix = "org-registry."
)

// OrgRegistrySecretName is the name of the pull Secret of the
// organization login called name.
func OrgRegistrySecretName(name string) string { return orgRegistryPrefix + name }

// OrgRegistryObject is the pull Secret of the organization login r in m's
// namespace; the refusal code when its password does not open
// (SecretUnreadable).
func OrgRegistryObject(m *store.Materialization, r *store.OrgRegistry, ring *keyring.Keyring) (corev1.Secret, string) {
	login, err := ring.OpenOrgRegistry(*r)
	if err != nil {
		return corev1.Secret{}, "SecretUnreadable"
	}
	return corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      OrgRegistrySecretName(r.Name),
			Namespace: m.Namespace,
			Labels: map[string]string{
				v1alpha1.LabelManagedBy:   v1alpha1.LabelManagerValue,
				v1alpha1.LabelOrg:         m.Org.String(),
				v1alpha1.LabelProject:     m.ProjectSlug,
				v1alpha1.LabelEnvironment: EnvironmentName(m.ProjectSlug, m.EnvironmentSlug),
				OrgRegistryLabel:          r.ID.String(),
			},
		},
		Type: corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(login.DockerConfig(r.Server))},
	}, ""
}

// IsOrgRegistryObject reports whether live is the pull Secret Kuben wrote
// for the organization login r.
func IsOrgRegistryObject(live *corev1.Secret, m *store.Materialization, r *store.OrgRegistry) bool {
	return BelongsTo(live.Labels, m.Org) && live.Labels[OrgRegistryLabel] == r.ID.String()
}

// writeOrgRegistry writes, or brings up to date, the pull Secret of the
// organization login m's run pulls with.
func (w *Worker) writeOrgRegistry(ctx context.Context, m *store.Materialization, ring *keyring.Keyring) stop {
	r, ok := m.OrgRegistry.Get()
	if !ok {
		return nil
	}
	desired, code := OrgRegistryObject(m, &r, ring)
	if code != "" {
		w.d.Logger.Error("an organization registry login does not open", "run", m.Run.String(), "registry", r.Name)
		return refused(code)
	}
	api := w.d.Cluster.Typed.CoreV1().Secrets(m.Namespace)
	live, err := api.Get(ctx, desired.Name, metav1.GetOptions{})
	switch {
	case err == nil && !IsOrgRegistryObject(live, m, &r):
		return refused("NameTaken")
	case err == nil:
		if live.Type == desired.Type && bytes.Equal(live.Data[corev1.DockerConfigJsonKey], desired.Data[corev1.DockerConfigJsonKey]) {
			return nil
		}
		if live.Type != desired.Type {
			// The type of a Secret never changes: write it anew.
			if err := api.Delete(ctx, live.Name, metav1.DeleteOptions{}); err != nil && !isNotFound(err) {
				return retryKube(err)
			}
			return stopRetry{err: contended(desired.Name)}
		}
		updated := live.DeepCopy()
		updated.Labels = desired.Labels
		updated.Data = desired.Data
		switch _, err := api.Update(ctx, updated, metav1.UpdateOptions{FieldManager: FieldManager}); {
		case err == nil:
			w.d.Logger.Info("organization registry login rotated", "run", m.Run.String(), "secret", desired.Name)
			return nil
		case isConflict(err):
			return stopRetry{err: contended(desired.Name)}
		default:
			return retryKube(err)
		}
	case !isNotFound(err):
		return retryKube(err)
	}
	switch _, err := api.Create(ctx, &desired, metav1.CreateOptions{FieldManager: FieldManager}); {
	case err == nil:
		w.d.Logger.Info("organization registry login written", "run", m.Run.String(), "secret", desired.Name)
		return nil
	case isConflict(err):
		return stopRetry{err: contended(desired.Name)}
	default:
		return retryKube(err)
	}
}

// collectOrgRegistries removes the pull Secrets in m's namespace of
// organization logins that were deleted (or renamed); the number removed.
func (w *Worker) collectOrgRegistries(ctx context.Context, m *store.Materialization) (int, *Error) {
	api := w.d.Cluster.Typed.CoreV1().Secrets(m.Namespace)
	selector := v1alpha1.ManagedSelector + "," + v1alpha1.LabelOrg + "=" + m.Org.String() + "," + OrgRegistryLabel
	listed, err := api.List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return 0, kubeError(err)
	}
	if len(listed.Items) == 0 {
		return 0, nil
	}
	current, err := w.orgRegistries(ctx, m.Org)
	if err != nil {
		return 0, storeError(err)
	}
	cutoff := clock.SaturatingSub(w.d.Clock.NowMs(), keepUnused.Milliseconds())
	removed := 0
	for i := range listed.Items {
		s := &listed.Items[i]
		young := s.CreationTimestamp.IsZero() || s.CreationTimestamp.UnixMilli() > cutoff
		if young || slices.Contains(current, s.Name+"/"+s.Labels[OrgRegistryLabel]) {
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
		w.d.Logger.Info("pull secrets of deleted registry logins removed", "namespace", m.Namespace, "removed", removed)
	}
	return removed, nil
}

// orgRegistries is `<secret name>/<id>` of every login of org.
func (w *Worker) orgRegistries(ctx context.Context, org ids.OrgID) ([]string, error) {
	t, err := w.d.Store.Tenant(ctx, org)
	if err != nil {
		return nil, err //nolint:wrapcheck // wrapped by storeError
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	logins, err := t.OrgRegistries(ctx)
	if err != nil {
		return nil, err //nolint:wrapcheck // wrapped by storeError
	}
	out := make([]string, 0, len(logins))
	for _, r := range logins {
		out = append(out, OrgRegistrySecretName(r.Name)+"/"+r.ID.String())
	}
	return out, nil
}

// KeepOrgPull makes desired pull with the organization logins live pulls
// with: which organization login a run uses is read when the run is, so a
// login added or deleted after the run was delivered is not a change
// someone else made to its App. Without a live object desired is kept.
func KeepOrgPull(desired *v1alpha1.App, live opt.Val[*v1alpha1.App]) {
	app, ok := live.Get()
	if !ok || app == nil {
		return
	}
	isOrg := func(name string) bool { return strings.HasPrefix(name, orgRegistryPrefix) }
	pull := slices.DeleteFunc(slices.Clone(desired.Spec.ImagePullSecrets), isOrg)
	for _, name := range app.Spec.ImagePullSecrets {
		if isOrg(name) {
			pull = append(pull, name)
		}
	}
	if len(pull) == 0 {
		pull = nil
	}
	desired.Spec.ImagePullSecrets = pull
}
