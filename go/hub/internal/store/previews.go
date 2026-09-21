package store

// Previews (M5.1); a partial port of repo/previews.rs: whether a target is
// an app of an untrusted preview, which a deployment checks. Opening,
// syncing and closing previews follow with the preview work.

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
