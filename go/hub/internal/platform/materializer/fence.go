package materializer

// The generation fence (ADR-027) for materialized App objects.
//
// A worker writes the App object of its run only while the run's
// generation is the target's current one, as SQL says after the live object
// was read; the write carries the resourceVersion of that read. A stale
// worker therefore never lowers the generation of a live object: either SQL
// already shows the newer generation, or the newer write changed the
// resourceVersion and the stale write fails with 409 and starts over.
//
// The generation annotation of the live object is only a report: a value
// higher than any generation SQL accepted was not written by Kuben.

import (
	"strconv"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ops/target"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

// Fence is whether a run may write its target's App object.
//
//sumtype:decl
type Fence interface{ isFence() }

type (
	// FenceWrite means write; the run's generation is the target's current one.
	FenceWrite struct{}
	// FenceForged means write, replacing an annotation that claims a generation
	// SQL never accepted.
	FenceForged struct{ Live uint64 }
	// FenceSuperseded means do not write; the target is at another generation,
	// owned by a newer run.
	FenceSuperseded struct{ Current uint64 }
)

func (FenceWrite) isFence()      {}
func (FenceForged) isFence()     {}
func (FenceSuperseded) isFence() {}

// MayWrite reports whether f lets the run write.
func MayWrite(f Fence) bool {
	_, superseded := f.(FenceSuperseded)
	return !superseded
}

// FenceFor decides for the run of generation ours, with the target's
// current generation read from SQL after the live object's annotations
// were read.
func FenceFor(ours, current target.Generation, live map[string]string) Fence {
	if current != ours {
		return FenceSuperseded{Current: uint64(current)}
	}
	if g, ok := LiveGeneration(live).Get(); ok && g > uint64(ours) {
		return FenceForged{Live: g}
	}
	return FenceWrite{}
}

// LiveGeneration is the generation annotation among a live object's
// annotations, when it has a readable one.
func LiveGeneration(annotations map[string]string) opt.Val[uint64] {
	text, ok := annotations[v1alpha1.AnnotationGeneration]
	if !ok {
		return opt.None[uint64]()
	}
	g, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return opt.None[uint64]()
	}
	return opt.Some(g)
}
