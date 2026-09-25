package httpapi

// The vulnerability state of an app's current release (M4.6): the newest
// scan of each image, what the environment's gate says about it, and the
// images' SBOMs (routes/apps/scans.rs).

import (
	"context"
	"math"
	"slices"
	"strings"

	"github.com/Teamtem-dev/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/internal/core/scan"
	"github.com/Teamtem-dev/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/internal/httpapi/httpx"
)

// scanCount is a u32 count in the contract's int32.
func scanCount(n uint32) int32 {
	return int32(min(n, math.MaxInt32)) //nolint:gosec // bounded
}

// scanDto is ScanDto::from: `databaseUpdatedAt` is null when unknown.
func scanDto(s scan.Summary) gen.ScanDto {
	updated := opt.None[string]()
	if at, ok := s.DBUpdatedAt.Get(); ok {
		updated = opt.Some(Timestamp(at))
	}
	findings := make([]string, 0, len(s.Findings))
	for _, f := range s.Findings {
		findings = append(findings, string(f.Severity)+":"+f.ID)
	}
	return gen.ScanDto{
		Status:            string(s.Status),
		Scanner:           s.Scanner,
		DatabaseUpdatedAt: optNilString(updated),
		ScannedAt:         Timestamp(s.ScannedAt),
		Critical:          scanCount(s.Counts.Critical),
		High:              scanCount(s.Counts.High),
		Medium:            scanCount(s.Counts.Medium),
		Low:               scanCount(s.Counts.Low),
		Unknown:           scanCount(s.Counts.Unknown),
		Findings:          findings,
	}
}

// gateOf is the gate's word and reasons for verdict.
func gateOf(verdict scan.Verdict) (string, []string) {
	switch v := verdict.(type) {
	case scan.Pass:
		return "pass", []string{}
	case scan.Warn:
		return "warn", nonNil(v.Reasons)
	case scan.Block:
		return "block", nonNil(v.Reasons)
	}
	return "pass", []string{}
}

// appScansDto is the answer for digests of release: each image with its
// newest scan (null when never scanned) and whether an SBOM is kept.
func appScansDto(
	release gen.OptNilUUID, verdict scan.Verdict, digests []string,
	latest map[string]scan.Summary, sboms map[string]struct{},
) gen.AppScansDto {
	gate, reasons := gateOf(verdict)
	images := make([]gen.ImageScanDto, 0, len(digests))
	for _, d := range digests {
		image := gen.ImageScanDto{Digest: d}
		if s, ok := latest[d]; ok {
			image.Scan = gen.NewOptNilScanDto(scanDto(s))
		} else {
			image.Scan.SetToNull()
		}
		_, image.Sbom = sboms[d]
		images = append(images, image)
	}
	return gen.AppScansDto{Release: release, Gate: gate, Reasons: reasons, Images: images}
}

// GetAppScans is the scans of the app's current release and the gate's
// verdict.
func (s *Server) GetAppScans(ctx context.Context, params gen.GetAppScansParams) (gen.GetAppScansRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	app, err := s.findApp(ctx, a, params.Project, params.Environment, params.App)
	if err != nil {
		return nil, err
	}
	if _, err := a.Require(perm.AppRead, app.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	release, ok := app.app.Release.Get()
	if !ok {
		dto := appScansDto(nullUUID(), scan.Pass{}, nil, nil, nil)
		return &dto, nil
	}
	t, err := s.deps.Store.Tenant(ctx, app.env.project.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	digests, _, err := t.ReleaseDigests(ctx, release)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	latest, err := t.LatestScans(ctx, digests)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	sboms, err := t.SbomsKept(ctx, digests)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	verdict, found, err := t.ScanVerdict(ctx, app.app.Target, release, s.deps.Clock.NowMs())
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found {
		verdict = scan.Pass{}
	}
	dto := appScansDto(gen.NewOptNilUUID(release.UUID()), verdict, digests, latest, sboms)
	return &dto, nil
}

// sbomFile is the Content-Disposition of digest's SBOM for app.
func sbomFile(app, digest string) string {
	return `attachment; filename="` + app + "-" + strings.ReplaceAll(digest, ":", "-") + `.cdx.json"`
}

// GetAppSbom is the CycloneDX SBOM of one image of the app's current
// release (gzip-encoded JSON).
func (s *Server) GetAppSbom(ctx context.Context, params gen.GetAppSbomParams) (gen.GetAppSbomRes, error) {
	a, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	app, err := s.findApp(ctx, a, params.Project, params.Environment, params.App)
	if err != nil {
		return nil, err
	}
	if _, err := a.Require(perm.AppRead, app.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	missing := kerrors.New(kerrors.NotFound, "an SBOM of `%s` for app `%s`", params.Digest, params.App)
	release, ok := app.app.Release.Get()
	if !ok {
		return nil, missing
	}
	t, err := s.deps.Store.Tenant(ctx, app.env.project.org)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	digests, found, err := t.ReleaseDigests(ctx, release)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found || !slices.Contains(digests, params.Digest) {
		return nil, missing
	}
	content, found, err := t.Sbom(ctx, params.Digest)
	if err != nil {
		return nil, err //nolint:wrapcheck // a store error, answered as internal
	}
	if !found {
		return nil, missing
	}
	// The generated server writes the Content-Type and the bytes as they
	// are; the other two headers go straight into the response's header
	// map, which every writer on the way shares, so the compressor sees the
	// body is gzip already.
	if w, ok := httpx.ResponseWriterFrom(ctx); ok {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Disposition", sbomFile(params.App, params.Digest))
	}
	body := gen.GetAppSbomOKApplicationVndCyclonedxJSON(content)
	return &body, nil
}
