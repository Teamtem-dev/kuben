package metrics_test

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/metrics"
)

// The names and labels are the contract dashboards read.
func TestTheExpositionCarriesRustsMetricNames(t *testing.T) {
	m := metrics.New()
	m.SubsystemFailed("builds")
	m.SubsystemPanicked("notify")
	m.ReconcileFailed("App")
	m.Leading(true)
	m.SSELagged(3)
	m.AuditWriteFailed()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`kuben_subsystem_failures_total{subsystem="builds"} 1`,
		`kuben_subsystem_panics_total{subsystem="notify"} 1`,
		`kuben_reconcile_errors_total{kind="App"} 1`,
		"kuben_leader 1",
		"kuben_sse_lagged_total 3",
		"kuben_audit_write_errors_total 1",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
}
