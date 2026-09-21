package store

// Image scans and the scan gate (M4.6, migration 0024); a partial port of
// repo/scans.rs: [Tenant.ScanVerdict] applies [scan.Evaluate] to a release
// on a target (the environment's gate, the newest scan of every digest of
// the release, and the unexpired exceptions of the project), which a
// deployment checks. Recording scans, SBOMs and exceptions follows with the
// scan work.

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/scan"
)

const (
	latestScans = "SELECT DISTINCT ON (digest) digest, status, scanner, db_updated_at, critical, high, " +
		"medium, low, unknown, findings, scanned_at FROM artifact_scans " +
		"WHERE org_id = $1 AND digest = ANY($2) ORDER BY digest, scanned_at DESC"
	exceptedVulnerabilities = "SELECT DISTINCT vulnerability FROM vulnerability_exceptions " +
		"WHERE org_id = $1 AND (project_id IS NULL OR project_id = $2) " +
		"AND revoked_at IS NULL AND expires_at > $3"
	targetRelease = "SELECT t.project_id, r.artifacts::text FROM application_targets t " +
		"JOIN releases r ON r.id = $2 AND r.org_id = t.org_id AND r.application_id = t.application_id " +
		"WHERE t.id = $1 AND t.org_id = $3"
)

// scanCount reads a finding count; a negative one is a decode error.
func scanCount(n int32) (uint32, error) {
	if n < 0 {
		return 0, decodeErr("read a scan", errOutOfRange)
	}
	return uint32(n), nil //nolint:gosec // checked above
}

// scannedRow is a digest and its newest scan.
type scannedRow struct {
	digest  string
	summary scan.Summary
}

func scanSummary(row pgx.CollectableRow) (scannedRow, error) {
	const op = "read a scan"
	var (
		digest, status, scanner               string
		dbUpdatedAt                           *int64
		critical, high, medium, low, unknowns int32
		findings                              []string
		scannedAt                             int64
	)
	if err := row.Scan(&digest, &status, &scanner, &dbUpdatedAt, &critical, &high, &medium, &low, &unknowns,
		&findings, &scannedAt); err != nil {
		return scannedRow{}, err
	}
	s := scan.Summary{Scanner: scanner, DBUpdatedAt: opt.FromPtr(dbUpdatedAt), ScannedAt: scannedAt}
	switch status {
	case string(scan.StatusOK):
		s.Status = scan.StatusOK
	case string(scan.StatusUnavailable):
		s.Status = scan.StatusUnavailable
	default:
		return scannedRow{}, decodeErr(op, "unknown scan status %s", rustQuote(status))
	}
	var err error
	for _, c := range []struct {
		n   int32
		out *uint32
	}{
		{critical, &s.Counts.Critical},
		{high, &s.Counts.High},
		{medium, &s.Counts.Medium},
		{low, &s.Counts.Low},
		{unknowns, &s.Counts.Unknown},
	} {
		if *c.out, err = scanCount(c.n); err != nil {
			return scannedRow{}, err
		}
	}
	// `SEVERITY:ID`; what does not read as one is left out, as in Rust.
	for _, f := range findings {
		severity, id, ok := strings.Cut(f, ":")
		if !ok {
			continue
		}
		parsed, err := scan.ParseSeverity(severity)
		if err != nil {
			continue
		}
		s.Findings = append(s.Findings, scan.Finding{Severity: parsed, ID: id})
	}
	return scannedRow{digest: digest, summary: s}, nil
}

// digestsOf is the distinct digests of a release's `artifacts` (process →
// digest), in order.
func digestsOf(artifacts string) ([]string, error) {
	var byProcess map[string]string
	if err := json.Unmarshal([]byte(artifacts), &byProcess); err != nil {
		return nil, decodeErr("judge a release", "%v", err)
	}
	digests := make([]string, 0, len(byProcess))
	for _, d := range byProcess {
		digests = append(digests, d)
	}
	slices.Sort(digests)
	return slices.Compact(digests), nil
}

// LatestScans is the newest scan of each of digests that has one.
func (t *Tenant) LatestScans(ctx context.Context, digests []string) (map[string]scan.Summary, error) {
	if digests == nil {
		digests = []string{}
	}
	rows, err := queryAll(ctx, t.tx, "read the latest scans", latestScans, scanSummary, t.org.String(), digests)
	if err != nil {
		return nil, err
	}
	out := make(map[string]scan.Summary, len(rows))
	for _, r := range rows {
		out[r.digest] = r.summary
	}
	return out, nil
}

// ScanVerdict is what the scan gate of tgt's environment says about release
// at now; false when the target or release is not the organization's.
func (t *Tenant) ScanVerdict(ctx context.Context, tgt ids.TargetID, release ids.ReleaseID, now int64) (scan.Verdict, bool, error) {
	const op = "judge a release"
	org := t.org.String()
	type targetRow struct {
		project   ids.ProjectID
		artifacts string
	}
	row, ok, err := queryOpt(ctx, t.tx, op, targetRelease, func(row pgx.CollectableRow) (targetRow, error) {
		var r targetRow
		err := row.Scan(&r.project, &r.artifacts)
		return r, err
	}, tgt, release, org)
	if err != nil || !ok {
		return nil, false, err
	}
	revision, ok, err := t.PolicyOfTarget(ctx, tgt)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return scan.Pass{}, true, nil
	}
	digests, err := digestsOf(row.artifacts)
	if err != nil {
		return nil, false, err
	}
	latest, err := t.LatestScans(ctx, digests)
	if err != nil {
		return nil, false, err
	}
	vulnerabilities, err := queryAll(ctx, t.tx, op, exceptedVulnerabilities, pgx.RowTo[string], org, row.project, now)
	if err != nil {
		return nil, false, err
	}
	excepted := make(map[string]struct{}, len(vulnerabilities))
	for _, v := range vulnerabilities {
		excepted[v] = struct{}{}
	}
	scans := make([]scan.Scanned, 0, len(digests))
	for _, d := range digests {
		s, found := latest[d]
		image := scan.Scanned{Digest: d}
		if found {
			image.Scan = opt.Some(s)
		}
		scans = append(scans, image)
	}
	return scan.Evaluate(revision.Policy.Scan, scans, excepted, now), true, nil
}
