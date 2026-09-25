package api_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/go-faster/jx"
	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/scan"
	api "github.com/Teamtem-dev/kuben/internal/httpapi"
	"github.com/Teamtem-dev/kuben/internal/httpapi/gen"
)

// jsonOf is v as generic JSON.
func jsonOf(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// ScanDto::from and AppScansDto: the members and nulls serde wrote.
func TestScansShowTheNewestScanAndTheGate(t *testing.T) {
	summary := scan.Summary{
		Status:  scan.StatusOK,
		Scanner: "trivy 0.74.0",
		Counts:  scan.Counts{Critical: 1, High: 2},
		Findings: []scan.Finding{
			{Severity: scan.SeverityCritical, ID: "CVE-2026-7"},
			{Severity: scan.SeverityHigh, ID: "GHSA-abcd-1234-efgh"},
		},
		ScannedAt: 1_500,
	}
	release := uuid.MustParse("0192f3a1-0000-7000-8000-000000000001")
	dto := api.AppScansDtoOf(gen.NewOptNilUUID(release), scan.Block{Reasons: []string{"CVE-2026-7"}},
		[]string{"sha256:a", "sha256:b"}, map[string]scan.Summary{"sha256:a": summary},
		map[string]struct{}{"sha256:b": {}})
	want := map[string]any{
		"release": release.String(),
		"gate":    "block",
		"reasons": []any{"CVE-2026-7"},
		"images": []any{
			map[string]any{
				"digest": "sha256:a",
				"sbom":   false,
				"scan": map[string]any{
					"status": "ok", "scanner": "trivy 0.74.0", "databaseUpdatedAt": nil,
					"scannedAt": "1970-01-01T00:00:01.5Z",
					"critical":  float64(1), "high": float64(2), "medium": float64(0), "low": float64(0), "unknown": float64(0),
					"findings": []any{"critical:CVE-2026-7", "high:GHSA-abcd-1234-efgh"},
				},
			},
			map[string]any{"digest": "sha256:b", "sbom": true, "scan": nil},
		},
	}
	if diff := cmp.Diff(want, jsonOf(t, &dto)); diff != "" {
		t.Errorf("scans (-want +got):\n%s", diff)
	}

	summary.DBUpdatedAt = opt.Some[int64](0)
	if s := api.ScanDtoOf(summary); s.DatabaseUpdatedAt.Or("") != "1970-01-01T00:00:00Z" {
		t.Errorf("database time: %v", s.DatabaseUpdatedAt)
	}
}

// An app without a release passes, with nothing to show.
func TestAnAppWithoutAReleaseHasNoScans(t *testing.T) {
	var none gen.OptNilUUID
	none.SetToNull()
	dto := api.AppScansDtoOf(none, scan.Pass{}, nil, nil, nil)
	want := map[string]any{"release": nil, "gate": "pass", "reasons": []any{}, "images": []any{}}
	if diff := cmp.Diff(want, jsonOf(t, &dto)); diff != "" {
		t.Errorf("no release (-want +got):\n%s", diff)
	}
	warn := api.AppScansDtoOf(none, scan.Warn{}, nil, nil, nil)
	if warn.Gate != "warn" || warn.Reasons == nil {
		t.Errorf("warn: %+v", warn)
	}
}

// The SBOM goes out as stored (gzip bytes, not JSON text) under the name
// Rust gave it.
func TestTheSbomIsSentAsStored(t *testing.T) {
	if got := api.SbomFile("api", "sha256:abc"); got != `attachment; filename="api-sha256-abc.cdx.json"` {
		t.Errorf("file: %s", got)
	}
	gz := []byte{0x1f, 0x8b, 0x08, 0x00, 0xff, 0x00, '"', ','}
	body := gen.GetAppSbomOKApplicationVndCyclonedxJSON(gz)
	e := new(jx.Encoder)
	body.Encode(e)
	if !bytes.Equal(e.Bytes(), gz) {
		t.Errorf("encoded %x, want %x", e.Bytes(), gz)
	}
}
