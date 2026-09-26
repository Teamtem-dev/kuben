package jsonx_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/Teamtem-dev/kuben/internal/core/opt"
	"github.com/Teamtem-dev/kuben/internal/jsonx"
)

func object(t *testing.T, text string) jsonx.Object {
	t.Helper()
	var o jsonx.Object
	if err := json.Unmarshal([]byte(text), &o); err != nil {
		t.Fatal(err)
	}
	return o
}

func TestRequiredRefusesAbsentAndNull(t *testing.T) {
	o := object(t, `{"a":1,"b":null,"c":"x"}`)
	var n int
	if err := jsonx.Required(o, "a", &n); err != nil || n != 1 {
		t.Fatalf("got %d, %v", n, err)
	}
	var missing *jsonx.MissingError
	for _, key := range []string{"b", "z"} {
		err := jsonx.Required(o, key, &n)
		if err == nil {
			t.Fatalf("%s: absent and null are refused", key)
		}
		if !errors.As(err, &missing) || err.Error() != "missing field `"+key+"`" {
			t.Errorf("%s: got %v", key, err)
		}
	}
	if err := jsonx.Required(o, "c", &n); err == nil || errors.As(err, &missing) {
		t.Errorf("a wrong type is a decode error, got %v", err)
	}
}

func TestOptionalTakesNullAsAbsent(t *testing.T) {
	o := object(t, `{"a":2,"b":null}`)
	var v opt.Val[int]
	if err := jsonx.Optional(o, "a", &v); err != nil || v != opt.Some(2) {
		t.Fatalf("got %v, %v", v, err)
	}
	for _, key := range []string{"b", "z"} {
		v = opt.Some(9)
		if err := jsonx.Optional(o, key, &v); err != nil || v.IsSome() {
			t.Errorf("%s: got %v, %v", key, v, err)
		}
	}
}

func TestMustField(t *testing.T) {
	var doc struct {
		ID jsonx.Must[uint64] `json:"id"`
	}
	if err := json.Unmarshal([]byte(`{"id":7}`), &doc); err != nil {
		t.Fatal(err)
	}
	if id, err := doc.ID.Get("id"); err != nil || id != 7 {
		t.Fatalf("got %d, %v", id, err)
	}
	doc.ID = jsonx.Must[uint64]{}
	if err := json.Unmarshal([]byte(`{"id":null}`), &doc); err != nil {
		t.Fatal(err)
	}
	if _, err := doc.ID.Get("id"); err == nil || err.Error() != "missing field `id`" {
		t.Fatalf("got %v", err)
	}
}

func TestTakeLeavesTheRest(t *testing.T) {
	o := object(t, `{"a":1,"b":null,"rest":true}`)
	var n int
	var b opt.Val[int]
	if err := jsonx.Take(o, "a", &n); err != nil {
		t.Fatal(err)
	}
	if err := jsonx.TakeOptional(o, "b", &b); err != nil {
		t.Fatal(err)
	}
	if err := jsonx.Take(o, "missing", &n); err == nil {
		t.Fatal("still required")
	}
	if len(o) != 1 || o["rest"] == nil {
		t.Fatalf("got %v", o)
	}
}

func TestDecodeAnyKeepsNumberText(t *testing.T) {
	v, err := jsonx.DecodeAny([]byte(`{"a":1.0,"b":1,"c":[1e21]}`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := jsonx.CanonicalValue(v)
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"a":1.0,"b":1,"c":[1e+21]}` {
		t.Fatalf("got %s", got)
	}
	if _, err := jsonx.DecodeAny([]byte(`{} {}`)); err == nil {
		t.Fatal("trailing data")
	}
}
