package store

// What an organization's apps may request, for admission (M4.5); the port
// of repo/usage.rs.
//
// Admission reads the newest configuration of every live target: that is
// what the next run of each renders. Resources are computed from it with
// the platform's size presets outside the store.

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
)

const (
	liveConfigs = "SELECT t.id, p.environment_id, c.config::text AS config " +
		"FROM application_targets t " +
		"JOIN environment_placements p ON p.id = t.placement_id AND p.org_id = t.org_id " +
		"LEFT JOIN LATERAL (SELECT r.config FROM target_config_revisions r " +
		"WHERE r.target_id = t.id AND r.org_id = t.org_id ORDER BY r.revision DESC LIMIT 1) c ON TRUE " +
		"WHERE t.org_id = $1 AND NOT t.deleting"
	liveEnvironmentCount = "SELECT count(*) FROM environments " +
		"WHERE org_id = $1 AND deleted_at IS NULL AND NOT deleting"
)

// LiveConfig is a live target and its newest configuration, if it has one.
type LiveConfig struct {
	Target      ids.TargetID
	Environment ids.EnvironmentID
	Config      opt.Val[any]
}

// LiveConfigs is every live target of the organization with its newest
// configuration.
func (t *Tenant) LiveConfigs(ctx context.Context) ([]LiveConfig, error) {
	return queryAll(ctx, t.tx, "read live configurations", liveConfigs, func(row pgx.CollectableRow) (LiveConfig, error) {
		var c LiveConfig
		var config *string
		if err := row.Scan(&c.Target, &c.Environment, &config); err != nil {
			return LiveConfig{}, err
		}
		var err error
		c.Config, err = jsonColumn(config)
		return c, err
	}, t.org.String())
}

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

// AdmitEnvironment is admission::admit_environment (M4.5): a conflict when
// the organization already has as many live environments as quota allows.
// No quota admits everything. The API and the preview lifecycle both admit
// new environments through it.
func (t *Tenant) AdmitEnvironment(ctx context.Context, quota opt.Val[uint64]) error {
	limit, ok := quota.Get()
	if !ok {
		return nil
	}
	n, err := t.LiveEnvironmentCount(ctx)
	if err != nil {
		return err
	}
	if n >= limit {
		return kerr.New(kerr.Conflict, "the organization's quota allows %d environments", limit)
	}
	return nil
}
