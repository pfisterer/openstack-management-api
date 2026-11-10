package tree

import (
	"encoding/json"
	"strings"
	"testing"
)

// A field sent as null removes it, one left out stays; the former clear_* flags are refused (10.11.2026).
func TestUpdateNodeRequest_NullRemoves(t *testing.T) {
	var r UpdateNodeRequest
	if err := json.Unmarshal([]byte(`{"termination_date":null,"auto_approve":null,"max_project_term_days":null,"workspace_scratch_gb":null,"batch_priority":null,"batch_jobs":null,"release_max":null}`), &r); err != nil {
		t.Fatal(err)
	}
	if !r.ClearTerminationDate || !r.ClearAutoApprove || !r.ClearMaxProjectTermDays || !r.ClearWorkspaceScratchGB || !r.ClearBatchPriority || !r.ClearBatchJobs || r.ReleaseMax == nil {
		t.Fatalf("null must remove every field: %+v", r)
	}
	var left UpdateNodeRequest
	if err := json.Unmarshal([]byte(`{"name":"x","termination_date":"2026-12-31"}`), &left); err != nil {
		t.Fatal(err)
	}
	if left.ClearTerminationDate || left.ClearAutoApprove || left.ClearBatchJobs || left.ReleaseMax != nil || left.TerminationDate == nil || *left.Name != "x" {
		t.Fatalf("fields left out must stay: %+v", left)
	}
	err := json.Unmarshal([]byte(`{"clear_batch_jobs":true}`), &left)
	if err == nil || !strings.Contains(err.Error(), `"batch_jobs": null`) {
		t.Fatalf("a former flag must be refused with the field to send, got %v", err)
	}
	out, _ := json.Marshal(UpdateNodeRequest{})
	if string(out) != "{}" {
		t.Fatalf("an empty request must marshal without nulls, got %s", out)
	}
}
