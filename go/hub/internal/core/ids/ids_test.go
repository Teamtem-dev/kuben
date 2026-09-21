package ids_test

import (
	"encoding/json"
	"testing"

	"github.com/Teamtem-dev/kuben/go/hub/internal/core/ids"
)

func TestIDsRoundTripThroughStrings(t *testing.T) {
	id := ids.New[ids.User]()
	parsed, err := ids.Parse[ids.User](id.String())
	if err != nil {
		t.Fatal(err)
	}
	if parsed != id {
		t.Fatalf("got %s, want %s", parsed, id)
	}
	if _, err := ids.Parse[ids.User]("not-a-uuid"); err == nil {
		t.Fatal("garbage must not parse")
	}
}

func TestIDsAreTimeOrdered(t *testing.T) {
	a := ids.New[ids.Org]()
	b := ids.New[ids.Org]()
	if a.Compare(b) > 0 {
		t.Fatalf("%s was made before %s", a, b)
	}
}

func TestIDsAreTextInJSON(t *testing.T) {
	id := ids.New[ids.Release]()
	data, err := json.Marshal(map[string]ids.ReleaseID{"id": id})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"id":"` + id.String() + `"}`; string(data) != want {
		t.Fatalf("got %s, want %s", data, want)
	}
	var back map[string]ids.ReleaseID
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back["id"] != id {
		t.Fatal("round trip")
	}
}
