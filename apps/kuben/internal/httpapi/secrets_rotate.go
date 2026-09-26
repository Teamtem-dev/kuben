package httpapi

// Rolling a secret out, deleting it, and its revisions (routes/secrets.rs).

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/access"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

// rotate starts a rotation run for every app of e that references secret.
// The caller must be allowed to deploy each of them; an app that cannot
// take a run now (never deployed, pinned, being deleted) is reported, not
// fatal.
func (s *Server) rotate(ctx context.Context, t *store.Tenant, a access.Access, e envScope, secret string) ([]gen.RolloutDto, error) {
	users, err := t.SecretUsers(ctx, e.env.ID, secret)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if len(users) == 0 {
		return nil, nil
	}
	if _, err := a.Require(perm.AppDeploy, e.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	_, actor := a.Actor()
	apps, err := t.Apps(ctx, e.env.ID)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	out := make([]gen.RolloutDto, 0, len(users))
	for _, u := range users {
		i := indexOfTarget(apps, u)
		if i < 0 {
			continue
		}
		if err := ensureMayDeploy(ctx, t, a, u.Target, e.chain()); err != nil {
			return nil, err
		}
		rollout, err := s.rotateApp(ctx, t, a, e, rotation{secret: secret, app: apps[i], actor: actor})
		if err != nil {
			return nil, err
		}
		out = append(out, rollout)
	}
	return out, nil
}

func indexOfTarget(apps []store.AppRecord, u store.SecretUser) int {
	for i, app := range apps {
		if app.Target == u.Target {
			return i
		}
	}
	return -1
}

// rotation is one app's rotation of secret.
type rotation struct {
	secret string
	app    store.AppRecord
	actor  string
}

// skippedRollout is an app that takes no rotation run, and why.
func skippedRollout(app, why string) gen.RolloutDto {
	return gen.RolloutDto{
		App:               app,
		Run:               gen.OptNilUUID{Null: true, Set: true},
		ApprovalsRequired: 0,
		Skipped:           gen.NewOptNilString(why),
	}
}

// rotateApp starts r's run, or says why there is none.
func (s *Server) rotateApp(ctx context.Context, t *store.Tenant, a access.Access, e envScope, r rotation) (gen.RolloutDto, error) {
	slug := r.app.Slug
	release, hasRelease := r.app.Release.Get()
	revision, hasRevision := r.app.ConfigRevision.Get()
	if !hasRelease || !hasRevision {
		return skippedRollout(slug, "the app has no release yet"), nil
	}
	expected := r.app.DesiredGeneration
	hash := sha256.Sum256(fmt.Appendf(nil, "rotation/%s/%s/%s/%d", r.secret, release, revision, uint64(expected)))
	audit := requestAudit(a, "deployment.accepted", "app", e.project.project.Slug+"/"+e.env.Slug+"/"+slug)
	started, err := t.StartDeployment(ctx, store.StartDeployment{
		Project: e.project.project.ID, Target: r.app.Target, Release: release, ConfigRevision: revision,
		ExpectedGeneration: expected, LifecycleUID: r.app.LifecycleUID, Reason: store.ReasonRotation,
		RequestedBy: r.actor, InputHash: hash[:],
	}, audit, opt.None[store.IdempotencyKey]())
	if err != nil {
		return gen.RolloutDto{}, err //nolint:wrapcheck // a store error, answered as internal
	}
	switch st := started.(type) {
	case store.StartedAccepted:
		return gen.RolloutDto{
			App:               slug,
			Run:               gen.NewOptNilUUID(st.Run.UUID()),
			ApprovalsRequired: int32(st.ApprovalsRequired),
			Skipped:           optNilString(opt.None[string]()),
		}, nil
	case store.StartedRejected:
		return skippedRollout(slug, st.Reject.Error()), nil
	case store.StartedSecretRevoked:
		return skippedRollout(slug, "another secret it references is revoked"), nil
	case store.StartedVulnerabilityBlocked:
		return skippedRollout(slug, "the vulnerability gate refuses its release"), nil
	case store.StartedFrozen:
		return skippedRollout(slug, "the environment is frozen"), nil
	case store.StartedUntrusted:
		return skippedRollout(slug, "an untrusted preview binds no secrets"), nil
	case store.StartedNotFound, store.StartedReplayed, store.StartedKeyReused:
		return skippedRollout(slug, "the app changed meanwhile"), nil
	case nil:
	}
	return gen.RolloutDto{}, kerrors.New(kerrors.Internal, "an unknown start result")
}

// secretInUse refuses to delete secret while apps reference it.
func secretInUse(secret string, apps []string) error {
	quoted := make([]string, 0, len(apps))
	for _, app := range apps {
		quoted = append(quoted, "`"+app+"`")
	}
	return kerrors.New(kerrors.Conflict, "secret `%s` is referenced by %s", secret, strings.Join(quoted, ", "))
}

// DeleteSecret deletes a secret. Refused while an app of the environment
// references it.
func (s *Server) DeleteSecret(ctx context.Context, params gen.DeleteSecretParams) (gen.DeleteSecretRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	e, err := s.findEnvironment(ctx, a, params.Project, params.Environment)
	if err != nil {
		return nil, err
	}
	if _, err := a.Require(perm.SecretWrite, e.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	secret := params.Secret
	users, deleted, err := s.deleteStoredSecret(ctx, a, e, secret)
	if err != nil {
		return nil, err
	}
	if deleted {
		return &gen.DeleteSecretNoContent{}, nil
	}
	cluster, err := s.cluster()
	if err != nil {
		return nil, err
	}
	api := cluster.Typed.CoreV1().Secrets(e.namespace())
	list, err := api.List(ctx, metav1.ListOptions{LabelSelector: legacySelector()})
	if err != nil {
		return nil, kubeError(err, secret)
	}
	found := false
	for i := range list.Items {
		found = found || list.Items[i].Name == secret
	}
	if !found {
		return nil, kerrors.New(kerrors.NotFound, "secret `%s`", secret)
	}
	if len(users) > 0 {
		return nil, secretInUse(secret, users)
	}
	if err := api.Delete(ctx, secret, metav1.DeleteOptions{}); err != nil {
		return nil, kubeError(err, secret)
	}
	return &gen.DeleteSecretNoContent{}, nil
}

// deleteStoredSecret deletes the encrypted secret; when there is none, the
// apps that reference the name (a cluster Secret may still be theirs).
func (s *Server) deleteStoredSecret(ctx context.Context, a access.Access, e envScope, secret string) ([]string, bool, error) {
	t, err := s.deps.Store.Tenant(ctx, e.project.org)
	if err != nil {
		return nil, false, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	audit := requestAudit(a, "secret.deleted", "secret", secretReference(e, secret))
	deleted, err := t.DeleteSecret(ctx, e.env.ID, secret, audit)
	if err != nil {
		return nil, false, err //nolint:wrapcheck // a store error, answered as internal
	}
	switch d := deleted.(type) {
	case store.SecretDeletedDone:
		return nil, true, t.Commit(ctx) //nolint:wrapcheck // a store error, answered as internal
	case store.SecretDeletedInUse:
		return nil, false, secretInUse(secret, d.Apps)
	case store.SecretDeletedNotFound, nil:
	}
	users, err := t.SecretUsers(ctx, e.env.ID, secret)
	if err != nil {
		return nil, false, err //nolint:wrapcheck // a store error, answered as internal
	}
	slugs := make([]string, 0, len(users))
	for _, u := range users {
		slugs = append(slugs, u.Slug)
	}
	return slugs, false, nil
}

// ListSecretRevisions lists the revisions of an encrypted secret, newest
// first (no values).
func (s *Server) ListSecretRevisions(ctx context.Context, params gen.ListSecretRevisionsParams) (gen.ListSecretRevisionsRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	e, err := s.findEnvironment(ctx, a, params.Project, params.Environment)
	if err != nil {
		return nil, err
	}
	if _, err := a.Require(perm.SecretRead, e.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	t, err := s.deps.Store.Tenant(ctx, e.project.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	revisions, found, err := t.SecretRevisions(ctx, e.env.ID, params.Secret)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found {
		return nil, kerrors.New(kerrors.NotFound, "secret `%s`", params.Secret)
	}
	var current uint64
	if len(revisions) > 0 {
		current = revisions[0].Revision
	}
	out := make(gen.ListSecretRevisionsOKApplicationJSON, 0, len(revisions))
	for _, r := range revisions {
		revokedAt := opt.None[string]()
		if at, ok := r.RevokedAt.Get(); ok {
			revokedAt = opt.Some(Timestamp(at))
		}
		out = append(out, gen.SecretRevisionDto{
			Revision:  revisionNumber(r.Revision),
			Keys:      nonNil(r.Keys),
			Current:   r.Revision == current,
			CreatedBy: r.CreatedBy,
			CreatedAt: Timestamp(r.CreatedAt),
			RevokedAt: optNilString(revokedAt),
			RevokedBy: optNilString(r.RevokedBy),
		})
	}
	return &out, nil
}

// RevokeSecretRevision revokes a revision for good: runs bound to it fail
// instead of delivering it. Revoking the current revision blocks
// deployments until a new value is set.
func (s *Server) RevokeSecretRevision(ctx context.Context, params gen.RevokeSecretRevisionParams) (gen.RevokeSecretRevisionRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	e, err := s.findEnvironment(ctx, a, params.Project, params.Environment)
	if err != nil {
		return nil, err
	}
	if _, err := a.Require(perm.SecretWrite, e.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	if params.Revision < 0 {
		return nil, kerrors.New(kerrors.Validation, "revision must be a non-negative number")
	}
	revision, secret := uint64(params.Revision), params.Secret
	_, actor := a.Actor()
	audit := requestAudit(a, "secret.revision.revoked", "secret", secretReference(e, secret))
	audit.Data = opt.Some[any](map[string]any{"revision": revision})
	t, err := s.deps.Store.Tenant(ctx, e.project.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // a no-op after the commit
	revoked, err := t.RevokeSecretRevision(ctx, e.env.ID, secret, revision, actor, audit)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	switch revoked {
	case store.RevokedDone:
		if err := t.Commit(ctx); err != nil {
			return nil, err //nolint:wrapcheck // a store error, answered as internal
		}
		return &gen.RevokeSecretRevisionNoContent{}, nil
	case store.RevokedAlready:
		return nil, kerrors.New(kerrors.Conflict, "revision %d of `%s` is revoked already", revision, secret)
	case store.RevokedNotFound:
	}
	return nil, kerrors.New(kerrors.NotFound, "revision %d of secret `%s`", revision, secret)
}
