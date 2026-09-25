package api

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

// The operations the gate lets through must be exactly the ones the
// contract's Rust handlers served without a principal; a new public
// operation is a decision, not an accident.
func TestPublicOperationsExistInTheContract(t *testing.T) {
	data, err := os.ReadFile("../../packages/api-client/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	ops := map[string]bool{}
	for _, item := range spec.Paths {
		for _, op := range item {
			ops[op.OperationID] = true
		}
	}
	for _, id := range slices.Concat(publicOperations, userOperations) {
		if !ops[id] {
			t.Errorf("%s is not an operation of the contract", id)
		}
	}
	if len(ops) < 100 {
		t.Fatalf("only %d operations read", len(ops))
	}
}
