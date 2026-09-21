package wire_test

import (
	"encoding/json"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/Teamtem-dev/kuben/go/hub/internal/wire"
)

// The fixtures come from the Rust code (crates/kuben-platform/src/
// compat_fixtures.rs): the contract is whatever it wrote.
func fixture(t *testing.T, name string, out any) {
	t.Helper()
	data, err := os.ReadFile("../../testdata/compat/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatal(err)
	}
}

func TestCanonicalMatchesRust(t *testing.T) {
	var f struct {
		Cases []struct{ Input, Canonical, SHA256 string }
	}
	fixture(t, "canonical.json", &f)
	if len(f.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range f.Cases {
		got, err := wire.Canonical([]byte(c.Input))
		if err != nil {
			t.Errorf("%s: %v", c.Input, err)
			continue
		}
		if got != c.Canonical {
			t.Errorf("%s:\n got %q\nwant %q", c.Input, got, c.Canonical)
		}
		if h := wire.SHA256(got); h != c.SHA256 {
			t.Errorf("%s: hash %s, want %s", c.Input, h, c.SHA256)
		}
	}
}

func TestFloatsPrintAsSerde(t *testing.T) {
	var f struct {
		Printed []struct{ Bits, Text string }
		Parsed  []struct{ Input, Canonical string }
	}
	fixture(t, "floats.json", &f)
	if len(f.Printed) < 4000 {
		t.Fatalf("only %d printed floats", len(f.Printed))
	}
	failures := 0
	for _, p := range f.Printed {
		bits, err := strconv.ParseUint(p.Bits, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		if got := wire.FormatFloat(math.Float64frombits(bits)); got != p.Text {
			failures++
			if failures <= 10 {
				t.Errorf("%v: got %s, want %s", math.Float64frombits(bits), got, p.Text)
			}
		}
	}
	for _, p := range f.Parsed {
		got, err := wire.Canonical([]byte(p.Input))
		if err != nil || got != p.Canonical {
			t.Errorf("%s: got %q (%v), want %q", p.Input, got, err, p.Canonical)
		}
	}
	if failures > 0 {
		t.Errorf("%d of %d floats differ", failures, len(f.Printed))
	}
}

func TestCanonicalRefusesWhatIsNotOneValue(t *testing.T) {
	for _, bad := range []string{``, `{`, `1 2`, `[1e999]`} {
		if _, err := wire.Canonical([]byte(bad)); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

func TestCanonicalValueSortsStructFields(t *testing.T) {
	got, err := wire.CanonicalValue(struct {
		Z string `json:"z"`
		A []int  `json:"a"`
	}{Z: "<&>", A: []int{2, 1}})
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"a":[2,1],"z":"<&>"}` {
		t.Fatalf("got %s", got)
	}
}

// serde_json reads decimals with a fast, not always correctly rounded
// algorithm, so canonical text holding floats is not a fixed point there
// either ("1.1220202122000201e+23" reads back as 1.12202021220002e+23).
// What must hold: the output is valid JSON, and without floats it is stable.
func FuzzCanonicalIsStable(f *testing.F) {
	for _, seed := range []string{`{"b":[1,2.5,"x"],"a":null}`, `[-0,1e21,"\u2028"]`, `"\u0000"`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		once, err := wire.Canonical([]byte(text))
		if err != nil {
			return
		}
		if !json.Valid([]byte(once)) {
			t.Fatalf("canonical output %q is not JSON", once)
		}
		if strings.ContainsAny(once, ".eE") && strings.ContainsAny(once, "0123456789") {
			return // floats: see above
		}
		twice, err := wire.Canonical([]byte(once))
		if err != nil || once != twice {
			t.Fatalf("not a fixed point: %q then %q (%v)", once, twice, err)
		}
	})
}
