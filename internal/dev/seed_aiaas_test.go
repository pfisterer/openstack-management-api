package dev

import (
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
)

// The AI part of the seed only appears with AI resources in the catalogue, and then keeps every child within its parent.
func TestWithProviderBudget(t *testing.T) {
	_, plain := DefaultMockTreeState()
	if got := WithProviderBudget(plain, nil); len(got) != len(plain) {
		t.Fatalf("without AI resources the seed changed: %d → %d nodes", len(plain), len(got))
	}
	catalog := []common.Resource{
		{ID: "ws-vcpu-hours", Provider: "aiaas", Kind: common.KindWindow, Window: "30d"},
		{ID: "llm-mtokens", Provider: "aiaas", Kind: common.KindWindow, Window: "30d"},
	}
	_, base := DefaultMockTreeState()
	nodes := WithProviderBudget(base, catalog)
	byID := map[string]model.Node{}
	for _, n := range nodes {
		byID[n.ID] = n
	}
	for _, id := range []string{AIBudgetID, AICourseBudgetID, "p_ai_course", "p_ai_personal", "p_ai_research", "p_ai_thesis", "p_mixed"} {
		if _, ok := byID[id]; !ok {
			t.Errorf("missing %s", id)
		}
	}
	if _, ok := byID["p_ai_course"].Limit["ws-gpu-hours-l4-3q"]; ok {
		t.Errorf("a resource the catalogue lacks made it into a limit: %v", byID["p_ai_course"].Limit)
	}
	if byID["p_mixed"].Limit["cores"] == 0 || byID["p_mixed"].Limit["llm-mtokens"] == 0 {
		t.Errorf("the mixed project is not both: %v", byID["p_mixed"].Limit)
	}
	// Children within their parent, per resource of the AI service.
	sum := map[string]common.ProjectQuota{}
	for _, n := range nodes {
		if n.ParentID == nil || n.Status == model.StatusPending {
			continue
		}
		if sum[*n.ParentID] == nil {
			sum[*n.ParentID] = common.ProjectQuota{}
		}
		for _, r := range catalog {
			sum[*n.ParentID][r.ID] += n.Limit[r.ID]
		}
	}
	for parent, used := range sum {
		for id, v := range used {
			if l := byID[parent].Limit[id]; l != common.UnlimitedQuota && v > l {
				t.Errorf("%s: children hold %d %s, the budget %d", parent, v, id, l)
			}
		}
	}
}
