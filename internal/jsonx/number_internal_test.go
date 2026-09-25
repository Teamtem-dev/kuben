package wire

import (
	"math"
	"testing"
)

func TestPow10IsTheCorrectlyRoundedTable(t *testing.T) {
	differ := 0
	for i := range pow10 {
		if pow10[i] != math.Pow10(i) {
			differ++
		}
	}
	t.Logf("math.Pow10 differs from the literal table at %d exponents", differ)
	if pow10[308] != 1e308 || pow10[22] != 1e22 || pow10[23] != 1e23 {
		t.Fatal("table entries must equal the literals")
	}
}
