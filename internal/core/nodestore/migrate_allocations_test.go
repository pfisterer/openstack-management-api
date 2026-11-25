package nodestore

import (
	"testing"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
)

func legacyTree() []LegacyNode {
	root, uni, stud := "root", "uni", "stud"
	end := "2027-03-31"
	return []LegacyNode{
		{Node: model.Node{ID: root, Kind: model.KindBudget, Status: model.StatusApproved}},
		{Node: model.Node{ID: uni, Kind: model.KindBudget, ParentID: &root, Status: model.StatusApproved}},
		{Node: model.Node{ID: stud, Kind: model.KindBudget, ParentID: &uni, Status: model.StatusApproved}},
		{Node: model.Node{ID: "p_plain", Kind: model.KindProject, ParentID: &stud, Status: model.StatusApproved, Name: "Plain", Owner: "user:a@x",
			Limit: common.ProjectQuota{"cores": 4, "ipv4": 0}, TerminationDate: &end, AuthorizedUsers: []common.AuthorizedUser{{Token: "user:b@x"}},
			Bindings: map[string]model.Binding{"openstack": {ExternalID: "os-1"}}, History: []model.HistoryEntry{{Event: "created"}, {Event: "approved"}}}},
		{Node: model.Node{ID: "p_extra", Kind: model.KindProject, ParentID: &stud, Status: model.StatusApproved, Name: "Extra", Owner: "user:a@x",
			Limit: common.ProjectQuota{"cores": 2}},
			Extras: []LegacyAllocation{{BudgetID: uni, Limit: common.ProjectQuota{"cores": 8, "ipv4": 1}, Reason: "lab", GrantedBy: "m@x", GrantedAt: "2026-10-01T00:00:00Z"}}},
		{Node: model.Node{ID: "p_change", Kind: model.KindProject, ParentID: &stud, Status: model.StatusChangePending, Owner: "user:c@x",
			Limit: common.ProjectQuota{"cores": 1}, Pending: &model.PendingChanges{Limit: &common.ProjectQuota{"cores": 3}}}},
		{Node: model.Node{ID: "p_pending", Kind: model.KindProject, ParentID: &stud, Status: model.StatusPending, Owner: "user:d@x", Limit: common.ProjectQuota{"cores": 2}}},
		{Node: model.Node{ID: "p_released", Kind: model.KindProject, ParentID: &stud, Status: model.StatusReleased, Owner: "user:e@x", Limit: common.ProjectQuota{"cores": 5}}},
		{Node: model.Node{ID: "p_import", Kind: model.KindProject, ParentID: new("unassigned"), Status: model.StatusImported, Limit: common.ProjectQuota{"cores": 7}}},
	}
}

// Every project keeps its id and becomes a project outside the tree with one allocation under its former budget, its extra allocations further ones; the sum is what it held.
func TestMigrateToAllocations(t *testing.T) {
	writes, rep, err := MigrateToAllocations(legacyTree(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Mismatches) > 0 {
		t.Fatalf("mismatches: %v", rep.Mismatches)
	}
	if rep.Projects != 6 || rep.Allocations != 6 || rep.ExtraAllocations != 1 || rep.PendingChanges != 1 || rep.Imported != 1 {
		t.Errorf("report = %+v", rep)
	}
	byID := map[string]model.Node{}
	allocs := map[string][]model.Node{}
	for _, n := range writes {
		byID[n.ID] = n
		if n.Kind == model.KindAllocation {
			allocs[n.ProjectID] = append(allocs[n.ProjectID], n)
		}
	}
	plain := byID["p_plain"]
	if plain.ParentID != nil || len(plain.FundingBudgetIDs) != 1 || plain.FundingBudgetIDs[0] != "stud" || plain.Owner != "user:a@x" || len(plain.AuthorizedUsers) != 1 ||
		plain.Binding("openstack").ExternalID != "os-1" || len(plain.History) != 2 || plain.Limit["cores"] != 4 || *plain.TerminationDate != "2027-03-31" {
		t.Errorf("p_plain = %+v", plain)
	}
	main := byID[MigratedAllocationID("p_plain", "stud")]
	if main.Kind != model.KindAllocation || *main.ParentID != "stud" || main.Limit["cores"] != 4 || main.Status != model.StatusApproved || main.Owner != "user:a@x" || *main.TerminationDate != "2027-03-31" {
		t.Errorf("its allocation = %+v", main)
	}
	if extra := byID["p_extra"]; extra.Limit["cores"] != 10 || extra.Limit["ipv4"] != 1 || len(allocs["p_extra"]) != 2 {
		t.Errorf("p_extra = %v with %d allocations", extra.Limit, len(allocs["p_extra"]))
	}
	if x := byID[MigratedAllocationID("p_extra", "uni")]; x.Reason != "lab" || x.CreatedBy != "m@x" || x.Limit["cores"] != 8 {
		t.Errorf("the extra allocation = %+v", x)
	}
	ch := byID["p_change"]
	ca := byID[MigratedAllocationID("p_change", "stud")]
	if ch.Status != model.StatusApproved || ch.Pending != nil || ca.Status != model.StatusChangePending || (*ca.Pending.Limit)["cores"] != 3 || ch.Limit["cores"] != 1 {
		t.Errorf("a waiting change goes to the allocation: project %q %+v, allocation %q %+v", ch.Status, ch.Pending, ca.Status, ca.Pending)
	}
	if pend := byID[MigratedAllocationID("p_pending", "stud")]; pend.Status != model.StatusPending || pend.Limit["cores"] != 2 {
		t.Errorf("a waiting request stays one: %+v", pend)
	}
	if rel := byID["p_released"]; rel.Limit["cores"] != 5 || byID[MigratedAllocationID("p_released", "stud")].Status != model.StatusReleased {
		t.Errorf("a released project keeps what it held: %v", rel.Limit)
	}
	if imp := byID["p_import"]; imp.ParentID != nil || len(allocs["p_import"]) != 0 || imp.Limit["cores"] != 7 {
		t.Errorf("an import leaves the tree without an allocation: %+v", imp)
	}
}

// Run again on its own result it does nothing, and the ids are the same in every run (a dry run names what the real one writes).
func TestMigrateToAllocations_Idempotent(t *testing.T) {
	writes, _, _ := MigrateToAllocations(legacyTree(), time.Now())
	var again []LegacyNode
	for _, n := range writes {
		again = append(again, LegacyNode{Node: n})
	}
	more, rep, err := MigrateToAllocations(again, time.Now())
	if err != nil || len(more) != 0 || rep.Allocations != 0 {
		t.Errorf("second run wrote %d nodes (%+v, %v)", len(more), rep, err)
	}
	if MigratedAllocationID("p", "b") != MigratedAllocationID("p", "b") || MigratedAllocationID("p", "b") == MigratedAllocationID("p", "c") {
		t.Error("ids are not a function of project and budget")
	}
}

// Data migrated by the first form of the model keeps everything that pays: the attributes move to the allocations, the project's end becomes the earliest of its paying allocations, nothing ends or shrinks.
func TestReviseAllocationLeaves(t *testing.T) {
	early, late := "2026-11-23", "2027-03-31"
	nodes := []model.Node{
		{ID: "p", Kind: model.KindProject, Status: model.StatusApproved, Limit: common.ProjectQuota{"cores": 6}, TerminationDate: &late, Attributes: model.Attributes{"billing": {"wbs": "1"}}},
		{ID: "a1", Kind: model.KindAllocation, ProjectID: "p", ParentID: new("b1"), Status: model.StatusApproved, Limit: common.ProjectQuota{"cores": 2}, TerminationDate: &late, CreatedAt: "1"},
		{ID: "a2", Kind: model.KindAllocation, ProjectID: "p", ParentID: new("b2"), Status: model.StatusApproved, Limit: common.ProjectQuota{"cores": 4}, TerminationDate: &early, CreatedAt: "2", Attributes: model.Attributes{"billing": {"wbs": "2"}}},
	}
	out, err := ReviseAllocationLeaves(nodes)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]model.Node{}
	for _, n := range out {
		byID[n.ID] = n
	}
	p := byID["p"]
	if p.Attributes != nil || p.Limit["cores"] != 6 || *p.TerminationDate != early || len(p.FundingBudgetIDs) != 2 || p.FundingBudgetIDs[0] != "b1" {
		t.Errorf("project = %+v", p)
	}
	if byID["a1"].Attributes["billing"]["wbs"] != "1" || byID["a2"].Attributes["billing"]["wbs"] != "2" || byID["a2"].Status != model.StatusApproved {
		t.Errorf("allocations = %+v / %+v", byID["a1"], byID["a2"])
	}
}
