package store

// Previews (M5.1); a partial port of repo/previews.rs: whether a target is
// an app of an untrusted preview, which a deployment checks, and closing a
// preview whose environment is deleted. Opening and syncing previews follow
// with the preview work.

import (
	"context"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
)

const untrustedTarget = "SELECT EXISTS (SELECT 1 FROM application_targets t " +
	"JOIN environment_placements pl ON pl.id = t.placement_id AND pl.org_id = t.org_id " +
	"JOIN previews p ON p.environment_id = pl.environment_id AND p.org_id = t.org_id " +
	"WHERE t.id = $1 AND t.org_id = $2 AND NOT p.trusted)"

// UntrustedTarget reports whether tgt is an app of an untrusted preview.
func (t *Tenant) UntrustedTarget(ctx context.Context, tgt ids.TargetID) (bool, error) {
	var untrusted bool
	err := queryOne(ctx, t.tx, "check a preview's trust", untrustedTarget, []any{&untrusted}, tgt, t.org.String())
	return untrusted, err
}

// CloseReason is why a preview closed.
type CloseReason string

// The close reasons, with their stored names.
const (
	// CloseClosed: the pull request was closed or merged.
	CloseClosed CloseReason = "closed"
	// CloseExpired: its lifetime ran out.
	CloseExpired CloseReason = "expired"
	// CloseManual: someone destroyed it.
	CloseManual CloseReason = "manual"
	// CloseDeleted: its environment was deleted.
	CloseDeleted CloseReason = "deleted"
)

const closePreview = "UPDATE previews SET state = 'closed', closed_at = $3, close_reason = $4, updated_at = $3, " +
	"last_event_at = GREATEST(last_event_at, $5) " +
	"WHERE environment_id = $1 AND org_id = $2 AND state = 'active'"

// ClosePreview closes the active preview of environment for reason; false
// when it has none.
func (t *Tenant) ClosePreview(ctx context.Context, environment ids.EnvironmentID, reason CloseReason, eventAt int64) (bool, error) {
	n, err := exec(ctx, t.tx, "close a preview", closePreview, environment, t.org.String(), t.store.now(),
		string(reason), eventAt)
	return n == 1, err
}
