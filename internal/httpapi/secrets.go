package api

// Secrets of an environment (routes/secrets.rs, M4.4, ADR-030). Values are
// write-only: they can be set and replaced, never read back.
//
// Every change is a new encrypted, immutable revision in SQL. Setting a
// value rolls it out, by default, with a `rotation` run of every app of the
// environment that references the secret, under the environment's policy
// (a production rotation waits for its approval). A revision can be revoked
// for good; runs bound to it fail instead of delivering it. Secrets Kuben
// wrote into the cluster before M4.4 are still listed and can be deleted.

import (
	"context"
	"maps"
	"math"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Teamtem-dev/kuben/api/v1alpha1"
	kerr "github.com/Teamtem-dev/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/internal/httpapi/access"
	"github.com/Teamtem-dev/kuben/internal/httpapi/gen"
	secrets "github.com/Teamtem-dev/kuben/internal/keyring"
	"github.com/Teamtem-dev/kuben/internal/store"
)

const (
	// maxSecretBytes bounds the sum of all values of one secret.
	maxSecretBytes = 256 * 1024
	// maxSealedBytes bounds the sealed JSON object of one revision.
	maxSealedBytes = 384 * 1024
	// maxSecretKeys is the most keys one secret holds.
	maxSecretKeys = 256
)

// revisionNumber is a revision as the contract's int64 (revisions count
// from 1 and never reach 2^63).
func revisionNumber(n uint64) int64 {
	if n > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(n)
}

// secretDto is SecretDto::from(SecretSummary).
func secretDto(s store.SecretSummary) gen.SecretDto {
	return gen.SecretDto{
		Name:      s.Name,
		Keys:      nonNil(s.Keys),
		CreatedAt: gen.NewOptNilString(Timestamp(s.CreatedAt)),
		Storage:   gen.SecretStorageEncrypted,
		Revision:  gen.NewOptNilInt64(revisionNumber(s.CurrentRevision)),
		Revoked:   s.Revoked,
		UpdatedAt: gen.NewOptNilString(Timestamp(s.UpdatedAt)),
	}
}

// legacySecretDto is SecretDto::from(&Secret): a Secret Kuben wrote into
// the cluster before encrypted revisions.
func legacySecretDto(s *corev1.Secret) gen.SecretDto {
	created := opt.None[string]()
	if !s.CreationTimestamp.IsZero() {
		created = opt.Some(Timestamp(s.CreationTimestamp.UnixMilli()))
	}
	return gen.SecretDto{
		Name:      s.Name,
		Keys:      slices.Sorted(maps.Keys(s.Data)),
		CreatedAt: optNilString(created),
		Storage:   gen.SecretStorageCluster,
		Revision:  optNilInt64(opt.None[int64]()),
		Revoked:   false,
		UpdatedAt: optNilString(opt.None[string]()),
	}
}

// legacySelector selects the Secrets Kuben wrote into the cluster before
// M4.4 (not revision objects).
func legacySelector() string {
	return v1alpha1.ManagedSelector + ",!" + secrets.SecretID
}

// checkValues checks the keys and sizes of data.
func checkValues(data map[string]string) error {
	if len(data) == 0 {
		return kerr.New(kerr.Validation, "a secret needs at least one key")
	}
	if len(data) > maxSecretKeys {
		return kerr.New(kerr.Validation, "a secret holds at most %d keys", maxSecretKeys)
	}
	total := 0
	for _, k := range slices.Sorted(maps.Keys(data)) {
		if err := SecretKey(k); err != nil {
			return err
		}
		total += len(data[k])
	}
	sealed := maxSealedBytes + 1
	if text, err := secrets.ValuesJSON(data); err == nil {
		sealed = len(text)
	}
	if total > maxSecretBytes || sealed > maxSealedBytes {
		return kerr.New(kerr.Validation, "secret values exceed %d bytes", maxSecretBytes)
	}
	return nil
}

// keyring is the secret keyring; unavailable when encryption is not
// configured.
func (s *Server) keyring() (*secrets.Keyring, error) {
	k, ok := s.deps.Keyring.Get()
	if !ok || k == nil {
		return nil, kerr.New(kerr.Unavailable, "secret encryption is not configured")
	}
	return k, nil
}

// secretReference is `project/environment/secret`, for audit records.
func secretReference(e envScope, name string) string {
	return e.project.project.Slug + "/" + e.env.Slug + "/" + name
}

// errUntrusted refuses secrets to an untrusted preview (apps/mod.rs
// untrusted).
func errUntrusted() error {
	return kerr.New(kerr.Conflict, "this preview comes from a fork: it cannot use secrets or registry logins")
}

// ListSecrets lists the secrets of an environment (names and keys).
func (s *Server) ListSecrets(ctx context.Context, params gen.ListSecretsParams) ([]gen.SecretDto, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	e, err := s.findEnvironment(ctx, a, params.Project, params.Environment)
	if err != nil {
		return nil, err
	}
	if _, err := a.Require(perm.SecretRead, e.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerr already
	}
	stored, err := s.storedSecrets(ctx, e)
	if err != nil {
		return nil, err
	}
	out := make([]gen.SecretDto, 0, len(stored))
	for _, sec := range stored {
		if _, opaque := sec.Kind.(store.SecretOpaque); opaque {
			out = append(out, secretDto(sec))
		}
	}
	cluster, err := s.cluster()
	if err != nil {
		return out, nil //nolint:nilerr // without a cluster only the store's secrets are listed
	}
	list, err := cluster.Typed.CoreV1().Secrets(e.namespace()).List(ctx, metav1.ListOptions{LabelSelector: legacySelector()})
	if err != nil {
		s.deps.Logger.Warn("cluster secrets are not listed", "error", err.Error())
		return out, nil
	}
	for i := range list.Items {
		item := &list.Items[i]
		if !slices.ContainsFunc(out, func(o gen.SecretDto) bool { return o.Name == item.Name }) {
			out = append(out, legacySecretDto(item))
		}
	}
	slices.SortStableFunc(out, func(x, y gen.SecretDto) int { return strings.Compare(x.Name, y.Name) })
	return out, nil
}

func (s *Server) storedSecrets(ctx context.Context, e envScope) ([]store.SecretSummary, error) {
	t, err := s.deps.Store.Tenant(ctx, e.project.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx)           //nolint:errcheck // read only
	return t.Secrets(ctx, e.env.ID) //nolint:wrapcheck // a store error, answered as internal
}

// PutSecret sets a secret's values: a new revision, rolled out unless
// `rollout` is false.
func (s *Server) PutSecret(ctx context.Context, req *gen.PutSecret, params gen.PutSecretParams) (gen.PutSecretRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	e, err := s.findEnvironment(ctx, a, params.Project, params.Environment)
	if err != nil {
		return nil, err
	}
	if _, err := a.Require(perm.SecretWrite, e.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerr already
	}
	if err := DNSLabel("secret name", params.Secret, 63); err != nil {
		return nil, err
	}
	values := map[string]string(req.Data)
	if err := checkValues(values); err != nil {
		return nil, err
	}
	t, err := s.deps.Store.Tenant(ctx, e.project.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	if err := s.storeRevision(ctx, t, a, e, newRevision{name: params.Secret, kind: store.SecretOpaque{}, values: values}); err != nil {
		return nil, err
	}
	var rollouts []gen.RolloutDto
	if req.Rollout.Or(true) {
		if rollouts, err = s.rotate(ctx, t, a, e, params.Secret); err != nil {
			return nil, err
		}
	}
	summary, found, err := t.Secret(ctx, e.env.ID, params.Secret)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found {
		return nil, kerr.New(kerr.Internal, "the new secret revision is missing")
	}
	if err := t.Commit(ctx); err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	dto := secretDto(summary)
	if len(rollouts) > 0 {
		dto.Rollouts = rollouts
	}
	return &dto, nil
}

// newRevision is a new revision of a secret.
type newRevision struct {
	name   string
	kind   store.SecretKind
	values map[string]string
}

// storeRevision seals and records a new revision in t's transaction.
func (s *Server) storeRevision(ctx context.Context, t *store.Tenant, a access.Access, e envScope, n newRevision) error {
	keyring, err := s.keyring()
	if err != nil {
		return err
	}
	deleting := kerr.New(kerr.Conflict, "environment `%s` is being deleted", e.env.Slug)
	if e.deleting() {
		return deleting
	}
	// No secret ever reaches code from outside the repository (M5.1).
	untrusted, err := t.UntrustedEnvironment(ctx, e.env.ID)
	if err != nil {
		return err //nolint:wrapcheck // a store error, answered as internal
	}
	if untrusted {
		return errUntrusted()
	}
	_, actor := a.Actor()
	reservation, err := t.ReserveSecretRevision(ctx, e.project.project.ID, e.env.ID, n.name, n.kind, actor)
	if err != nil {
		return err //nolint:wrapcheck // a store error, answered as internal
	}
	var reserved store.Reserved
	switch r := reservation.(type) {
	case store.ReservationReserved:
		reserved = r.Reserved
	case store.ReservationNoEnvironment:
		return deleting
	case store.ReservationTaken:
		return kerr.New(kerr.Conflict, "%s", r.Why)
	case nil:
		return kerr.New(kerr.Internal, "no reservation")
	}
	who := secrets.Identity{Org: e.project.org.String(), Secret: reserved.Secret.String(), Revision: reserved.Revision}
	sealed, err := keyring.SealValues(who, n.values)
	if err != nil {
		return kerr.New(kerr.Internal, "sealing a secret failed: %s", err.Error())
	}
	keys := slices.Sorted(maps.Keys(n.values))
	audit := requestAudit(a, "secret.revision.created", "secret", secretReference(e, n.name))
	return t.InsertSecretRevision(ctx, reserved, keys, sealed, actor, audit) //nolint:wrapcheck // a store error, answered as internal
}

// registryLogin is the login of registry in e, opened; none when e has
// none (or no keyring and no login).
func (s *Server) registryLogin(ctx context.Context, t *store.Tenant, e envScope, registry string) (opt.Val[secrets.RegistryLogin], error) {
	if s.deps.Keyring.IsNone() {
		_, found, err := t.RegistryLogin(ctx, e.env.ID, registry)
		if err != nil {
			return opt.None[secrets.RegistryLogin](), err //nolint:wrapcheck // a store error, answered as internal
		}
		if !found {
			return opt.None[secrets.RegistryLogin](), nil
		}
	}
	keyring, err := s.keyring()
	if err != nil {
		return opt.None[secrets.RegistryLogin](), err
	}
	return keyring.OpenRegistryLogin(ctx, t, e.project.org, e.env.ID, registry) //nolint:wrapcheck // a kerr or store error
}
