package protocol_test

import (
	"encoding/json"
	"testing"

	"github.com/Teamtem-dev/kuben/go/kubenapi/protocol"
)

// The wire form of the Rust `Apply` (serde camelCase).
func TestApplyWireForm(t *testing.T) {
	data, err := json.Marshal(protocol.Apply{Target: "t", Namespace: "kb-shop-prod", Name: "web", Spec: "{}"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != `{"target":"t","namespace":"kb-shop-prod","name":"web","spec":"{}"}` {
		t.Fatal(got)
	}
}
