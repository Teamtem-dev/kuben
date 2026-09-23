package materializer_test

import (
	"testing"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
	"github.com/Teamtem-dev/kuben/go/hub/internal/platform/materializer"
	"github.com/Teamtem-dev/kuben/go/kubenapi/v1alpha1"
)

// write.rs only_objects_kuben_made_for_the_same_organization_are_taken_over.
func TestOnlyObjectsKubenMadeForTheSameOrganizationAreTakenOver(t *testing.T) {
	org := ids.New[ids.Org]()
	ours := map[string]string{v1alpha1.LabelManagedBy: v1alpha1.LabelManagerValue, v1alpha1.LabelOrg: org.String()}
	if !materializer.BelongsTo(ours, org) {
		t.Fatal("ours")
	}
	if materializer.BelongsTo(ours, ids.New[ids.Org]()) {
		t.Fatal("another organization")
	}
	if materializer.BelongsTo(map[string]string{v1alpha1.LabelOrg: org.String()}, org) {
		t.Fatal("not managed by Kuben")
	}
	if materializer.BelongsTo(nil, org) {
		t.Fatal("no labels")
	}
}
