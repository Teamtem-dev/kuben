package store

// Login-throttle windows (scenario 1), shared by every replica; the port of
// repo/throttle.rs. Buckets are opaque keys chosen by the caller, which
// hashes them: no email address or client IP is stored.

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// ThrottleWindow is a fixed window: Failures counted since StartedAt (unix
// ms).
type ThrottleWindow struct {
	Failures  int64
	StartedAt int64
}

const (
	selectWindow = "SELECT failures, started_at FROM login_throttle WHERE bucket = $1"
	// recordFailure counts one failure atomically. A window that began at or
	// before $3 (now - window) has expired and restarts at 1.
	recordFailure = "INSERT INTO login_throttle (bucket, failures, started_at) VALUES ($1, 1, $2) " +
		"ON CONFLICT (bucket) DO UPDATE SET " +
		"failures = CASE WHEN login_throttle.started_at > $3 THEN login_throttle.failures + 1 ELSE 1 END, " +
		"started_at = CASE WHEN login_throttle.started_at > $3 THEN login_throttle.started_at ELSE $2 END"
	clearWindow  = "DELETE FROM login_throttle WHERE bucket = $1"
	purgeWindows = "DELETE FROM login_throttle WHERE started_at <= $1"
)

// ThrottleWindow is bucket's current window, if it has one.
func (s *Store) ThrottleWindow(ctx context.Context, bucket string) (ThrottleWindow, bool, error) {
	return queryOpt(ctx, s.db, "read a throttle window", selectWindow,
		func(row pgx.CollectableRow) (ThrottleWindow, error) {
			var w ThrottleWindow
			err := row.Scan(&w.Failures, &w.StartedAt)
			return w, err
		}, bucket)
}

// ThrottleRecordFailure records a failed attempt at now; windows that
// started at or before windowStart are replaced by a new one.
func (s *Store) ThrottleRecordFailure(ctx context.Context, bucket string, now, windowStart int64) error {
	_, err := exec(ctx, s.db, "record a failed attempt", recordFailure, bucket, now, windowStart)
	return err
}

// ThrottleClear forgets bucket's window.
func (s *Store) ThrottleClear(ctx context.Context, bucket string) error {
	_, err := exec(ctx, s.db, "clear a throttle window", clearWindow, bucket)
	return err
}

// ThrottlePurge drops windows that started at or before windowStart
// (expired) and returns how many.
func (s *Store) ThrottlePurge(ctx context.Context, windowStart int64) (uint64, error) {
	return exec(ctx, s.db, "purge throttle windows", purgeWindows, windowStart)
}
