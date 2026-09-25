// Package clock is Kuben's time: unix milliseconds (int64), so rows sort and
// compare without time-zone conversion. Code that needs "now" takes a
// [Clock], so tests set the time instead of sleeping.
package clock

import (
	"math"
	"time"
)

const hourMs = int64(time.Hour / time.Millisecond)

// Clock tells the time in unix milliseconds.
type Clock interface{ NowMs() int64 }

// System is the wall clock.
type System struct{}

// NowMs is the current time.
func (System) NowMs() int64 { return time.Now().UnixMilli() }

// Fixed is a clock that stands still, for tests.
type Fixed int64

// NowMs is the fixed time.
func (f Fixed) NowMs() int64 { return int64(f) }

// PlusHours adds hours to a unix-millisecond timestamp, saturating instead
// of overflowing.
func PlusHours(tsMs int64, hours uint64) int64 {
	if hours > uint64(math.MaxInt64/hourMs) {
		return math.MaxInt64
	}
	return SaturatingAdd(tsMs, int64(hours)*hourMs)
}

// Seconds is n seconds as a duration, saturating at the longest duration
// instead of wrapping (configured timeouts are u64 seconds in Rust).
func Seconds(n uint64) time.Duration {
	if n > uint64(math.MaxInt64/int64(time.Second)) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(n) * time.Second //nolint:gosec // bounded above
}

// SaturatingAdd is a+b, clamped to the int64 range.
func SaturatingAdd(a, b int64) int64 {
	switch {
	case b > 0 && a > math.MaxInt64-b:
		return math.MaxInt64
	case b < 0 && a < math.MinInt64-b:
		return math.MinInt64
	default:
		return a + b
	}
}

// SaturatingSub is a-b, clamped to the int64 range.
func SaturatingSub(a, b int64) int64 {
	switch {
	case b > 0 && a < math.MinInt64+b:
		return math.MinInt64
	case b < 0 && a > math.MaxInt64+b:
		return math.MaxInt64
	default:
		return a - b
	}
}
