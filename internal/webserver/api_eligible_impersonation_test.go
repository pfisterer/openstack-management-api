package webserver_test

// Reproduction of the prod report "user added to Can request does not see the
// budget": a root admin creates a budget whose only eligible requester is a
// user the role provider knows nothing about beyond their own user token, then
// acts on behalf of that user — eligible-for-me must list the budget.

import (
	"net/http"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/tree"
)

func TestEligibleForMe_UnderImpersonationOfUnknownUser(t *testing.T) {
	h := setupRouter(t)

	// The ghost is no mock identity: like a fresh federated user, the role
	// provider answers with only their self token.
	const ghost = "ghost@uni.example"

	rr := do(t, h, http.MethodPost, "/v1/nodes", userRoot, tree.CreateNodeRequest{
		ParentID: model.RootNodeID, Kind: model.KindBudget, Name: "Spielbudget",
		Reason: "repro", Limit: cores(4),
		AdminScope:         []string{"user:" + userRoot},
		EligibleRequesters: []string{"user:" + ghost},
	})
	assertStatus(t, rr, http.StatusCreated)
	var budget model.Node
	mustDecode(t, rr, &budget)

	rr = do(t, h, http.MethodGet, "/v1/nodes/eligible-for-me", ghost, nil)
	assertStatus(t, rr, http.StatusOK)
	eligible := decodePage(t, rr)
	found := false
	for _, n := range eligible {
		if n.ID == budget.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("impersonated eligible requester should see the budget, got %v", nodeIDs(eligible))
	}
}
