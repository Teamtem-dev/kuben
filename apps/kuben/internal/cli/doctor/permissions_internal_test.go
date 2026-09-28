package doctor

import "testing"

// The leader-election Lease is granted by the chart's Role in Kuben's own
// namespace, never cluster-wide: asking cluster-wide would be denied.
func TestOnlyTheLeaseIsCheckedInKubensOwnNamespace(t *testing.T) {
	for _, p := range needed() {
		attrs := accessReview(p, "kuben-system").Spec.ResourceAttributes
		want := ""
		if p.resource == "leases" {
			want = "kuben-system"
		}
		if attrs.Namespace != want {
			t.Errorf("%s %s: namespace %q, want %q", p.verb, p.resource, attrs.Namespace, want)
		}
	}
}
