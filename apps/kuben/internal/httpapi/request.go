package httpapi

// What the SQL-backed routes share (routes/request.rs, ADR-032): the audit
// record of an operation a request asks for, and duplicates as 409.

import (
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/access"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

// requestAudit is the audit record of an operation a request asked for.
// The request itself is audited by the middleware as usual.
func requestAudit(a access.Access, action, targetKind, targetRef string) store.NewAudit {
	kind, _ := a.Actor()
	return store.NewAudit{
		ActorKind:  kind,
		ActorID:    opt.Some(a.Current.User.ID.String()),
		Action:     action,
		TargetKind: opt.Some(targetKind),
		TargetRef:  opt.Some(targetRef),
		Outcome:    "accepted",
	}
}

// duplicate is a write refused as a duplicate: 409, naming what exists.
// Any other error is returned as it is.
func duplicate(err error, what string) error {
	if store.IsUniqueViolation(err) {
		return kerrors.New(kerrors.Conflict, "%s already exists", what)
	}
	return err
}
