package httpapi

// An app's CPU and memory (M5.5, routes/apps/metrics.rs): the last hour
// from this replica's live window, the last week from hourly rollups.
// Missing data is reported as unavailable, never as zero.

import (
	"context"
	"math"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/kerrors"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/perm"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/httpapi/gen"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/usage"
)

// GetAppMetrics is an app's CPU and memory usage.
func (s *Server) GetAppMetrics(ctx context.Context, params gen.GetAppMetricsParams) (gen.GetAppMetricsRes, error) {
	acc, err := s.access(ctx)
	if err != nil {
		return nil, err
	}
	a, err := s.findApp(ctx, acc, params.Project, params.Environment, params.App)
	if err != nil {
		return nil, err
	}
	if _, err := acc.Require(perm.AppRead, a.chain()); err != nil {
		return nil, err //nolint:wrapcheck // a kerrors already
	}
	window := params.Window.Or("1h")
	now := s.deps.Clock.NowMs()
	var (
		points []gen.MetricPoint
		reason opt.Val[string]
	)
	switch window {
	case "1h":
		points, reason = liveMetrics(s.deps.Usage, a.app.Namespace, a.app.Slug, now)
	case "7d":
		points, reason, err = s.hourlyMetrics(ctx, a, now)
		if err != nil {
			return nil, err
		}
	default:
		return nil, kerrors.New(kerrors.Validation, "window must be 1h or 7d, not `%s`", window)
	}
	return &gen.MetricsDto{
		Window:    window,
		Available: !reason.IsSome(),
		Reason:    optNilString(reason),
		Points:    points,
	}, nil
}

// liveMetrics is namespace/slug's last hour from this replica's live
// window, and why there is nothing to show when there is not.
func liveMetrics(live opt.Val[*usage.Buffer], namespace, slug string, now int64) ([]gen.MetricPoint, opt.Val[string]) {
	buffer, ok := live.Get()
	if !ok {
		return []gen.MetricPoint{}, opt.Some("usage is not collected on this server")
	}
	samples := buffer.Window(namespace, slug, now-usage.HourMs)
	points := make([]gen.MetricPoint, 0, len(samples))
	for _, x := range samples {
		points = append(points, gen.MetricPoint{
			At:          Timestamp(x.At),
			CpuMillis:   clampInt64(x.CPUMillis),
			MemoryBytes: clampInt64(x.MemoryBytes),
			CpuMax:      optNilInt64(opt.None[int64]()),
			MemoryMax:   optNilInt64(opt.None[int64]()),
			Pods:        gen.NewOptNilInt32(int32(min(x.Pods, math.MaxInt32))), //nolint:gosec // G115: bounded above
		})
	}
	if len(points) > 0 {
		return points, opt.None[string]()
	}
	return points, opt.Some(buffer.Unavailable().Or(
		"no samples yet: the app runs no pods, or they were not measured"))
}

// hourlyMetrics is the app's last week from hourly rollups.
func (s *Server) hourlyMetrics(ctx context.Context, a appScope, now int64) ([]gen.MetricPoint, opt.Val[string], error) {
	t, err := s.deps.Store.Tenant(ctx, a.env.project.org)
	if err != nil {
		return nil, opt.None[string](), err //nolint:wrapcheck // a store error, answered as internal
	}
	defer t.Rollback(ctx) //nolint:errcheck // read only
	hours, err := t.UsageSince(ctx, a.app.Target, now-7*24*usage.HourMs)
	if err != nil {
		return nil, opt.None[string](), err //nolint:wrapcheck // a store error, answered as internal
	}
	points := make([]gen.MetricPoint, 0, len(hours))
	for _, h := range hours {
		// Stored values are never negative (the table's checks); Rust read
		// them with unsigned_abs.
		points = append(points, gen.MetricPoint{
			At:          Timestamp(h.Hour),
			CpuMillis:   absInt64(h.CPUAvg),
			MemoryBytes: absInt64(h.MemoryAvg),
			CpuMax:      gen.NewOptNilInt64(absInt64(h.CPUMax)),
			MemoryMax:   gen.NewOptNilInt64(absInt64(h.MemoryMax)),
			Pods:        optNilInt32Null(),
		})
	}
	if len(points) > 0 {
		return points, opt.None[string](), nil
	}
	return points, opt.Some("no hourly usage recorded yet"), nil
}

// clampInt64 is v as the contract's int64, saturating.
func clampInt64(v uint64) int64 {
	return int64(min(v, math.MaxInt64)) //nolint:gosec // G115: bounded above
}

// absInt64 is |v|, saturating at the largest int64.
func absInt64(v int64) int64 {
	switch {
	case v >= 0:
		return v
	case v == math.MinInt64:
		return math.MaxInt64
	default:
		return -v
	}
}

func optNilInt32Null() gen.OptNilInt32 {
	var n gen.OptNilInt32
	n.SetToNull()
	return n
}
