package store

// Hourly usage of apps (M5.5, migration 0034); the port of repo/rollups.rs.

import (
	"context"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
)

const (
	targetByName = "SELECT t.id FROM application_targets t " +
		"JOIN environment_placements pl ON pl.id = t.placement_id AND pl.org_id = t.org_id " +
		"JOIN applications a ON a.id = t.application_id AND a.org_id = t.org_id " +
		"WHERE t.org_id = $1 AND pl.namespace = $2 AND a.slug = $3 AND t.deleted_at IS NULL " +
		"LIMIT 1"
	keepUsage = "INSERT INTO usage_rollups " +
		"(target_id, org_id, hour, cpu_avg, cpu_max, memory_avg, memory_max, samples) " +
		"VALUES ($1, $2, $3, $4, $5, $6, $7, $8) " +
		"ON CONFLICT (target_id, hour) DO UPDATE SET " +
		"cpu_avg = CASE WHEN EXCLUDED.samples > usage_rollups.samples THEN EXCLUDED.cpu_avg ELSE usage_rollups.cpu_avg END, " +
		"memory_avg = CASE WHEN EXCLUDED.samples > usage_rollups.samples THEN EXCLUDED.memory_avg ELSE usage_rollups.memory_avg END, " +
		"cpu_max = GREATEST(usage_rollups.cpu_max, EXCLUDED.cpu_max), " +
		"memory_max = GREATEST(usage_rollups.memory_max, EXCLUDED.memory_max), " +
		"samples = GREATEST(usage_rollups.samples, EXCLUDED.samples)"
	usageSince = "SELECT hour, cpu_avg, cpu_max, memory_avg, memory_max, samples FROM usage_rollups " +
		"WHERE org_id = $1 AND target_id = $2 AND hour >= $3 ORDER BY hour"
)

// UsageHour is an hour of an app's usage, as stored.
type UsageHour struct {
	// Hour is the hour's start, unix milliseconds.
	Hour      int64
	CPUAvg    int64
	CPUMax    int64
	MemoryAvg int64
	MemoryMax int64
	Samples   int32
}

// UsageRollup is an hour of an app's usage to keep: CPU in millicores,
// memory in bytes, averaged and peaked over Samples samples.
type UsageRollup struct {
	// Hour is the hour's start, unix milliseconds.
	Hour      int64
	CPUAvg    uint64
	CPUMax    uint64
	MemoryAvg uint64
	MemoryMax uint64
	Samples   uint32
}

// TargetByName is the live app slug in namespace.
func (t *Tenant) TargetByName(ctx context.Context, namespace, slug string) (ids.TargetID, bool, error) {
	return queryOpt(ctx, t.tx, "find a target by name", targetByName, scanID[ids.Target],
		t.org.String(), namespace, slug)
}

// KeepUsage keeps an hour of target's usage. A repeated hour keeps the
// averages of the fuller sample and the higher peaks.
func (t *Tenant) KeepUsage(ctx context.Context, target ids.TargetID, r UsageRollup) error {
	const op = "keep usage"
	values := make([]int64, 0, 4)
	for _, v := range []uint64{r.CPUAvg, r.CPUMax, r.MemoryAvg, r.MemoryMax} {
		if v > math.MaxInt64 {
			return dbErr(op, fmt.Errorf("error occurred while encoding a value: %d is out of range for BIGINT", v))
		}
		values = append(values, int64(v))
	}
	if r.Samples > math.MaxInt32 {
		return dbErr(op, fmt.Errorf("error occurred while encoding a value: %d is out of range for INTEGER", r.Samples))
	}
	_, err := exec(ctx, t.tx, op, keepUsage, target, t.org.String(), r.Hour,
		values[0], values[1], values[2], values[3], int32(r.Samples))
	return err
}

// UsageSince is target's hours since since, oldest first.
func (t *Tenant) UsageSince(ctx context.Context, target ids.TargetID, since int64) ([]UsageHour, error) {
	return queryAll(ctx, t.tx, "read usage", usageSince, func(row pgx.CollectableRow) (UsageHour, error) {
		var h UsageHour
		err := row.Scan(&h.Hour, &h.CPUAvg, &h.CPUMax, &h.MemoryAvg, &h.MemoryMax, &h.Samples)
		return h, err
	}, t.org.String(), target, since)
}
