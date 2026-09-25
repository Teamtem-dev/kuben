package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/scan"
	api "github.com/Teamtem-dev/kuben/internal/httpapi"
	"github.com/Teamtem-dev/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/internal/store"
)

// exceptionBody is routes/vulnerabilities.rs tests body().
func exceptionBody(vulnerability string, days int32) gen.CreateException {
	return gen.CreateException{
		Vulnerability: vulnerability,
		Reason:        "not reachable from the network",
		Owner:         "platform@example.com",
		Days:          days,
	}
}

// routes/vulnerabilities.rs exceptions_are_bounded.
func TestExceptionsAreBounded(t *testing.T) {
	for _, ok := range []gen.CreateException{exceptionBody("CVE-2026-12345", 30), exceptionBody("GHSA-abcd-1234-efgh", 90)} {
		if err := api.CheckException(&ok); err != nil {
			t.Errorf("%s: %v", ok.Vulnerability, err)
		}
	}
	ownerless := exceptionBody("CVE-1", 1)
	ownerless.Owner = "  "
	cases := []struct {
		body gen.CreateException
		want string
	}{
		{exceptionBody("", 1), "validation failed: `` is not a vulnerability id"},
		{exceptionBody("-CVE", 1), "validation failed: `-CVE` is not a vulnerability id"},
		{exceptionBody("CVE 1", 1), "validation failed: `CVE 1` is not a vulnerability id"},
		{exceptionBody("CVE-1", 0), "validation failed: an exception lasts 1 to 90 days"},
		{exceptionBody("CVE-1", 91), "validation failed: an exception lasts 1 to 90 days"},
		{ownerless, "validation failed: owner must be 1 to 256 printable characters"},
		// Beyond the Rust cases: the other limits and their texts.
		{exceptionBody(strings.Repeat("a", 65), 1), "validation failed: `" + strings.Repeat("a", 65) + "` is not a vulnerability id"},
		{func() gen.CreateException {
			b := exceptionBody("CVE-1", 1)
			b.Reason = "line\nbreak"
			return b
		}(), "validation failed: reason must be 1 to 1024 printable characters"},
		{func() gen.CreateException {
			b := exceptionBody("CVE-1", 1)
			b.Owner = strings.Repeat("é", 257)
			return b
		}(), "validation failed: owner must be 1 to 256 printable characters"},
	}
	for _, c := range cases {
		if err := api.CheckException(&c.body); err == nil || err.Error() != c.want {
			t.Errorf("%+v: %v, want %s", c.body, err, c.want)
		}
	}
	// The id is judged trimmed; 256 characters (not bytes) are fine.
	padded := exceptionBody("  CVE-1  ", 1)
	padded.Owner = strings.Repeat("é", 256)
	if err := api.CheckException(&padded); err != nil {
		t.Errorf("padded: %v", err)
	}
}

// ExceptionDto::of: active while unrevoked and unexpired; absent values
// are null.
func TestExceptionsSayWhetherTheyAreInForce(t *testing.T) {
	e := store.VulnException{
		ID: uuid.Nil, Vulnerability: "CVE-1", Reason: "r", Owner: "o", CreatedBy: "user:x",
		CreatedAt: 0, ExpiresAt: 2_000,
	}
	dto := api.ExceptionDtoOf(e, 1_000)
	if !dto.Active || !dto.Project.IsNull() || !dto.RevokedAt.IsNull() {
		t.Errorf("in force: %+v", dto)
	}
	if dto.CreatedAt != "1970-01-01T00:00:00Z" || dto.ExpiresAt != "1970-01-01T00:00:02Z" {
		t.Errorf("times: %s %s", dto.CreatedAt, dto.ExpiresAt)
	}
	if api.ExceptionDtoOf(e, 2_000).Active {
		t.Error("an expired exception is in force")
	}
	e.RevokedAt = opt.Some[int64](1_500)
	revoked := api.ExceptionDtoOf(e, 1_000)
	if revoked.Active || revoked.RevokedAt.Or("") != "1970-01-01T00:00:01.5Z" {
		t.Errorf("revoked: %+v", revoked)
	}
}

// tests/http.rs m4_the_scan_gate_refuses_known_critical_findings.
func TestM4TheScanGateRefusesKnownCriticalFindings(t *testing.T) {
	f := newFixture(t)
	f.sqlApp()
	alice := f.signIn("alice@example.com", seedPassword)
	bob := f.signIn("bob@example.com", seedPassword)
	gate := map[string]any{
		"requiredApprovals": 0, "deployRole": "developer", "approveRole": "admin",
		"scan": map[string]any{"mode": "block", "severity": "critical"},
	}
	status, policy, _ := alice.do("PUT", policyURL, gate)
	if status != http.StatusOK {
		t.Fatalf("policy: %d %v", status, policy)
	}
	if scanned, _ := policy["scan"].(map[string]any); scanned["mode"] != "block" {
		t.Fatalf("policy: %v", policy)
	}
	f.withTenant(func(ctx context.Context, tn *store.Tenant, _ store.Project) {
		_, err := tn.RecordScan(ctx, store.NewScan{
			Repository: "ghcr.io/acme/api",
			Digest:     deployDigest,
			Summary: scan.Summary{
				Status:    scan.StatusOK,
				Scanner:   "trivy 0.74.0",
				Counts:    scan.Counts{Critical: 1},
				Findings:  []scan.Finding{{Severity: scan.SeverityCritical, ID: "CVE-2026-7"}},
				ScannedAt: time.Now().UnixMilli(),
			},
		})
		if err != nil {
			t.Fatal(err)
		}
	})
	status, refused, _ := alice.deploy(0, "")
	if text, _ := json.Marshal(refused); status != http.StatusConflict || !strings.Contains(string(text), "CVE-2026-7") {
		t.Fatalf("a deploy with a known critical finding: %d %s", status, text)
	}

	const exceptions = "/api/v1/vulnerability-exceptions"
	grant := map[string]any{
		"vulnerability": "CVE-2026-7", "reason": "not reachable", "owner": "platform", "days": 30, "project": "shop",
	}
	if status := bob.status("POST", exceptions, grant); status != http.StatusForbidden {
		t.Fatalf("a viewer granted an exception: %d", status)
	}
	status, granted, _ := alice.do("POST", exceptions, grant)
	if status != http.StatusCreated || granted["active"] != true {
		t.Fatalf("grant: %d %v", status, granted)
	}
	if status, run, _ := alice.deploy(0, ""); status != http.StatusAccepted {
		t.Fatalf("a deploy with the finding excepted: %d %v", status, run)
	}

	status, scans, _ := bob.do("GET", prodPath+"/apps/api/scans", nil)
	if status != http.StatusOK || scans["gate"] != "pass" {
		t.Fatalf("scans: %d %v", status, scans)
	}
	images, _ := scans["images"].([]any)
	if len(images) == 0 {
		t.Fatalf("scans: %v", scans)
	}
	first, _ := images[0].(map[string]any)
	newest, _ := first["scan"].(map[string]any)
	if newest["critical"] != float64(1) || first["sbom"] != false {
		t.Fatalf("scans: %v", scans)
	}
	id, _ := granted["id"].(string)
	revoke := exceptions + "/" + id
	if status := alice.status("DELETE", revoke, nil); status != http.StatusNoContent {
		t.Fatalf("revoke: %d", status)
	}
	if status := alice.status("DELETE", revoke, nil); status != http.StatusNotFound {
		t.Fatalf("revoke twice: %d", status)
	}
	if status, listed := bob.list(exceptions); status != http.StatusOK || len(listed) != 0 {
		t.Fatalf("revoked exceptions are not in force: %d %v", status, listed)
	}
}
