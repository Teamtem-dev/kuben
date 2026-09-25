package api

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"strconv"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api/gen"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/authz"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/model"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/perm"
)

// The audit log reader (routes/audit.rs, scenario 2): newest first,
// cursor-paginated, limited to the organizations in which the caller holds
// audit-read.

// AuditPageSize is the page size a request asks for: 50 by default,
// clamped to 1–200.
func AuditPageSize(limit opt.Val[int64]) int64 {
	return min(max(limit.Or(50), 1), 200)
}

// AuditStatus is the HTTP status recorded in an audit record's data, when
// it holds one that fits a u16 (serde's `as_u64` then `u16::try_from`).
func AuditStatus(data opt.Val[any]) opt.Val[int32] {
	d, ok := data.Get()
	if !ok {
		return opt.None[int32]()
	}
	fields, ok := d.(map[string]any)
	if !ok {
		return opt.None[int32]()
	}
	// Stored documents are read with wire.DecodeAny: numbers are
	// json.Number, and serde's as_u64 takes only integer literals.
	n, ok := fields["status"].(json.Number)
	if !ok {
		return opt.None[int32]()
	}
	status, err := strconv.ParseUint(n.String(), 10, 16)
	if err != nil {
		return opt.None[int32]()
	}
	return opt.Some(int32(status))
}

// ListAudit is the audit log of the caller's organization(s).
func (s *Server) ListAudit(ctx context.Context, params gen.ListAuditParams) (gen.ListAuditRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	limit := AuditPageSize(opt.None[int64]())
	if l, ok := params.Limit.Get(); ok {
		limit = AuditPageSize(opt.Some(l))
	}
	before := opt.None[int64]()
	if b, ok := params.Before.Get(); ok {
		before = opt.Some(b)
	}
	orgs := []ids.OrgID{}
	for _, o := range a.OrgIDs() {
		if _, err := a.Require(perm.AuditRead, authz.OrgChain(o)); err == nil {
			orgs = append(orgs, o)
		}
	}
	if len(orgs) == 0 {
		return nil, kerr.ErrForbidden
	}
	events := []model.AuditEvent{}
	for _, org := range orgs {
		page, err := s.deps.Store.ListAudit(ctx, org, before, limit)
		if err != nil {
			return nil, err //nolint:wrapcheck // a store error, answered as internal
		}
		events = append(events, page...)
	}
	slices.SortStableFunc(events, func(x, y model.AuditEvent) int { return cmp.Compare(y.Seq, x.Seq) })
	events = events[:min(int64(len(events)), limit)]
	users, err := s.deps.Store.ListUsers(ctx)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	emails := make(map[string]string, len(users))
	for _, u := range users {
		emails[u.ID.String()] = u.Email
	}
	var next gen.OptNilInt64
	next.SetToNull()
	if int64(len(events)) == limit && len(events) > 0 {
		next = gen.NewOptNilInt64(events[len(events)-1].Seq)
	}
	out := make([]gen.AuditEventDto, 0, len(events))
	for _, e := range events {
		out = append(out, auditEventDto(e, emails))
	}
	return &gen.AuditPage{Events: out, NextBefore: next}, nil
}

func auditEventDto(e model.AuditEvent, emails map[string]string) gen.AuditEventDto {
	actor := opt.None[string]()
	if id, ok := e.ActorID.Get(); ok {
		if email, found := emails[id]; found {
			actor = opt.Some(email)
		} else {
			actor = opt.Some(id)
		}
	}
	var status gen.OptNilInt32
	if n, ok := AuditStatus(e.Data).Get(); ok {
		status = gen.NewOptNilInt32(n)
	} else {
		status.SetToNull()
	}
	return gen.AuditEventDto{
		Seq:        e.Seq,
		ID:         e.ID.String(),
		At:         e.CreatedAt,
		ActorKind:  e.ActorKind,
		Actor:      optNilString(actor),
		Action:     e.Action,
		TargetKind: optNilString(e.TargetKind),
		Target:     optNilString(e.TargetRef),
		Outcome:    e.Outcome,
		Status:     status,
		IP:         optNilString(e.IP),
		RequestID:  optNilString(e.RequestID),
	}
}
