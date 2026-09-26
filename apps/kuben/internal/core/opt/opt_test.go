package opt_test

import (
	"encoding/json"
	"testing"

	"github.com/Teamtem-dev/kuben/apps/kuben/internal/core/opt"
)

type doc struct {
	Name  opt.Val[string] `json:"name"`
	Extra opt.Val[int]    `json:"extra,omitzero"`
}

func TestAbsentIsNullOrLeftOut(t *testing.T) {
	got, err := json.Marshal(doc{})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"name":null}` {
		t.Fatalf("got %s", got)
	}
}

func TestRoundTrip(t *testing.T) {
	in := doc{Name: opt.Some("web"), Extra: opt.Some(0)}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"name":"web","extra":0}` {
		t.Fatalf("got %s", data)
	}
	var out doc
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("got %+v", out)
	}
	if err := json.Unmarshal([]byte(`{"name":null}`), &out); err != nil {
		t.Fatal(err)
	}
	if out.Name.IsSome() {
		t.Fatal("null is absent")
	}
}

func TestPointersAndFallbacks(t *testing.T) {
	if opt.None[int]().Ptr() != nil {
		t.Fatal("absent has no pointer")
	}
	n := 7
	v := opt.FromPtr(&n)
	n = 8
	if got, ok := v.Get(); !ok || got != 7 {
		t.Fatalf("a copy, got %d %v", got, ok)
	}
	if opt.FromPtr[int](nil).Or(3) != 3 {
		t.Fatal("fallback")
	}
}
