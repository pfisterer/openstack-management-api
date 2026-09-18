package webserver_test

import (
	"net/http"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/tree"
)

// A project may only hold an availability its budget holds. The UI hides what a
// budget does not carry, but that is presentation: the API has to refuse it, or
// anyone who may request under an auto-approve budget grants themselves the
// availability by calling the API directly.
func TestLeafCannotTakeAvailabilityItsBudgetLacks(t *testing.T) {
	h := setupRouter(t)

	rr := do(t, h, http.MethodPost, "/v1/nodes", userStudent, tree.CreateNodeRequest{
		ParentID: "b_cs_students", Kind: tree.KindProject, Name: "sneaky",
		Reason: "wants public ipv4", Limit: common.ProjectQuota{"cores": 1, "ram": 1, "storage": 1, "dhbw-ipv4": 1},
	})
	if rr.Code < 400 {
		var n tree.Node
		mustDecode(t, rr, &n)
		t.Fatalf("expected a 4xx for an availability the budget does not hold, got %d with status %q", rr.Code, n.Status)
	}
}

// The rule must not overreach: under a budget that holds the availability, a
// requester may ask for it.
func TestLeafMayTakeAvailabilityItsBudgetHolds(t *testing.T) {
	h := setupRouter(t)

	rr := do(t, h, http.MethodPost, "/v1/nodes", userRoot, tree.CreateNodeRequest{
		ParentID: tree.RootNodeID, Kind: tree.KindBudget, Name: "Networked lab",
		Reason: "lab with public ipv4", Limit: common.ProjectQuota{"cores": 4, "ram": 8, "storage": 20, "dhbw-ipv4": 1},
		AdminScope:         common.TokenList{"user:" + userRoot},
		EligibleRequesters: common.TokenList{"user:" + userStudent},
	})
	assertStatus(t, rr, http.StatusCreated)
	var budget tree.Node
	mustDecode(t, rr, &budget)

	rr = do(t, h, http.MethodPost, "/v1/nodes", userStudent, tree.CreateNodeRequest{
		ParentID: budget.ID, Kind: tree.KindProject, Name: "with ipv4",
		Reason: "needs public ipv4", Limit: common.ProjectQuota{"cores": 1, "ram": 1, "storage": 1, "dhbw-ipv4": 1},
	})
	assertStatus(t, rr, http.StatusCreated)
}
