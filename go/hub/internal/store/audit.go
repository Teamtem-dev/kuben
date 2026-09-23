package store

// The append-only audit log; the port of repo/audit.rs.

import (
	"context"
	"math"

	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/model"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/wire"
)

// NewAudit is the input for an audit record. The log is append-only: there
// is no update or delete.
type NewAudit struct {
	OrgID      opt.Val[ids.OrgID]
	ActorKind  string
	ActorID    opt.Val[string]
	Action     string
	TargetKind opt.Val[string]
	TargetRef  opt.Val[string]
	Outcome    string
	IP         opt.Val[string]
	RequestID  opt.Val[string]
	// Data is any JSON value; it is stored as serde_json wrote it (compact,
	// keys sorted).
	Data opt.Val[any]
}

const (
	insertAudit = "INSERT INTO audit_events " +
		"(id, org_id, actor_kind, actor_id, action, target_kind, target_ref, outcome, ip, request_id, data, created_at) " +
		"VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)"
	selectRecentAudit = "SELECT seq, id, org_id, actor_kind, actor_id, action, target_kind, target_ref, outcome, ip, " +
		"request_id, data, created_at FROM audit_events ORDER BY seq DESC LIMIT $1"
	selectOrgAuditPage = "SELECT seq, id, org_id, actor_kind, actor_id, action, target_kind, target_ref, " +
		"outcome, ip, request_id, data, created_at FROM audit_events WHERE org_id = $1 AND seq < $2 " +
		"ORDER BY seq DESC LIMIT $3"
)

// insert appends a on q: the pool, or the transaction of an accepted
// request, so the record commits with it (I01).
func (a NewAudit) insert(ctx context.Context, q querier, now int64) (ids.AuditID, error) {
	const op = "append an audit record"
	id := ids.New[ids.Audit]()
	data := opt.None[string]()
	if v, ok := a.Data.Get(); ok {
		text, err := wire.CanonicalValue(v)
		if err != nil {
			return ids.AuditID{}, dbErr(op, err)
		}
		data = opt.Some(text)
	}
	orgID := opt.None[string]()
	if org, ok := a.OrgID.Get(); ok {
		orgID = opt.Some(org.String())
	}
	_, err := exec(ctx, q, op, insertAudit,
		id.String(), orgID.Ptr(), a.ActorKind, a.ActorID.Ptr(), a.Action, a.TargetKind.Ptr(), a.TargetRef.Ptr(),
		a.Outcome, a.IP.Ptr(), a.RequestID.Ptr(), data.Ptr(), now)
	if err != nil {
		return ids.AuditID{}, err
	}
	return id, nil
}

func scanAudit(row pgx.CollectableRow) (model.AuditEvent, error) {
	var e model.AuditEvent
	var org *ids.OrgID
	var actorID, targetKind, targetRef, ip, requestID, data *string
	err := row.Scan(&e.Seq, &e.ID, &org, &e.ActorKind, &actorID, &e.Action, &targetKind, &targetRef,
		&e.Outcome, &ip, &requestID, &data, &e.CreatedAt)
	if err != nil {
		return model.AuditEvent{}, err
	}
	e.OrgID = opt.FromPtr(org)
	e.ActorID = opt.FromPtr(actorID)
	e.TargetKind = opt.FromPtr(targetKind)
	e.TargetRef = opt.FromPtr(targetRef)
	e.IP = opt.FromPtr(ip)
	e.RequestID = opt.FromPtr(requestID)
	// Data that is not JSON reads as absent, as in Rust (`.ok()`).
	if data != nil {
		if v, err := wire.DecodeAny([]byte(*data)); err == nil {
			e.Data = opt.Some(v)
		}
	}
	return e, nil
}

// AppendAudit appends one audit record.
func (s *Store) AppendAudit(ctx context.Context, a NewAudit) (ids.AuditID, error) {
	return a.insert(ctx, s.db, s.now())
}

// AppendAudit appends an audit record to the tenant's transaction.
func (t *Tenant) AppendAudit(ctx context.Context, a NewAudit) error {
	a.OrgID = opt.Some(t.org)
	_, err := a.insert(ctx, t.tx, t.store.now())
	return err
}

// ListAudit is one page of an org's audit log, newest first. before is the
// seq of the last event of the previous page.
func (s *Store) ListAudit(ctx context.Context, org ids.OrgID, before opt.Val[int64], limit int64) ([]model.AuditEvent, error) {
	return queryAll(ctx, s.db, "list the audit log", selectOrgAuditPage, scanAudit,
		org.String(), before.Or(math.MaxInt64), limit)
}

// RecentAudit is the newest limit audit records of every organization.
func (s *Store) RecentAudit(ctx context.Context, limit int64) ([]model.AuditEvent, error) {
	return queryAll(ctx, s.db, "read recent audit records", selectRecentAudit, scanAudit, limit)
}
