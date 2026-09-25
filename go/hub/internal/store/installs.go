package store

// The installer's journal, copied from the host once the server runs
// (M2.6); the port of repo/installs.rs.

import (
	"context"

	"github.com/jackc/pgx/v5"
)

const (
	recordInstallJournal = "INSERT INTO install_journals (host, journal, recorded_at) " +
		"VALUES ($1, $2::jsonb, kuben_now_ms()) " +
		"ON CONFLICT (host) DO UPDATE SET journal = EXCLUDED.journal, recorded_at = EXCLUDED.recorded_at " +
		"WHERE install_journals.journal IS DISTINCT FROM EXCLUDED.journal"
	readInstallJournal = "SELECT journal::text, recorded_at FROM install_journals WHERE host = $1"
)

// RecordInstallJournal keeps journal (any JSON value) as the latest install
// journal of host; false when it was recorded already.
func (s *Store) RecordInstallJournal(ctx context.Context, host string, journal any) (bool, error) {
	const op = "record an install journal"
	text, err := canonical(op, journal)
	if err != nil {
		return false, err
	}
	n, err := exec(ctx, s.db, op, recordInstallJournal, host, text)
	return n == 1, err
}

// InstallJournal is the install journal recorded for host, with when it
// was recorded.
func (s *Store) InstallJournal(ctx context.Context, host string) (any, int64, bool, error) {
	const op = "read an install journal"
	type row struct {
		text string
		at   int64
	}
	r, ok, err := queryOpt(ctx, s.db, op, readInstallJournal, func(r pgx.CollectableRow) (row, error) {
		var out row
		return out, r.Scan(&out.text, &out.at)
	}, host)
	if err != nil || !ok {
		return nil, 0, false, err
	}
	journal, err := jsonValue(op, r.text)
	if err != nil {
		return nil, 0, false, err
	}
	return journal, r.at, true, nil
}
