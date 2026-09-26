package capacity_test

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"testing/quick"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/capacity"
	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
)

func TestQuantitiesParseLikeKubernetes(t *testing.T) {
	cpu := []struct {
		q      string
		millis uint64
		ok     bool
	}{
		{"500m", 500, true},
		{"2", 2000, true},
		{"0.5", 500, true},
		{"1.25", 1250, true},
		{".1", 100, true},
		{" 250m ", 250, true},
		{"", 0, false},
		{"m", 0, false},
		{"abc", 0, false},
		{"1.2.3", 0, false},
		{"-1", 0, false},
		{"1x", 0, false},
	}
	for _, c := range cpu {
		if got, ok := capacity.CPUMillis(c.q); got != c.millis || ok != c.ok {
			t.Errorf("CPUMillis(%q) = %d, %v; want %d, %v", c.q, got, ok, c.millis, c.ok)
		}
	}
	mem := []struct {
		q     string
		bytes uint64
		ok    bool
	}{
		{"512Mi", 512 << 20, true},
		{"1Gi", 1 << 30, true},
		{"1.5Gi", 3 << 29, true},
		{"1G", 1_000_000_000, true},
		{"100k", 100_000, true},
		{"1048576", 1 << 20, true},
		{"0.0000000011Gi", 0, false},
		{"0.000000001Gi", 2, true}, // rounded up
		{"", 0, false},
		{"Mi", 0, false},
		{"1Zi", 0, false},
		{"one", 0, false},
		{"1.0000000001Gi", 0, false},
		{"99999999999Ei", 0, false}, // overflow
		{"18446744073709551615", math.MaxUint64, true},
		{"18446744073709551616", 0, false},
	}
	for _, c := range mem {
		if got, ok := capacity.Bytes(c.q); got != c.bytes || ok != c.ok {
			t.Errorf("Bytes(%q) = %d, %v; want %d, %v", c.q, got, ok, c.bytes, c.ok)
		}
	}
}

func TestQuotasBoundEveryResource(t *testing.T) {
	limits := capacity.Limits{CPUMillis: opt.Some[uint64](2000), MemoryBytes: opt.Some[uint64](4 << 30)}
	fits := capacity.Pods(4, 500, 1<<30)
	if err := limits.Admit(fits); err != nil {
		t.Fatal(err)
	}
	var refused capacity.Exceeded
	if err := limits.Admit(fits.Plus(capacity.Pods(1, 100, 0))); !errors.As(err, &refused) {
		t.Fatalf("got %v", err)
	}
	if want := (capacity.Exceeded{Resource: capacity.CPU, Requested: 2100, Limit: 2000}); refused != want {
		t.Errorf("got %+v", refused)
	}
	if got := refused.Error(); got != "CPU requests would reach 2100m CPU, over the quota of 2 CPU" {
		t.Errorf("got %q", got)
	}
	err := limits.Admit(capacity.Pods(5, 0, 1<<30))
	if err == nil || err.Error() != "memory requests would reach 5Gi, over the quota of 4Gi" {
		t.Errorf("got %v", err)
	}
	pods := capacity.Limits{Pods: opt.Some[uint64](3)}
	if err := pods.Admit(capacity.Pods(4, 0, 0)); !errors.As(err, &refused) || refused.Resource != capacity.PodCount {
		t.Errorf("got %v", err)
	}
	if got := refused.Error(); got != "pods requests would reach 4 pods, over the quota of 3 pods" {
		t.Errorf("got %q", got)
	}
	if !(capacity.Limits{}).IsUnlimited() || limits.IsUnlimited() {
		t.Error("only limits without any bound are unlimited")
	}
	huge := capacity.Pods(math.MaxUint64, math.MaxUint64, math.MaxUint64)
	if err := (capacity.Limits{}).Admit(huge); err != nil {
		t.Errorf("got %v", err)
	}
}

func TestValuesAreShownForPeople(t *testing.T) {
	cases := []struct {
		resource capacity.Resource
		value    uint64
		want     string
	}{
		{capacity.CPU, 2000, "2 CPU"},
		{capacity.CPU, 0, "0 CPU"},
		{capacity.CPU, 1500, "1500m CPU"},
		{capacity.Memory, 3 << 30, "3Gi"},
		{capacity.Memory, 512 << 20, "512Mi"},
		{capacity.Memory, (1 << 20) + 1, "2Mi"},
		{capacity.Memory, math.MaxUint64, "17592186044416Mi"},
		{capacity.PodCount, 7, "7 pods"},
	}
	for _, c := range cases {
		if got := capacity.Show(c.resource, c.value); got != c.want {
			t.Errorf("Show(%s, %d) = %q, want %q", c.resource, c.value, got, c.want)
		}
	}
}

func TestPodsLargerThanAnyNodeDoNotSchedule(t *testing.T) {
	node := opt.Some(capacity.NodeCapacity{CPUMillis: 4000, MemoryBytes: 8 << 30})
	if got := capacity.EstimateFit(node, 4000, 8<<30); got != (capacity.FitsEstimate{}) {
		t.Errorf("got %#v", got)
	}
	cpu, ok := capacity.EstimateFit(node, 4001, 0).(capacity.UnlikelyToSchedule)
	if !ok || cpu.Why != "a pod requests 4001m CPU, the largest node offers 4 CPU" {
		t.Errorf("got %#v", cpu)
	}
	mem, ok := capacity.EstimateFit(node, 1, (8<<30)+1).(capacity.UnlikelyToSchedule)
	if !ok || !strings.Contains(mem.Why, "memory") {
		t.Errorf("got %#v", mem)
	}
	unknown, ok := capacity.EstimateFit(opt.None[capacity.NodeCapacity](), 1, 1).(capacity.UnknownConstraints)
	if !ok || unknown.Why != "the cluster's nodes are not known yet" {
		t.Errorf("got %#v", unknown)
	}
}

func TestDemandsAddUpWithoutOverflow(t *testing.T) {
	property := func(a, b uint64, n uint16) bool {
		count := uint64(n % 1000)
		d := capacity.Pods(count, a, b)
		sum := capacity.Sum(d, d)
		return sum.CPUMillis >= d.CPUMillis && sum.MemoryBytes >= d.MemoryBytes && sum.Pods == count*2
	}
	if err := quick.Check(property, nil); err != nil {
		t.Error(err)
	}
	if got := capacity.Sum(); got != (capacity.Demand{}) {
		t.Errorf("got %+v", got)
	}
	top := capacity.Pods(2, math.MaxUint64, 1)
	if top.CPUMillis != math.MaxUint64 || top.Plus(top).CPUMillis != math.MaxUint64 {
		t.Errorf("got %+v", top)
	}
}

func TestMillicoresRoundTrip(t *testing.T) {
	property := func(n uint32) bool {
		m := uint64(n % 10_000_000)
		millis, ok := capacity.CPUMillis(fmt.Sprintf("%dm", m))
		cores, okCores := capacity.CPUMillis(fmt.Sprintf("%d", m*1000))
		return ok && okCores && millis == m && cores == m*1_000_000
	}
	if err := quick.Check(property, nil); err != nil {
		t.Error(err)
	}
}
