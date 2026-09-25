package store

// Backups, restores and the database's own facts (M4.7, migration 0025);
// the port of repo/backups.rs.
//
// After a restore the installation must not trust what happened after the
// backup was taken: [Store.AfterRestore] ends every session, revokes every
// API token, and raises every target's generation far beyond any the
// clusters may have seen, so envelopes and objects written in the lost
// interval are outranked instead of mistaken for newer state.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ops/target"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
)

// RestoreGenerationJump is how far a restore raises every generation.
const RestoreGenerationJump int64 = 1 << 20

const (
	insertBackup = "INSERT INTO backup_runs " +
		"(id, kind, status, location, bytes, sha256, kuben, schema, detail, started_at, finished_at) " +
		"VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)"
	lastGoodBackup = "SELECT location, bytes, sha256, finished_at FROM backup_runs " +
		"WHERE status = 'succeeded' ORDER BY finished_at DESC LIMIT 1"
	schemaVersion = "SELECT COALESCE(max(version), 0) FROM _sqlx_migrations WHERE success"
	countTables   = "SELECT count(*) FROM information_schema.tables WHERE table_schema = current_schema()"
	databaseFacts = "SELECT current_setting('server_version_num')::int, " +
		"COALESCE((SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()), false), " +
		"current_setting('wal_level'), current_setting('archive_mode'), pg_is_in_recovery()"
	endSessions      = "UPDATE sessions SET revoked_at = $1 WHERE revoked_at IS NULL"
	revokeAllTokens  = "UPDATE api_tokens SET revoked_at = $1 WHERE revoked_at IS NULL"
	raiseGenerations = "UPDATE application_targets " +
		"SET desired_generation = desired_generation + $2 WHERE org_id = $1"
	// redeliverable is every live target with the release and
	// configuration of its newest run.
	redeliverable = "SELECT t.project_id, t.id, r.release_id, r.config_revision_id, " +
		"t.desired_generation, t.lifecycle_uid FROM application_targets t " +
		"JOIN LATERAL (SELECT d.release_id, d.config_revision_id FROM deployment_runs d " +
		"WHERE d.target_id = t.id AND d.org_id = t.org_id ORDER BY d.generation DESC LIMIT 1) r ON TRUE " +
		"WHERE t.org_id = $1 AND NOT t.deleting"
	insertRestore = "INSERT INTO restore_events " +
		"(id, backup_created_at, backup_kuben, kuben, generation_jump, restored_at) " +
		"VALUES ($1, $2, $3, $4, $5, $6)"
)

// BackupRecord is a backup written.
type BackupRecord struct {
	Scheduled  bool
	Succeeded  bool
	Location   string
	Bytes      opt.Val[uint64]
	SHA256     opt.Val[string]
	Kuben      string
	Schema     int64
	Detail     opt.Val[string]
	StartedAt  int64
	FinishedAt int64
}

// LastBackup is the newest good backup.
type LastBackup struct {
	Location   string
	Bytes      opt.Val[uint64]
	SHA256     opt.Val[string]
	FinishedAt int64
}

// DatabaseFacts is what the database server says about itself.
type DatabaseFacts struct {
	// VersionNum is `server_version_num`, e.g. 170004.
	VersionNum int32
	// TLS: this connection is encrypted.
	TLS         bool
	WALLevel    string
	ArchiveMode string
	// InRecovery: a standby, where Kuben cannot write.
	InRecovery bool
}

// Major is the major version, e.g. 17.
func (f DatabaseFacts) Major() int32 { return f.VersionNum / 10_000 }

// ArchivesWAL reports whether WAL is archived, so a point-in-time recovery
// is possible.
func (f DatabaseFacts) ArchivesWAL() bool {
	return (f.ArchiveMode == "on" || f.ArchiveMode == "always") && f.WALLevel != "minimal"
}

// Restored is what a restore changed.
type Restored struct {
	SessionsEnded uint64
	TokensRevoked uint64
	TargetsRaised uint64
}

// DatabaseIsEmpty reports whether the database at url has no tables yet
// (never migrated).
func DatabaseIsEmpty(ctx context.Context, url string) (bool, error) {
	const op = "check the database is empty"
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return false, dbErr(op, err)
	}
	var tables int64
	err = conn.QueryRow(ctx, countTables).Scan(&tables)
	return tables == 0, errors.Join(dbErr(op, err), dbErr(op, conn.Close(ctx)))
}

// SchemaVersion is the newest migration applied.
func (s *Store) SchemaVersion(ctx context.Context) (int64, error) {
	var v int64
	return v, dbErr("read the schema version", s.db.QueryRow(ctx, schemaVersion).Scan(&v))
}

// DatabaseFacts is the database server's version, encryption and WAL
// archiving.
func (s *Store) DatabaseFacts(ctx context.Context) (DatabaseFacts, error) {
	var f DatabaseFacts
	err := s.db.QueryRow(ctx, databaseFacts).Scan(&f.VersionNum, &f.TLS, &f.WALLevel, &f.ArchiveMode, &f.InRecovery)
	return f, dbErr("read the database facts", err)
}

// RecordBackup records a backup attempt.
func (s *Store) RecordBackup(ctx context.Context, b BackupRecord) error {
	const op = "record a backup"
	bytes := opt.None[int64]()
	if n, ok := b.Bytes.Get(); ok {
		if n > math.MaxInt64 {
			return protocolErr(op, "a backup of %d bytes does not fit a bigint", n)
		}
		bytes = opt.Some(int64(n))
	}
	kind, status := "manual", "failed"
	if b.Scheduled {
		kind = "scheduled"
	}
	if b.Succeeded {
		status = "succeeded"
	}
	id, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	_, err = exec(ctx, s.db, op, insertBackup, id, kind, status, b.Location, bytes.Ptr(), b.SHA256.Ptr(),
		b.Kuben, b.Schema, b.Detail.Ptr(), b.StartedAt, b.FinishedAt)
	return err
}

// LastBackup is the newest good backup, if any.
func (s *Store) LastBackup(ctx context.Context) (LastBackup, bool, error) {
	const op = "read the last backup"
	var (
		last   LastBackup
		bytes  *int64
		sha256 *string
	)
	err := s.db.QueryRow(ctx, lastGoodBackup).Scan(&last.Location, &bytes, &sha256, &last.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return LastBackup{}, false, nil
	}
	if err != nil {
		return LastBackup{}, false, dbErr(op, err)
	}
	if bytes != nil {
		n, err := counter(op, *bytes)
		if err != nil {
			return LastBackup{}, false, err
		}
		last.Bytes = opt.Some(n)
	}
	last.SHA256 = opt.FromPtr(sha256)
	return last, true, nil
}

// AfterRestore fences everything that happened after the restored backup
// was taken, in one transaction per step: sessions, API tokens, and the
// generations of every organization's targets.
func (s *Store) AfterRestore(ctx context.Context, backupCreatedAt int64, backupKuben, kuben string) (Restored, error) {
	const op = "fence a restore"
	now := s.now()
	var r Restored
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return r, dbErr(op, err)
	}
	defer func() { _ = rollback(ctx, tx) }() //nolint:errcheck // committed below on success
	if r.SessionsEnded, err = exec(ctx, tx, op, endSessions, now); err != nil {
		return r, err
	}
	if r.TokensRevoked, err = exec(ctx, tx, op, revokeAllTokens, now); err != nil {
		return r, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return r, fmt.Errorf("%s: %w", op, err)
	}
	if _, err := exec(ctx, tx, op, insertRestore, id, backupCreatedAt, backupKuben, kuben, RestoreGenerationJump, now); err != nil {
		return r, err
	}
	if err := tx.Commit(ctx); err != nil {
		return r, dbErr(op, err)
	}
	orgs, err := s.OrgIDs(ctx)
	if err != nil {
		return r, err
	}
	for _, org := range orgs {
		raised, err := s.raiseGenerations(ctx, org)
		if err != nil {
			return r, err
		}
		r.TargetsRaised += raised
	}
	return r, nil
}

func (s *Store) raiseGenerations(ctx context.Context, org ids.OrgID) (uint64, error) {
	t, err := s.Tenant(ctx, org)
	if err != nil {
		return 0, err
	}
	defer t.Rollback(ctx) //nolint:errcheck // committed below on success
	raised, err := exec(ctx, t.tx, "raise the generations", raiseGenerations, org.String(), RestoreGenerationJump)
	if err != nil {
		return 0, err
	}
	return raised, t.Commit(ctx)
}

// RedeliverAll starts a run of every live target's newest release and
// configuration, so the restored state is written again (to a fresh
// cluster, too). Runs are restarts: they carry nothing new, so neither
// approvals nor the scan gate hold them. It is the number started.
func (s *Store) RedeliverAll(ctx context.Context, requestedBy string) (int, error) {
	orgs, err := s.OrgIDs(ctx)
	if err != nil {
		return 0, err
	}
	started := 0
	for _, org := range orgs {
		n, err := s.redeliver(ctx, org, requestedBy)
		if err != nil {
			return started, err
		}
		started += n
	}
	return started, nil
}

// redeliverRow is a row of the redeliverable query.
type redeliverRow struct {
	project, target, release, config, lifecycle uuid.UUID
	generation                                  int64
}

func (s *Store) redeliver(ctx context.Context, org ids.OrgID, requestedBy string) (int, error) {
	const op = "redeliver the targets"
	t, err := s.Tenant(ctx, org)
	if err != nil {
		return 0, err
	}
	defer t.Rollback(ctx) //nolint:errcheck // committed below on success
	rows, err := queryAll(ctx, t.tx, op, redeliverable, func(r pgx.CollectableRow) (redeliverRow, error) {
		var x redeliverRow
		return x, r.Scan(&x.project, &x.target, &x.release, &x.config, &x.generation, &x.lifecycle)
	}, org.String())
	if err != nil {
		return 0, err
	}
	started := 0
	for _, row := range rows {
		generation, err := counter(op, row.generation)
		if err != nil {
			return 0, err
		}
		req := StartDeployment{
			Project: ids.From[ids.Project](row.project), Target: ids.From[ids.Target](row.target),
			Release: ids.From[ids.Release](row.release), ConfigRevision: ids.From[ids.ConfigRevision](row.config),
			ExpectedGeneration: target.Generation(generation), LifecycleUID: row.lifecycle,
			Reason: ReasonRestart, RequestedBy: requestedBy,
			InputHash: fmt.Appendf(nil, "restore/%s/%d", row.target, row.generation),
		}
		audit := NewAudit{
			ActorKind: "system", ActorID: opt.Some(requestedBy), Action: "deployment.redelivered",
			TargetKind: opt.Some("app"), TargetRef: opt.Some(row.target.String()), Outcome: "accepted",
		}
		result, err := t.StartDeployment(ctx, req, audit, opt.None[IdempotencyKey]())
		if err != nil {
			return 0, err
		}
		if _, ok := result.(StartedAccepted); ok {
			started++
		}
	}
	return started, t.Commit(ctx)
}
