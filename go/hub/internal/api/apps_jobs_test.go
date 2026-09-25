package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Teamtem-dev/kuben/go/hub/internal/api"
)

// routes/apps/jobs.rs manual_job_names_fit_the_limit.
func TestManualJobNamesFitTheLimit(t *testing.T) {
	name := api.ManualJobName(strings.Repeat("x", 80), 1_757_548_800)
	if len(name) > 63 || !strings.HasSuffix(name, "-run-1757548800") {
		t.Fatalf("name: len=%d, name=%s", len(name), name)
	}
}

// Without a Kubernetes cluster configured, runApp answers 503 once the
// process is known (an app without a scheduled process is refused first,
// as in Rust).
func TestRunAppWithoutClusterAnswers503(t *testing.T) {
	f := newFixture(t)
	f.seedApp()
	alice := f.signIn("alice@example.com", seedPassword)
	status, body, _ := alice.do("POST", "/api/v1/projects/shop/environments/prod/apps/api/run", map[string]any{})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 without a scheduled process, got %d: %v", status, body)
	}
	status, body, _ = alice.do("POST", "/api/v1/projects/shop/environments/prod/apps/api/run", map[string]any{"process": "web"})
	if status != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 without cluster, got %d: %v", status, body)
	}
}
