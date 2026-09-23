package store

// Change freezes, silences and ownership (M4.9); a partial port of
// repo/controls.rs: the freeze a deployment checks and the pause the
// materializer holds runs for. Creating and lifting
// freezes, silences and owners follow with the control routes.

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
)

const (
	targetEnvironment = "SELECT p.environment_id FROM application_targets t " +
		"JOIN environment_placements p ON p.id = t.placement_id AND p.org_id = t.org_id " +
		"WHERE t.id = $1 AND t.org_id = $2"
	activeFreeze = "SELECT reason FROM environment_freezes " +
		"WHERE environment_id = $1 AND org_id = $2 AND lifted_at IS NULL AND starts_at <= $3 AND ends_at > $3 " +
		"ORDER BY ends_at DESC LIMIT 1"
	paused = "SELECT paused_at IS NOT NULL FROM application_targets WHERE id = $1 AND org_id = $2"
)

func (t *Tenant) environmentOf(ctx context.Context, tgt ids.TargetID) (ids.EnvironmentID, bool, error) {
	return queryOpt(ctx, t.tx, "find a target's environment", targetEnvironment, scanID[ids.Environment],
		tgt, t.org.String())
}

// ActiveFreeze is the reason of the freeze on tgt's environment at now, if
// there is one.
func (t *Tenant) ActiveFreeze(ctx context.Context, tgt ids.TargetID, now int64) (string, bool, error) {
	environment, ok, err := t.environmentOf(ctx, tgt)
	if err != nil || !ok {
		return "", false, err
	}
	return queryOpt(ctx, t.tx, "read the active freeze", activeFreeze, pgx.RowTo[string],
		environment, t.org.String(), now)
}

// TargetPaused is whether the delivery of tgt is held now; false for a
// target that does not exist.
func (t *Tenant) TargetPaused(ctx context.Context, tgt ids.TargetID) (bool, error) {
	held, _, err := queryOpt(ctx, t.tx, "read a target's pause", paused, pgx.RowTo[bool], tgt, t.org.String())
	return held, err
}
