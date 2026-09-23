package store

// Usage (M5.5); a partial port of repo/usage.rs: the count the
// organization's environment quota is checked against. Usage rollups
// follow with the usage work.

import "context"

const liveEnvironmentCount = "SELECT count(*) FROM environments " +
	"WHERE org_id = $1 AND deleted_at IS NULL AND NOT deleting"

// LiveEnvironmentCount is the number of the organization's live
// environments, deleting ones not counted.
func (t *Tenant) LiveEnvironmentCount(ctx context.Context) (uint64, error) {
	const op = "count live environments"
	var n int64
	if err := queryOne(ctx, t.tx, op, liveEnvironmentCount, []any{&n}, t.org.String()); err != nil {
		return 0, err
	}
	return counter(op, n)
}
