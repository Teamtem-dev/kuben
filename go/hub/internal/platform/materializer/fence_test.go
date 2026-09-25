package materializer_test

import (
	"testing"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ops/target"
	"github.com/Teamtem-dev/kuben/go/hub/internal/core/opt"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/materializer"
	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

func annotated(value string) map[string]string {
	return map[string]string{v1alpha1.AnnotationGeneration: value}
}

func TestOnlyTheCurrentGenerationWrites(t *testing.T) {
	g2, g3 := target.Generation(2), target.Generation(3)
	cases := []struct {
		ours, current target.Generation
		live          map[string]string
		want          materializer.Fence
	}{
		{g2, g3, nil, materializer.FenceSuperseded{Current: 3}},
		{g3, g3, nil, materializer.FenceWrite{}}, // adoption
		{g3, g3, annotated("2"), materializer.FenceWrite{}},
		{g3, g3, annotated("3"), materializer.FenceWrite{}}, // a retried write
		{g3, g3, annotated("oops"), materializer.FenceWrite{}},
	}
	for _, c := range cases {
		if got := materializer.FenceFor(c.ours, c.current, c.live); got != c.want {
			t.Errorf("%d/%d %v: %#v", c.ours, c.current, c.live, got)
		}
	}
	if materializer.MayWrite(materializer.FenceFor(g2, g3, annotated("1"))) {
		t.Fatal("superseded")
	}
}

func TestAGenerationSQLNeverAcceptedIsForged(t *testing.T) {
	g3 := target.Generation(3)
	forged := materializer.FenceFor(g3, g3, annotated("99"))
	if forged != (materializer.FenceForged{Live: 99}) || !materializer.MayWrite(forged) {
		t.Fatalf("%#v", forged)
	}
	if got := materializer.LiveGeneration(annotated("99")); got != opt.Some[uint64](99) {
		t.Fatal(got)
	}
	if got := materializer.LiveGeneration(nil); got.IsSome() {
		t.Fatal(got)
	}
}
