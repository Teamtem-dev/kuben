// Package usage is the bounded resource usage of Kuben's apps: CPU and
// memory from the Kubernetes Metrics API (M5.5; plan §14.5). It replaces
// crates/kuben-platform/src/usage.rs.
//
//   - Live window. Each replica keeps the last hour in memory: at most
//     SamplesPerSeries samples for each of at most MaxSeries apps. The least
//     recently updated series goes first when the budget is full.
//   - Rollups. Hourly averages and maxima go to SQL and are kept for a week
//     (the retention pass removes older rows).
//   - Missing data. A window without samples is unavailable, never zero.
//     That covers a cluster without the Metrics API, a fresh replica and an
//     app that runs no pods.
package usage

import (
	"cmp"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/Teamtem-dev/kuben/api/v1alpha1"
	"github.com/Teamtem-dev/kuben/internal/core/capacity"
	"github.com/Teamtem-dev/kuben/internal/core/opt"
)

const (
	// SamplesPerSeries is the samples kept per app: an hour at the scrape
	// interval.
	SamplesPerSeries = 120
	// MaxSeries is the number of apps tracked at most.
	MaxSeries = 5_000
	// ScrapeEvery is how often the Metrics API is read.
	ScrapeEvery = 30 * time.Second
	// HourMs is an hour in milliseconds.
	HourMs int64 = 3_600_000
)

// Sample is one app's usage at one moment, summed over its pods.
type Sample struct {
	// At is unix milliseconds.
	At          int64
	CPUMillis   uint64
	MemoryBytes uint64
	Pods        uint32
}

// SeriesKey is an app, as its pods are labelled.
type SeriesKey struct {
	Namespace string
	App       string
	// Org is the organization label of its pods.
	Org string
}

func compareKeys(a, b SeriesKey) int {
	return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.App, b.App), cmp.Compare(a.Org, b.Org))
}

type series struct {
	// samples are oldest first.
	samples []Sample
	touched int64
}

// Buffer is the live usage of a replica. The zero value is empty and ready
// to use.
type Buffer struct {
	mu     sync.Mutex
	series map[SeriesKey]*series
	// unavailable is why the last scrape failed, if it did.
	unavailable opt.Val[string]
}

// Record adds sample to key's series, within the budgets.
func (b *Buffer) Record(key SeriesKey, sample Sample) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.series == nil {
		b.series = map[SeriesKey]*series{}
	}
	s, ok := b.series[key]
	if !ok {
		if len(b.series) >= MaxSeries {
			b.evictOldest()
		}
		s = &series{}
		b.series[key] = s
	}
	s.samples = append(s.samples, sample)
	if extra := len(s.samples) - SamplesPerSeries; extra > 0 {
		s.samples = slices.Delete(s.samples, 0, extra)
	}
	s.touched = sample.At
}

// evictOldest removes the least recently updated series; b.mu is held.
func (b *Buffer) evictOldest() {
	var (
		oldest SeriesKey
		at     int64 = math.MaxInt64
		found  bool
	)
	for k, s := range b.series {
		if !found || s.touched < at {
			oldest, at, found = k, s.touched, true
		}
	}
	if found {
		delete(b.series, oldest)
	}
}

// Trim forgets samples older than an hour before now, and series left empty.
func (b *Buffer) Trim(now int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for k, s := range b.series {
		keep := slices.IndexFunc(s.samples, func(x Sample) bool { return x.At >= now-HourMs })
		if keep < 0 {
			delete(b.series, k)
			continue
		}
		s.samples = slices.Delete(s.samples, 0, keep)
	}
}

// Window is namespace/app's samples since since, oldest first.
func (b *Buffer) Window(namespace, app string, since int64) []Sample {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := []Sample{}
	for k, s := range b.series {
		if k.Namespace != namespace || k.App != app {
			continue
		}
		for _, x := range s.samples {
			if x.At >= since {
				out = append(out, x)
			}
		}
		break
	}
	return out
}

// Series is one app's samples.
type Series struct {
	Key     SeriesKey
	Samples []Sample
}

// Between is every series' samples within [from, to), series without any
// left out.
func (b *Buffer) Between(from, to int64) []Series {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []Series
	for k, s := range b.series {
		var samples []Sample
		for _, x := range s.samples {
			if x.At >= from && x.At < to {
				samples = append(samples, x)
			}
		}
		if len(samples) > 0 {
			out = append(out, Series{Key: k, Samples: samples})
		}
	}
	return out
}

// Len is the number of series kept.
func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.series)
}

// Unavailable is why usage is unavailable right now, if it is.
func (b *Buffer) Unavailable() opt.Val[string] {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.unavailable
}

func (b *Buffer) setUnavailable(why opt.Val[string]) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.unavailable = why
}

// Rollup is an hour of one app's usage.
type Rollup struct {
	// Hour is the hour's start, unix milliseconds.
	Hour      int64
	CPUAvg    uint64
	CPUMax    uint64
	MemoryAvg uint64
	MemoryMax uint64
	Samples   uint32
}

// RollupOf is the rollup of samples of the hour starting at hour; false
// without any.
func RollupOf(hour int64, samples []Sample) (Rollup, bool) {
	if len(samples) == 0 {
		return Rollup{}, false
	}
	n := uint64(len(samples))
	r := Rollup{Hour: hour, Samples: uint32(min(n, math.MaxUint32))}
	var cpu, memory uint64
	for _, s := range samples {
		cpu = saturatingAdd(cpu, s.CPUMillis)
		memory = saturatingAdd(memory, s.MemoryBytes)
		r.CPUMax = max(r.CPUMax, s.CPUMillis)
		r.MemoryMax = max(r.MemoryMax, s.MemoryBytes)
	}
	r.CPUAvg, r.MemoryAvg = cpu/n, memory/n
	return r, true
}

// HourOf is the start of the hour at is in.
func HourOf(at int64) int64 {
	rem := at % HourMs
	if rem < 0 {
		rem += HourMs
	}
	return at - rem
}

func saturatingAdd(a, b uint64) uint64 {
	if a > math.MaxUint64-b {
		return math.MaxUint64
	}
	return a + b
}

// Measured is one app's sample of one scrape.
type Measured struct {
	Key    SeriesKey
	Sample Sample
}

// Summarize sums the containers of each pod of a PodMetricsList into one
// sample per app, ordered by key. Pods without Kuben's app and
// organization labels are skipped.
func Summarize(items []unstructured.Unstructured, at int64) []Measured {
	sums := map[SeriesKey]*Sample{}
	for _, item := range items {
		namespace, hasNamespace := str(item.Object, "metadata", "namespace")
		app, hasApp := str(item.Object, "metadata", "labels", v1alpha1.LabelApp)
		org, hasOrg := str(item.Object, "metadata", "labels", v1alpha1.LabelOrg)
		if !hasNamespace || !hasApp || !hasOrg {
			continue
		}
		key := SeriesKey{Namespace: namespace, App: app, Org: org}
		sum, ok := sums[key]
		if !ok {
			sum = &Sample{At: at}
			sums[key] = sum
		}
		sum.Pods++
		containers, ok := item.Object["containers"].([]any)
		if !ok {
			continue
		}
		for _, c := range containers {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			cpu, _ := str(cm, "usage", "cpu")
			memory, _ := str(cm, "usage", "memory")
			millis, _ := cpuNanosToMillis(cpu)
			bytes, _ := capacity.Bytes(memory)
			sum.CPUMillis = saturatingAdd(sum.CPUMillis, millis)
			sum.MemoryBytes = saturatingAdd(sum.MemoryBytes, bytes)
		}
	}
	out := make([]Measured, 0, len(sums))
	for k, s := range sums {
		out = append(out, Measured{Key: k, Sample: *s})
	}
	slices.SortFunc(out, func(a, b Measured) int { return compareKeys(a.Key, b.Key) })
	return out
}

// cpuNanosToMillis reads a CPU quantity of the Metrics API, which reports
// nanocores (`123456n`) or microcores (`123u`) as often as cores or
// millicores.
func cpuNanosToMillis(q string) (uint64, bool) {
	if n, ok := strings.CutSuffix(q, "n"); ok {
		v, ok := parseUint(n)
		return v / 1_000_000, ok
	}
	if u, ok := strings.CutSuffix(q, "u"); ok {
		v, ok := parseUint(u)
		return v / 1_000, ok
	}
	return capacity.CPUMillis(q)
}

// parseUint is Rust's u64 parser: decimal digits with one optional
// leading `+`.
func parseUint(s string) (uint64, bool) {
	s = strings.TrimPrefix(s, "+")
	if s == "" || s[0] == '+' || s[0] == '-' {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 10, 64)
	return v, err == nil
}

// str is the string at fields of obj; false when there is none, or what is
// there is not a string (serde_json's as_str).
func str(obj map[string]any, fields ...string) (string, bool) {
	s, found, err := unstructured.NestedString(obj, fields...)
	return s, found && err == nil
}
