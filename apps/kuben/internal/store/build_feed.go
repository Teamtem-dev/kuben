package store

// The builds that changed (2.1): what the event stream's `build` deltas
// are made of. Every change of a build attempt (its phase, its stages, its
// kept log) sets its updated_at, so a reader that remembers the newest
// updated_at it saw reads each change once, or again when two share a
// millisecond.

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	changedBuilds = "SELECT a.id, a.updated_at, p.namespace, pr.slug, e.slug, app.slug " +
		"FROM build_attempts a " +
		"JOIN application_targets t ON t.id = a.target_id AND t.org_id = a.org_id " +
		"JOIN applications app ON app.id = t.application_id AND app.org_id = a.org_id " +
		"JOIN environment_placements p ON p.id = t.placement_id AND p.org_id = a.org_id " +
		"JOIN environments e ON e.id = p.environment_id AND e.org_id = a.org_id " +
		"JOIN projects pr ON pr.id = a.project_id AND pr.org_id = a.org_id " +
		"WHERE a.org_id = $1 AND a.updated_at >= $2 ORDER BY a.updated_at, a.id LIMIT $3"
	attemptsByID = attemptsFrom + "a.org_id = $1 AND a.id = ANY($2::uuid[])"
)

// BuildChange is a build attempt as it is after a change, and the app it
// builds.
type BuildChange struct {
	Attempt BuildAttempt
	// UpdatedAt is when the attempt last changed (unix ms).
	UpdatedAt int64
	// Namespace and AppSlug name the app's object: `namespace/slug` is the
	// key of its projection.
	Namespace       string
	ProjectSlug     string
	EnvironmentSlug string
	AppSlug         string
}

// BuildsChangedSince is the organization's build attempts that changed at
// or after sinceMs, oldest change first, at most limit.
func (t *Tenant) BuildsChangedSince(ctx context.Context, sinceMs, limit int64) ([]BuildChange, error) {
	const op = "read the builds that changed"
	changes, err := queryAll(ctx, t.tx, op, changedBuilds, func(row pgx.CollectableRow) (BuildChange, error) {
		var c BuildChange
		err := row.Scan(&c.Attempt.ID, &c.UpdatedAt, &c.Namespace, &c.ProjectSlug, &c.EnvironmentSlug, &c.AppSlug)
		return c, err
	}, t.org.String(), sinceMs, limit)
	if err != nil || len(changes) == 0 {
		return nil, err
	}
	wanted := make([]uuid.UUID, 0, len(changes))
	for _, c := range changes {
		wanted = append(wanted, c.Attempt.ID.UUID())
	}
	attempts, err := queryAll(ctx, t.tx, op, attemptsByID, scanAttempt, t.org.String(), wanted)
	if err != nil {
		return nil, err
	}
	byID := make(map[uuid.UUID]BuildAttempt, len(attempts))
	for _, a := range attempts {
		byID[a.ID.UUID()] = a
	}
	out := make([]BuildChange, 0, len(changes))
	for _, c := range changes {
		if a, ok := byID[c.Attempt.ID.UUID()]; ok {
			c.Attempt = a
			out = append(out, c)
		}
	}
	return out, nil
}
