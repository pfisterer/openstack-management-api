package tree

import (
	"encoding/json"
	"strings"
	"testing"
)

// A field sent as null removes it, one left out stays; the former clear_* flags are refused (10.11.2026).
func TestUpdateNodeRequest_NullRemoves(t *testing.T) {
	var r UpdateNodeRequest
	if err := json.Unmarshal([]byte(`{"termination_date":null,"auto_approve":null,"max_project_term_days":null,"caps":null}`), &r); err != nil {
		t.Fatal(err)
	}
	if !r.ClearTerminationDate || !r.ClearAutoApprove || !r.ClearMaxProjectTermDays || !r.ClearCaps {
		t.Fatalf("null must remove every field: %+v", r)
	}
	var left UpdateNodeRequest
	if err := json.Unmarshal([]byte(`{"name":"x","termination_date":"2026-12-31"}`), &left); err != nil {
		t.Fatal(err)
	}
	if left.ClearTerminationDate || left.ClearAutoApprove || left.ClearCaps || left.Caps != nil || left.TerminationDate == nil || *left.Name != "x" {
		t.Fatalf("fields left out must stay: %+v", left)
	}
	err := json.Unmarshal([]byte(`{"clear_auto_approve":true}`), &left)
	if err == nil || !strings.Contains(err.Error(), `"auto_approve": null`) {
		t.Fatalf("a former flag must be refused with the field to send, got %v", err)
	}
	var one UpdateNodeRequest
	if err := json.Unmarshal([]byte(`{"caps":{"a":3,"b":null}}`), &one); err != nil || one.ClearCaps || *one.Caps["a"] != 3 || one.Caps["b"] != nil || len(one.Caps) != 2 {
		t.Fatalf("a cap set by value and one removed by null: %+v, %v", one, err)
	}
	out, _ := json.Marshal(UpdateNodeRequest{})
	if string(out) != "{}" {
		t.Fatalf("an empty request must marshal without nulls, got %s", out)
	}
}
