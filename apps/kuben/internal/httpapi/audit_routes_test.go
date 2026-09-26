package httpapi_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi"
)

func TestAuditPageSizes(t *testing.T) {
	cases := []struct {
		limit opt.Val[int64]
		want  int64
	}{
		{opt.None[int64](), 50},
		{opt.Some[int64](0), 1},
		{opt.Some[int64](-3), 1},
		{opt.Some[int64](20), 20},
		{opt.Some[int64](201), 200},
		{opt.Some[int64](1 << 62), 200},
	}
	for _, c := range cases {
		if got := httpapi.AuditPageSize(c.limit); got != c.want {
			t.Errorf("%v: %d, want %d", c.limit, got, c.want)
		}
	}
}

func TestAuditStatusIsAU16(t *testing.T) {
	cases := []struct {
		data opt.Val[any]
		want opt.Val[int32]
	}{
		{opt.None[any](), opt.None[int32]()},
		{opt.Some[any](map[string]any{"status": json.Number("403")}), opt.Some[int32](403)},
		{opt.Some[any](map[string]any{"status": json.Number("70000")}), opt.None[int32]()},
		{opt.Some[any](map[string]any{"status": json.Number("-1")}), opt.None[int32]()},
		{opt.Some[any](map[string]any{"status": json.Number("4.5")}), opt.None[int32]()},
		{opt.Some[any](map[string]any{"status": json.Number("403.0")}), opt.None[int32]()},
		{opt.Some[any](map[string]any{"status": float64(403)}), opt.None[int32]()},
		{opt.Some[any](map[string]any{"status": "403"}), opt.None[int32]()},
		{opt.Some[any](map[string]any{}), opt.None[int32]()},
		{opt.Some[any]([]any{float64(403)}), opt.None[int32]()},
	}
	for _, c := range cases {
		if got := httpapi.AuditStatus(c.data); got != c.want {
			t.Errorf("%v: %v, want %v", c.data, got, c.want)
		}
	}
}

type auditEvent struct{ action, outcome string }

// tests/http.rs scenario2_every_mutation_is_audited_without_handler_code.
func TestScenario2EveryMutationIsAuditedWithoutHandlerCode(t *testing.T) {
	f := newFixture(t)
	alice := f.signIn("alice@example.com", seedPassword)
	bob := f.signIn("bob@example.com", seedPassword)
	body := map[string]any{"name": "blog", "display_name": "Blog"}
	if status := bob.status("POST", "/api/v1/projects", body); status != 403 {
		t.Fatalf("bob creates a project: %d", status)
	}
	if status := alice.status("POST", "/api/v1/projects", body); status != 201 {
		t.Fatalf("SQL holds projects: %d", status)
	}

	status, page, _ := alice.do("GET", "/api/v1/audit", nil)
	if status != 200 {
		t.Fatalf("audit: %d %v", status, page)
	}
	events := auditEvents(t, page)
	seen := make([]auditEvent, 0, len(events))
	for _, e := range events {
		action, _ := e["action"].(string)
		outcome, _ := e["outcome"].(string)
		seen = append(seen, auditEvent{action, outcome})
	}
	want := []auditEvent{{"createProject", "denied"}, {"createProject", "success"}, {"project.apply", "accepted"}, {"login", "success"}}
	for _, want := range want {
		if !slices.Contains(seen, want) {
			t.Errorf("missing %v in %v", want, seen)
		}
	}
	i := slices.IndexFunc(events, func(e map[string]any) bool { return e["outcome"] == "denied" })
	if i < 0 {
		t.Fatal("no denied event")
	}
	denied := events[i]
	if denied["actor"] != "bob@example.com" || denied["status"] != float64(403) || denied["actor_kind"] != "session" {
		t.Fatalf("denied: %v", denied)
	}
	if page["next_before"] != nil {
		t.Fatalf("one page: %v", page["next_before"])
	}
	if status := bob.status("GET", "/api/v1/audit", nil); status != 403 {
		t.Fatalf("viewers cannot read the audit log: %d", status)
	}
}

func TestAuditPagesAreNewestFirst(t *testing.T) {
	f := newFixture(t)
	alice := f.signIn("alice@example.com", seedPassword)
	for i := range 3 {
		if status, _ := invite(alice, fmt.Sprintf("user%d@example.com", i), "viewer"); status != 201 {
			t.Fatalf("invite %d: %d", i, status)
		}
	}
	seqs := []float64{}
	path := "/api/v1/audit?limit=2"
	for range 10 {
		status, page, _ := alice.do("GET", path, nil)
		if status != 200 {
			t.Fatalf("%s: %d %v", path, status, page)
		}
		events := auditEvents(t, page)
		for _, e := range events {
			seq, _ := e["seq"].(float64)
			seqs = append(seqs, seq)
		}
		next, more := page["next_before"].(float64)
		if !more {
			if len(events) >= 2 {
				t.Fatalf("a full page has a next one: %v", page)
			}
			break
		}
		if len(events) != 2 || next != seqs[len(seqs)-1] {
			t.Fatalf("page: %v", page)
		}
		path = fmt.Sprintf("/api/v1/audit?limit=2&before=%d", int64(next))
	}
	// login + three invitations, at least; each page older than the last.
	if len(seqs) < 4 || !slices.IsSortedFunc(seqs, func(a, b float64) int { return int(b - a) }) {
		t.Fatalf("seqs: %v", seqs)
	}
	if len(slices.Compact(slices.Clone(seqs))) != len(seqs) {
		t.Fatalf("pages overlap: %v", seqs)
	}
}

func auditEvents(t *testing.T, page map[string]any) []map[string]any {
	t.Helper()
	raw, ok := page["events"].([]any)
	if !ok {
		t.Fatalf("events: %v", page)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		m, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("event: %v", e)
		}
		out = append(out, m)
	}
	return out
}
