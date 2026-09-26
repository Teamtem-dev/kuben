package store

// Login sessions; the port of repo/sessions.rs.

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/internal/core/model"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
)

// NewSession is the input for creating a session. IDHash is
// `sha256(session_id)`; the raw id only ever lives in the browser cookie
// (Invariant I-3).
type NewSession struct {
	IDHash    []byte
	UserID    ids.UserID
	ExpiresAt int64
	IP        opt.Val[string]
	UAHash    opt.Val[[]byte]
}

const (
	insertSession = "INSERT INTO sessions (id_hash, user_id, created_at, expires_at, last_seen_at, ip, ua_hash) " +
		"VALUES ($1, $2, $3, $4, $5, $6, $7)"
	selectSession       = "SELECT user_id, created_at, expires_at, last_seen_at, revoked_at FROM sessions WHERE id_hash = $1"
	touchSession        = "UPDATE sessions SET last_seen_at = $2 WHERE id_hash = $1"
	revokeSession       = "UPDATE sessions SET revoked_at = $2 WHERE id_hash = $1 AND revoked_at IS NULL"
	revokeAllForUser    = "UPDATE sessions SET revoked_at = $2 WHERE user_id = $1 AND revoked_at IS NULL"
	revokeOthersForUser = "UPDATE sessions SET revoked_at = $3 WHERE user_id = $1 AND id_hash <> $2 AND revoked_at IS NULL"
	deleteExpired       = "DELETE FROM sessions WHERE expires_at < $1"
)

func scanSession(row pgx.CollectableRow) (model.Session, error) {
	var s model.Session
	var lastSeen, revoked *int64
	if err := row.Scan(&s.UserID, &s.CreatedAt, &s.ExpiresAt, &lastSeen, &revoked); err != nil {
		return model.Session{}, err
	}
	s.LastSeenAt = opt.FromPtr(lastSeen)
	s.RevokedAt = opt.FromPtr(revoked)
	return s, nil
}

// CreateSession stores a new session, last seen now.
func (s *Store) CreateSession(ctx context.Context, n NewSession) error {
	now := s.now()
	_, err := exec(ctx, s.db, "create a session", insertSession,
		n.IDHash, n.UserID.String(), now, n.ExpiresAt, now, n.IP.Ptr(), n.UAHash.Ptr())
	return err
}

// FindSession is the session whose id hashes to idHash.
func (s *Store) FindSession(ctx context.Context, idHash []byte) (model.Session, bool, error) {
	return queryOpt(ctx, s.db, "find a session", selectSession, scanSession, idHash)
}

// TouchSession records that the session was used now.
func (s *Store) TouchSession(ctx context.Context, idHash []byte) error {
	_, err := exec(ctx, s.db, "touch a session", touchSession, idHash, s.now())
	return err
}

// RevokeSession revokes one session (once).
func (s *Store) RevokeSession(ctx context.Context, idHash []byte) error {
	_, err := exec(ctx, s.db, "revoke a session", revokeSession, idHash, s.now())
	return err
}

// RevokeAllSessions revokes every live session of user; it returns how
// many there were.
func (s *Store) RevokeAllSessions(ctx context.Context, user ids.UserID) (uint64, error) {
	return exec(ctx, s.db, "revoke sessions", revokeAllForUser, user.String(), s.now())
}

// RevokeOtherSessions revokes every session of user except keep (after a
// password change).
func (s *Store) RevokeOtherSessions(ctx context.Context, user ids.UserID, keep []byte) (uint64, error) {
	return exec(ctx, s.db, "revoke other sessions", revokeOthersForUser, user.String(), keep, s.now())
}

// PurgeExpiredSessions deletes the sessions that have expired.
func (s *Store) PurgeExpiredSessions(ctx context.Context) (uint64, error) {
	return exec(ctx, s.db, "purge expired sessions", deleteExpired, s.now())
}
