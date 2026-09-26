package store

// Release history (scenario 5): every spec change of an App is recorded as a
// numbered revision so it can be listed, compared and rolled back; the port
// of repo/releases.rs.

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/model"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
)

// NewRelease is the input for a release record. Spec is the App spec, which
// only ever holds Secret references, never values; it is stored as
// serde_json wrote it.
type NewRelease struct {
	OrgID     opt.Val[ids.OrgID]
	Namespace string
	App       string
	Image     opt.Val[string]
	Spec      any
	Reason    string
	ActorID   opt.Val[string]
	Note      opt.Val[string]
}

const (
	nextAppRevision  = "SELECT COALESCE(MAX(revision), 0) FROM app_releases WHERE namespace = $1 AND app = $2"
	insertAppRelease = "INSERT INTO app_releases " +
		"(id, org_id, namespace, app, revision, image, spec, reason, actor_id, note, created_at) " +
		"VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)"
	selectAppReleases = "SELECT id, revision, namespace, app, image, spec, reason, actor_id, note, created_at " +
		"FROM app_releases WHERE namespace = $1 AND app = $2 ORDER BY revision DESC LIMIT $3"
	selectAppRelease = "SELECT id, revision, namespace, app, image, spec, reason, actor_id, note, created_at " +
		"FROM app_releases WHERE namespace = $1 AND app = $2 AND revision = $3"
)

// maxReleaseAttempts: concurrent writers (Postgres HA) may race for the same
// revision; the unique index makes the loser retry with the next number.
const maxReleaseAttempts = 3

// RecordRelease records r as the next revision of its app.
func (s *Store) RecordRelease(ctx context.Context, r NewRelease) (model.AppRelease, error) {
	const op = "record a release"
	spec, err := canonical(op, r.Spec)
	if err != nil {
		return model.AppRelease{}, err
	}
	orgID := opt.None[string]()
	if org, ok := r.OrgID.Get(); ok {
		orgID = opt.Some(org.String())
	}
	for attempt := 1; ; attempt++ {
		var maxRevision int64
		if err := queryOne(ctx, s.db, op, nextAppRevision, []any{&maxRevision}, r.Namespace, r.App); err != nil {
			return model.AppRelease{}, err
		}
		release := model.AppRelease{
			ID:        uuid.Must(uuid.NewV7()).String(),
			Revision:  maxRevision + 1,
			Namespace: r.Namespace,
			App:       r.App,
			Image:     r.Image,
			Spec:      r.Spec,
			Reason:    r.Reason,
			ActorID:   r.ActorID,
			Note:      r.Note,
			CreatedAt: s.now(),
		}
		_, err := exec(ctx, s.db, op, insertAppRelease,
			release.ID, orgID.Ptr(), release.Namespace, release.App, release.Revision, release.Image.Ptr(), spec,
			release.Reason, release.ActorID.Ptr(), release.Note.Ptr(), release.CreatedAt)
		switch {
		case err == nil:
			return release, nil
		case IsUniqueViolation(err) && attempt < maxReleaseAttempts:
		default:
			return model.AppRelease{}, err
		}
	}
}

func scanAppRelease(row pgx.CollectableRow) (model.AppRelease, error) {
	var r model.AppRelease
	var image, actor, note *string
	var spec string
	if err := row.Scan(&r.ID, &r.Revision, &r.Namespace, &r.App, &image, &spec, &r.Reason, &actor, &note,
		&r.CreatedAt); err != nil {
		return model.AppRelease{}, err
	}
	v, err := jsonValue("read a release", spec)
	if err != nil {
		return model.AppRelease{}, err
	}
	r.Spec = v
	r.Image = opt.FromPtr(image)
	r.ActorID = opt.FromPtr(actor)
	r.Note = opt.FromPtr(note)
	return r, nil
}

// ListReleases is the newest limit releases of app in namespace, newest
// first.
func (s *Store) ListReleases(ctx context.Context, namespace, app string, limit int64) ([]model.AppRelease, error) {
	return queryAll(ctx, s.db, "list releases", selectAppReleases, scanAppRelease, namespace, app, limit)
}

// FindRelease is revision revision of app in namespace.
func (s *Store) FindRelease(ctx context.Context, namespace, app string, revision int64) (model.AppRelease, bool, error) {
	return queryOpt(ctx, s.db, "find a release", selectAppRelease, scanAppRelease, namespace, app, revision)
}
