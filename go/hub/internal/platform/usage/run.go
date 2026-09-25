package usage

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/clock"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/health"
	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

// Subsystem is the health entry and supervisor name of the collector.
const Subsystem = "usage"

// PodMetrics is the Metrics API's pod resource, read through the dynamic
// client.
func PodMetrics() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods"}
}

// Scrape reads every Kuben pod's usage once.
func Scrape(ctx context.Context, client dynamic.Interface, now int64) ([]Measured, error) {
	list, err := client.Resource(PodMetrics()).List(ctx, metav1.ListOptions{LabelSelector: v1alpha1.ManagedSelector})
	if err != nil {
		return nil, fmt.Errorf("list pod metrics: %w", err)
	}
	return Summarize(list.Items, now), nil
}

// Keep stores an hour of key's usage; a repeated hour keeps the larger
// values (the store, in the server).
type Keep func(ctx context.Context, key SeriesKey, r Rollup) error

// Deps are what the collector needs.
type Deps struct {
	Cluster dynamic.Interface
	Buffer  *Buffer
	Keep    Keep
	Health  *health.Health
	Clock   clock.Clock
	Logger  *slog.Logger
}

// Run scrapes into d.Buffer until ctx ends; after each full hour, it hands
// that hour's rollups to d.Keep.
func Run(ctx context.Context, d Deps) error {
	rolled := HourOf(d.Clock.NowMs())
	for {
		rolled = d.tick(ctx, rolled)
		timer := time.NewTimer(ScrapeEvery)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

// tick scrapes once and rolls up every hour completed since rolled; it
// returns the hour rolled up to.
func (d Deps) tick(ctx context.Context, rolled int64) int64 {
	measured, err := Scrape(ctx, d.Cluster, d.Clock.NowMs())
	if err != nil {
		// A missing Metrics API is a fact of the cluster, not a fault.
		d.Buffer.setUnavailable(opt.Some("the Metrics API is unavailable: " + err.Error()))
	} else {
		for _, m := range measured {
			d.Buffer.Record(m.Key, m.Sample)
		}
		d.Buffer.setUnavailable(opt.None[string]())
	}
	d.Health.OK(Subsystem)
	now := d.Clock.NowMs()
	d.Buffer.Trim(now)
	hour := HourOf(now)
	if hour <= rolled {
		return rolled
	}
	for from := rolled; from < hour; from += HourMs {
		for _, s := range d.Buffer.Between(from, from+HourMs) {
			r, ok := RollupOf(from, s.Samples)
			if !ok {
				continue
			}
			if err := d.Keep(ctx, s.Key, r); err != nil {
				d.Logger.Warn("a usage rollup was not kept", "error", err, "app", s.Key.App)
			}
		}
	}
	return hour
}
