package httpapi

import (
	"net/http"
	"slices"
	"strings"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/httpx"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/store"
)

// selfAudited operations write their own, richer record: the attempted
// email, or a deployment accepted in the same transaction as its record.
var selfAudited = []string{"login", "startDeployment"}

// Outcome is the audit outcome of an HTTP status (audit.rs).
func Outcome(status int) string {
	switch {
	case status >= 200 && status < 400:
		return "success"
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "denied"
	case status == http.StatusTooManyRequests:
		return "throttled"
	case status >= 400 && status < 500:
		return "failure"
	default:
		return "error"
	}
}

// PathParams pairs the `{name}` segments of a route template with the
// values of a concrete path.
func PathParams(template, path string) [][2]string {
	ts, ps := strings.Split(template, "/"), strings.Split(path, "/")
	var out [][2]string
	for i := range min(len(ts), len(ps)) {
		if name, ok := strings.CutPrefix(ts[i], "{"); ok {
			if name, ok = strings.CutSuffix(name, "}"); ok {
				out = append(out, [2]string{name, ps[i]})
			}
		}
	}
	return out
}

// audit records every mutating API request after it ran (scenario 2). The
// action is the operation id of the matched route, so a new endpoint is
// audited without a line in its handler; request bodies are never
// recorded, so secret values cannot reach the log.
func (s *Server) audit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		route, found := s.routes.FindRoute(r.Method, r.URL.Path)
		if !found {
			next.ServeHTTP(w, r)
			return
		}
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		action := route.OperationID()
		if slices.Contains(selfAudited, action) {
			return
		}
		s.writeAudit(r, route.PathPattern(), action, rec.Status())
	})
}

// statusRecorder remembers the status a handler wrote. The audit cannot
// ask the outer httpx.Writer: it runs inside http.TimeoutHandler, whose
// buffered writer reaches the outer one only after the handler returned.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

// Status is the status written (200 when the handler wrote only a body).
func (w *statusRecorder) Status() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// WriteHeader records the first status and passes it on.
func (w *statusRecorder) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) writeAudit(r *http.Request, template, action string, status int) {
	ctx := r.Context()
	params := PathParams(template, r.URL.Path)
	current, authenticated := httpx.UserFrom(ctx)
	kind := "anonymous"
	actorID := opt.None[string]()
	token := opt.None[string]()
	org := opt.None[ids.OrgID]()
	if authenticated {
		kind = string(current.Via)
		actorID = opt.Some(current.User.ID.String())
		if g, ok := current.Token.Get(); ok {
			token = opt.Some(g.ID.String())
			org = opt.Some(g.Org)
		} else if b, err := s.deps.Store.BindingsForUser(ctx, current.User.ID); err == nil && len(b) > 0 {
			org = opt.Some(b[0].OrgID)
		}
	}
	targetKind, targetRef := opt.None[string](), opt.None[string]()
	if len(params) > 0 {
		values := make([]string, 0, len(params))
		for _, p := range params {
			values = append(values, p[1])
		}
		targetKind = opt.Some(params[len(params)-1][0])
		targetRef = opt.Some(strings.Join(values, "/"))
	}
	req := httpx.RequestFrom(ctx)
	_, err := s.deps.Store.AppendAudit(ctx, store.NewAudit{
		OrgID:      org,
		ActorKind:  kind,
		ActorID:    actorID,
		Action:     action,
		TargetKind: targetKind,
		TargetRef:  targetRef,
		Outcome:    Outcome(status),
		IP:         req.IP,
		RequestID:  opt.Some(req.ID),
		Data:       opt.Some[any](map[string]any{"status": status, "method": r.Method, "token": token}),
	})
	if err != nil {
		s.deps.Health.Metrics().AuditWriteFailed()
		s.deps.Logger.Error("audit write failed", "error", err)
	}
}
