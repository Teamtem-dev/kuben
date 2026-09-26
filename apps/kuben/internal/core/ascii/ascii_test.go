package ascii_test

import (
	"testing"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/ascii"
)

func TestLowerTouchesASCIILettersOnly(t *testing.T) {
	cases := map[string]string{
		"":              "",
		"already lower": "already lower",
		"Kuben.DEV":     "kuben.dev",
		"ÀÉ-İ-ẞ-Ab":     "ÀÉ-İ-ẞ-ab",
		"MIXED_123":     "mixed_123",
	}
	for in, want := range cases {
		if got := ascii.Lower(in); got != want {
			t.Errorf("Lower(%q) = %q, want %q", in, got, want)
		}
	}
}
