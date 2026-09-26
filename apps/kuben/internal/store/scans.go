package store

// Image scans, SBOMs, vulnerability exceptions and the scan gate (M4.6,
// migration 0024); the port of repo/scans.rs. [Tenant.ScanVerdict] applies
// [scan.Evaluate] to a release on a target: the environment's gate, the
// newest scan of every digest of the release, and the unexpired exceptions
// of the project.

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ids"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/scan"
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

const (
	insertScan = "INSERT INTO artifact_scans " +
		"(id, org_id, repository, digest, status, scanner, db_updated_at, critical, high, medium, low, unknown, " +
		"findings, detail, build_attempt_id, scanned_at) " +
		"VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)"
	scanHistory = "SELECT digest, status, scanner, db_updated_at, critical, high, medium, low, unknown, " +
		"findings, scanned_at FROM artifact_scans " +
		"WHERE org_id = $1 AND digest = $2 ORDER BY scanned_at DESC LIMIT $3"
	insertSbom = "INSERT INTO artifact_sboms (org_id, digest, format, content, created_at) " +
		"VALUES ($1, $2, 'cyclonedx+json', $3, $4) ON CONFLICT (org_id, digest) DO NOTHING"
	sbomsKept       = "SELECT digest FROM artifact_sboms WHERE org_id = $1 AND digest = ANY($2)"
	selectSbom      = "SELECT content FROM artifact_sboms WHERE org_id = $1 AND digest = $2"
	insertException = "INSERT INTO vulnerability_exceptions " +
		"(id, org_id, project_id, vulnerability, reason, owner, created_by, created_at, expires_at) " +
		"VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)"
	selectExceptions = "SELECT id, project_id, vulnerability, reason, owner, created_by, created_at, " +
		"expires_at, revoked_at, revoked_by FROM vulnerability_exceptions " +
		"WHERE org_id = $1 AND ($2 OR (revoked_at IS NULL AND expires_at > $3)) " +
		"ORDER BY created_at DESC LIMIT 500"
	revokeException = "UPDATE vulnerability_exceptions SET revoked_at = $3, revoked_by = $4 " +
		"WHERE id = $1 AND org_id = $2 AND revoked_at IS NULL"
	releaseDigests = "SELECT artifacts::text FROM releases WHERE id = $1 AND org_id = $2"
	// rescanDue is the images live targets run (their newest successful
	// run's release) whose newest scan is older than $2.
	rescanDue = "SELECT x.repository, x.digest FROM ( " +
		"SELECT DISTINCT rel.source ->> 'image_repository' AS repository, a.value AS digest " +
		"FROM application_targets t " +
		"JOIN LATERAL (SELECT d.release_id FROM deployment_runs d " +
		"WHERE d.target_id = t.id AND d.org_id = t.org_id AND d.phase = 'succeeded' " +
		"ORDER BY d.generation DESC LIMIT 1) r ON TRUE " +
		"JOIN releases rel ON rel.id = r.release_id AND rel.org_id = t.org_id " +
		"CROSS JOIN LATERAL jsonb_each_text(rel.artifacts) AS a (key, value) " +
		"WHERE t.org_id = $1 AND NOT t.deleting AND rel.source ->> 'image_repository' IS NOT NULL) x " +
		"WHERE NOT EXISTS (SELECT 1 FROM artifact_scans s " +
		"WHERE s.org_id = $1 AND s.digest = x.digest AND s.scanned_at > $2) " +
		"ORDER BY x.digest LIMIT $3"
)

// NewScan is a scan to record.
type NewScan struct {
	Repository   string
	Digest       string
	Summary      scan.Summary
	Detail       opt.Val[string]
	BuildAttempt opt.Val[ids.BuildAttemptID]
}

// NewException is an exception to grant.
type NewException struct {
	// Project is the project it covers; every project when absent.
	Project       opt.Val[ids.ProjectID]
	Vulnerability string
	Reason        string
	Owner         string
	CreatedBy     string
	ExpiresAt     int64
}

// VulnException is a granted exception.
type VulnException struct {
	ID            uuid.UUID
	Project       opt.Val[ids.ProjectID]
	Vulnerability string
	Reason        string
	Owner         string
	CreatedBy     string
	CreatedAt     int64
	ExpiresAt     int64
	RevokedAt     opt.Val[int64]
	RevokedBy     opt.Val[string]
}

// ImageRef is a pushed image: its repository and digest.
type ImageRef struct {
	Repository string
	Digest     string
}

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

// RecordScan records a scan; at most [scan.MaxListed] findings are kept by
// id.
func (t *Tenant) RecordScan(ctx context.Context, n NewScan) (uuid.UUID, error) {
	const op = "record a scan"
	id := uuid.Must(uuid.NewV7())
	s := n.Summary
	listed := s.Findings[:min(len(s.Findings), scan.MaxListed)]
	findings := make([]string, 0, len(listed))
	for _, f := range listed {
		findings = append(findings, f.Severity.String()+":"+f.ID)
	}
	counts := make([]int32, 0, 5)
	for _, c := range []uint32{s.Counts.Critical, s.Counts.High, s.Counts.Medium, s.Counts.Low, s.Counts.Unknown} {
		v, err := int4(op, c)
		if err != nil {
			return uuid.UUID{}, err
		}
		counts = append(counts, v)
	}
	if _, err := exec(ctx, t.tx, op, insertScan, id, t.org.String(), n.Repository, n.Digest, s.Status.String(),
		s.Scanner, s.DBUpdatedAt.Ptr(), counts[0], counts[1], counts[2], counts[3], counts[4], findings,
		n.Detail.Ptr(), n.BuildAttempt.Ptr(), s.ScannedAt); err != nil {
		return uuid.UUID{}, err
	}
	return id, nil
}

// ScanHistory is the newest limit scans of digest.
func (t *Tenant) ScanHistory(ctx context.Context, digest string, limit int64) ([]scan.Summary, error) {
	rows, err := queryAll(ctx, t.tx, "read a scan history", scanHistory, scanSummary, t.org.String(), digest, limit)
	if err != nil {
		return nil, err
	}
	out := make([]scan.Summary, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.summary)
	}
	return out, nil
}

// PutSbom keeps the gzip content of digest's SBOM, unless one is kept
// already.
func (t *Tenant) PutSbom(ctx context.Context, digest string, content []byte) (bool, error) {
	n, err := exec(ctx, t.tx, "keep an SBOM", insertSbom, t.org.String(), digest, content, t.store.now())
	return n == 1, err
}

// SbomsKept is which of digests have an SBOM.
func (t *Tenant) SbomsKept(ctx context.Context, digests []string) (map[string]struct{}, error) {
	if digests == nil {
		digests = []string{}
	}
	kept, err := queryAll(ctx, t.tx, "read the kept SBOMs", sbomsKept, pgx.RowTo[string], t.org.String(), digests)
	if err != nil {
		return nil, err
	}
	out := make(map[string]struct{}, len(kept))
	for _, d := range kept {
		out[d] = struct{}{}
	}
	return out, nil
}

// Sbom is the gzip SBOM of digest.
func (t *Tenant) Sbom(ctx context.Context, digest string) ([]byte, bool, error) {
	return queryOpt(ctx, t.tx, "read an SBOM", selectSbom, pgx.RowTo[[]byte], t.org.String(), digest)
}

// CreateException grants an exception.
func (t *Tenant) CreateException(ctx context.Context, e NewException) (uuid.UUID, error) {
	id := uuid.Must(uuid.NewV7())
	if _, err := exec(ctx, t.tx, "grant an exception", insertException, id, t.org.String(), e.Project.Ptr(),
		e.Vulnerability, e.Reason, e.Owner, e.CreatedBy, t.store.now(), e.ExpiresAt); err != nil {
		return uuid.UUID{}, err
	}
	return id, nil
}

func scanException(row pgx.CollectableRow) (VulnException, error) {
	var (
		e         VulnException
		project   *uuid.UUID
		revokedAt *int64
		revokedBy *string
	)
	if err := row.Scan(&e.ID, &project, &e.Vulnerability, &e.Reason, &e.Owner, &e.CreatedBy, &e.CreatedAt,
		&e.ExpiresAt, &revokedAt, &revokedBy); err != nil {
		return VulnException{}, err
	}
	if project != nil {
		e.Project = opt.Some(ids.From[ids.Project](*project))
	}
	e.RevokedAt = opt.FromPtr(revokedAt)
	e.RevokedBy = opt.FromPtr(revokedBy)
	return e, nil
}

// Exceptions is the exceptions in force, or every one when all (the newest
// 500).
func (t *Tenant) Exceptions(ctx context.Context, all bool) ([]VulnException, error) {
	return queryAll(ctx, t.tx, "list exceptions", selectExceptions, scanException, t.org.String(), all, t.store.now())
}

// RevokeException revokes exception id. False when there is no such
// unrevoked one.
func (t *Tenant) RevokeException(ctx context.Context, id uuid.UUID, by string) (bool, error) {
	n, err := exec(ctx, t.tx, "revoke an exception", revokeException, id, t.org.String(), t.store.now(), by)
	return n == 1, err
}

// RescanDue is up to limit images of live targets whose newest scan is
// older than before (unix ms).
func (t *Tenant) RescanDue(ctx context.Context, before, limit int64) ([]ImageRef, error) {
	return queryAll(ctx, t.tx, "read the images due for a rescan", rescanDue,
		func(row pgx.CollectableRow) (ImageRef, error) {
			var r ImageRef
			err := row.Scan(&r.Repository, &r.Digest)
			return r, err
		}, t.org.String(), before, limit)
}

// ReleaseDigests is the distinct digests of release, if the organization
// has it.
func (t *Tenant) ReleaseDigests(ctx context.Context, release ids.ReleaseID) ([]string, bool, error) {
	artifacts, ok, err := queryOpt(ctx, t.tx, "read a release's digests", releaseDigests, pgx.RowTo[string],
		release, t.org.String())
	if err != nil || !ok {
		return nil, false, err
	}
	digests, err := digestsOf(artifacts)
	if err != nil {
		return nil, false, err
	}
	return digests, true, nil
}
