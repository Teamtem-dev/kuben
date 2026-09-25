// Package capacity is quota and scheduling admission (M4.5; plan §11, §16).
// It replaces the Rust module kuben-core/src/capacity.rs.
//
// A deployment is admitted on requested resources, never on current usage:
// the peak an app may request (every process at its maximum replicas, plus
// the one surge pod of a rolling update) plus what the other apps of the
// environment may request must fit the environment's quota, and the whole
// organization's must fit its quota. A pod larger than the largest
// schedulable node will not run; whether it fits otherwise is an estimate.
// A quota is never overridden; an estimate is only a warning.
package capacity

import (
	"fmt"
	"math"
	"math/bits"
	"strings"

	"github.com/Teamtem-dev/kuben/internal/core/opt"
)

// Demand is the resources a set of pods requests.
type Demand struct {
	CPUMillis   uint64
	MemoryBytes uint64
	Pods        uint64
}

// Pods is the demand of count pods of cpuMillis and memoryBytes each. It
// saturates instead of overflowing.
func Pods(count, cpuMillis, memoryBytes uint64) Demand {
	return Demand{
		CPUMillis:   saturatingMul(cpuMillis, count),
		MemoryBytes: saturatingMul(memoryBytes, count),
		Pods:        count,
	}
}

// Plus is the demand of both, saturating instead of overflowing.
func (d Demand) Plus(other Demand) Demand {
	return Demand{
		CPUMillis:   saturatingAdd(d.CPUMillis, other.CPUMillis),
		MemoryBytes: saturatingAdd(d.MemoryBytes, other.MemoryBytes),
		Pods:        saturatingAdd(d.Pods, other.Pods),
	}
}

// Sum is the demand of all of demands; nothing for none.
func Sum(demands ...Demand) Demand {
	var total Demand
	for _, d := range demands {
		total = total.Plus(d)
	}
	return total
}

func saturatingAdd(a, b uint64) uint64 {
	sum, carry := bits.Add64(a, b, 0)
	if carry != 0 {
		return math.MaxUint64
	}
	return sum
}

func saturatingMul(a, b uint64) uint64 {
	hi, lo := bits.Mul64(a, b)
	if hi != 0 {
		return math.MaxUint64
	}
	return lo
}

// Limits are upper bounds; an absent one is unlimited.
type Limits struct {
	CPUMillis   opt.Val[uint64]
	MemoryBytes opt.Val[uint64]
	Pods        opt.Val[uint64]
}

// Resource is what a quota bounds. The value is how messages name it.
type Resource string

// The resources.
const (
	CPU      Resource = "CPU"
	Memory   Resource = "memory"
	PodCount Resource = "pods"
)

// Exceeded is a demand beyond a quota.
type Exceeded struct {
	Resource  Resource
	Requested uint64
	Limit     uint64
}

func (e Exceeded) Error() string {
	return fmt.Sprintf("%s requests would reach %s, over the quota of %s",
		e.Resource, Show(e.Resource, e.Requested), Show(e.Resource, e.Limit))
}

// Show is value of resource for people.
func Show(resource Resource, value uint64) string {
	const mib, gib = 1 << 20, 1 << 30
	switch resource {
	case CPU:
		if value%1000 == 0 {
			return fmt.Sprintf("%d CPU", value/1000)
		}
		return fmt.Sprintf("%dm CPU", value)
	case Memory:
		if value%gib == 0 {
			return fmt.Sprintf("%dGi", value/gib)
		}
		rounded := value / mib
		if value%mib != 0 {
			rounded++
		}
		return fmt.Sprintf("%dMi", rounded)
	case PodCount:
		return fmt.Sprintf("%d pods", value)
	}
	return fmt.Sprintf("%d %s", value, string(resource))
}

// Admit admits demand (everything, the new app included) under these
// limits. The error is an [Exceeded] naming the first resource over its
// quota, in the order CPU, memory, pods.
func (l Limits) Admit(demand Demand) error {
	checks := []struct {
		resource  Resource
		limit     opt.Val[uint64]
		requested uint64
	}{
		{CPU, l.CPUMillis, demand.CPUMillis},
		{Memory, l.MemoryBytes, demand.MemoryBytes},
		{PodCount, l.Pods, demand.Pods},
	}
	for _, c := range checks {
		if limit, bounded := c.limit.Get(); bounded && c.requested > limit {
			return Exceeded{Resource: c.resource, Requested: c.requested, Limit: limit}
		}
	}
	return nil
}

// IsUnlimited reports whether nothing is bounded.
func (l Limits) IsUnlimited() bool {
	return l.CPUMillis.IsNone() && l.MemoryBytes.IsNone() && l.Pods.IsNone()
}

// NodeCapacity is the largest schedulable node, as last discovered.
type NodeCapacity struct {
	CPUMillis   uint64
	MemoryBytes uint64
}

// Estimate is whether the pods of a deployment are likely to be scheduled.
//
//sumtype:decl
type Estimate interface {
	estimate()
}

// FitsEstimate means every pod fits on the largest node.
type FitsEstimate struct{}

// UnlikelyToSchedule means a pod requests more than any schedulable node offers,
// so it will not run. Why says which resource.
type UnlikelyToSchedule struct{ Why string }

// UnknownConstraints means not known (no node facts); never shown as safe.
type UnknownConstraints struct{ Why string }

func (FitsEstimate) estimate()       {}
func (UnlikelyToSchedule) estimate() {}
func (UnknownConstraints) estimate() {}

// EstimateFit estimates whether a pod of cpuMillis and memoryBytes fits on
// largest.
func EstimateFit(largest opt.Val[NodeCapacity], cpuMillis, memoryBytes uint64) Estimate {
	node, known := largest.Get()
	if !known {
		return UnknownConstraints{Why: "the cluster's nodes are not known yet"}
	}
	if cpuMillis > node.CPUMillis {
		return UnlikelyToSchedule{Why: fmt.Sprintf("a pod requests %s, the largest node offers %s",
			Show(CPU, cpuMillis), Show(CPU, node.CPUMillis))}
	}
	if memoryBytes > node.MemoryBytes {
		return UnlikelyToSchedule{Why: fmt.Sprintf("a pod requests %s of memory, the largest node offers %s",
			Show(Memory, memoryBytes), Show(Memory, node.MemoryBytes))}
	}
	return FitsEstimate{}
}

// CPUMillis reads a Kubernetes CPU quantity (`500m`, `2`, `0.5`) as
// millicores; false when it is not one.
func CPUMillis(quantity string) (uint64, bool) {
	q := strings.TrimSpace(quantity)
	if millis, ok := strings.CutSuffix(q, "m"); ok {
		return parseUint(millis)
	}
	return scaled(q, 1000)
}

// Bytes reads a Kubernetes memory quantity (`512Mi`, `1Gi`, `1G`,
// `1048576`) as bytes; false when it is not one.
func Bytes(quantity string) (uint64, bool) {
	units := []struct {
		suffix string
		factor uint64
	}{
		{"Ki", 1 << 10},
		{"Mi", 1 << 20},
		{"Gi", 1 << 30},
		{"Ti", 1 << 40},
		{"Pi", 1 << 50},
		{"Ei", 1 << 60},
		{"k", 1_000},
		{"M", 1_000_000},
		{"G", 1_000_000_000},
		{"T", 1_000_000_000_000},
		{"P", 1_000_000_000_000_000},
		{"E", 1_000_000_000_000_000_000},
	}
	q := strings.TrimSpace(quantity)
	for _, u := range units {
		if number, ok := strings.CutSuffix(q, u.suffix); ok {
			return scaled(number, u.factor)
		}
	}
	return scaled(q, 1)
}

// parseUint reads decimal digits, with the one leading `+` Rust's integer
// parser accepts.
func parseUint(s string) (uint64, bool) {
	s = strings.TrimPrefix(s, "+")
	if s == "" || !digits(s) {
		return 0, false
	}
	var out uint64
	for _, c := range []byte(s) {
		hi, lo := bits.Mul64(out, 10)
		sum, carry := bits.Add64(lo, uint64(c-'0'), 0)
		if hi != 0 || carry != 0 {
			return 0, false
		}
		out = sum
	}
	return out, true
}

func digits(s string) bool {
	for _, c := range []byte(s) {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// scaled is number (an integer or a decimal) times factor, rounded up.
func scaled(number string, factor uint64) (uint64, bool) {
	whole, fraction, _ := strings.Cut(number, ".")
	if whole == "" && fraction == "" {
		return 0, false
	}
	if !digits(whole) || !digits(fraction) || len(fraction) > 9 {
		return 0, false
	}
	var units uint64
	if whole != "" {
		var ok bool
		if units, ok = parseUint(whole); !ok {
			return 0, false
		}
	}
	hi, out := bits.Mul64(units, factor)
	if hi != 0 {
		return 0, false
	}
	if fraction == "" {
		return out, true
	}
	part, ok := parseUint(fraction)
	if !ok {
		return 0, false
	}
	scale := uint64(1)
	for range len(fraction) {
		scale *= 10
	}
	// part < scale, so part*factor/scale < factor: the quotient fits 64 bits
	// and the high word is below the divisor, which Div64 requires.
	hi, lo := bits.Mul64(part, factor)
	extra, rem := bits.Div64(hi, lo, scale)
	if rem != 0 {
		extra++
	}
	sum, carry := bits.Add64(out, extra, 0)
	if carry != 0 {
		return 0, false
	}
	return sum, true
}
