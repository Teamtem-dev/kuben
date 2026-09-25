package api

// What the SQL-backed routes share (routes/request.rs, ADR-032): the audit
// record of an operation a request asks for, and duplicates as 409.

import (
	"github.com/Teamtem-dev/kuben/go/hub/internal/api/access"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/kerr"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
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
		return kerr.New(kerr.Conflict, "%s already exists", what)
	}
	return err
}
