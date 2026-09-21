package api_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
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
		if got := api.AuditPageSize(c.limit); got != c.want {
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
		{opt.Some[any](map[string]any{"status": float64(403)}), opt.Some[int32](403)},
		{opt.Some[any](map[string]any{"status": float64(70000)}), opt.None[int32]()},
		{opt.Some[any](map[string]any{"status": float64(-1)}), opt.None[int32]()},
		{opt.Some[any](map[string]any{"status": 4.5}), opt.None[int32]()},
		{opt.Some[any](map[string]any{"status": "403"}), opt.None[int32]()},
		{opt.Some[any](map[string]any{}), opt.None[int32]()},
		{opt.Some[any]([]any{float64(403)}), opt.None[int32]()},
	}
	for _, c := range cases {
		if got := api.AuditStatus(c.data); got != c.want {
			t.Errorf("%v: %v, want %v", c.data, got, c.want)
		}
	}
}

type auditEvent struct{ action, outcome string }

// tests/http.rs scenario2_every_mutation_is_audited_without_handler_code.
// createProject is not ported yet; member changes stand in for it: a
// denied role change by bob and an invitation by alice.
func TestScenario2EveryMutationIsAuditedWithoutHandlerCode(t *testing.T) {
	f := newFixture(t)
	alice := f.signIn("alice@example.com", seedPassword)
	bob := f.signIn("bob@example.com", seedPassword)
	_, me, _ := alice.do("GET", "/api/v1/me", nil)
	aliceID, _ := me["id"].(string)
	if status := bob.status("PATCH", "/api/v1/members/"+aliceID, map[string]any{"role": "viewer"}); status != 403 {
		t.Fatalf("bob changes a role: %d", status)
	}
	if status, _ := invite(alice, "carol@example.com", "developer"); status != 201 {
		t.Fatalf("alice invites: %d", status)
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
	for _, want := range []auditEvent{{"updateMember", "denied"}, {"inviteMember", "success"}, {"login", "success"}} {
		if !slices.Contains(seen, want) {
			t.Errorf("missing %v in %v", want, seen)
		}
	}
	i := slices.IndexFunc(events, func(e map[string]any) bool { return e["outcome"] == "denied" })
	if i < 0 {
		t.Fatal("no denied event")
	}
	denied := events[i]
	if denied["actor"] != "bob@example.com" || denied["status"] != float64(403) || denied["actor_kind"] != "session" ||
		denied["target_kind"] != "member" || denied["target"] != aliceID {
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
