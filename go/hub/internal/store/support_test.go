package store_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store"
	"github.com/Teamtem-dev/kuben/go/hub/internal/store/pgtest"
)

// Ported from support.rs.

func TestTheSummaryCountsAcrossOrganizations(t *testing.T) {
	s := pgtest.Store(t)
	ctx := t.Context()
	for _, slug := range []string{"a", "b"} {
		tn := tenant(t, s, org(t, s, slug, slug))
		must[ids.ProjectID](t, "project")(tn.CreateProject(ctx, "shop", "Shop"))
		openIncident(t, tn, "k")
		commit(t, tn)
	}
	summary := must[store.SupportSummary](t, "summary")(s.SupportSummary(ctx))
	if summary.Organizations != 2 || summary.Projects != 2 {
		t.Fatalf("summary: %+v", summary)
	}
	if diff := cmp.Diff([]store.IncidentCount{{Kind: "backup.stale", Severity: "critical", N: 2}},
		summary.OpenIncidents); diff != "" {
		t.Fatalf("open incidents (-want +got):\n%s", diff)
	}
	if summary.OutboxPending != 0 {
		t.Fatalf("outbox: %d", summary.OutboxPending)
	}
	text, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	if strings.Contains(string(text), "shop") {
		t.Fatalf("no names: %s", text)
	}
	if !strings.Contains(string(text), `"open_incidents":[["backup.stale","critical",2]]`) {
		t.Fatalf("Rust's tuples: %s", text)
	}
}
