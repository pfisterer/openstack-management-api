package tree

import (
	"context"
	"reflect"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
)

// Two budgets of the same name are told apart by the path above them, and the
// root, where every path starts, is left out.
func TestAttachParentNames_PathsBelowTheRoot(t *testing.T) {
	svc := availabilityService(t)
	ctx := context.Background()
	if err := svc.Bootstrap(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	root, cas, ma := model.RootNodeID, "cas", "ma"
	casLecture, maLecture := "cas-lecture", "ma-lecture"
	for _, n := range []model.Node{
		{ID: cas, Kind: model.KindBudget, ParentID: &root, Name: "CAS", Status: model.StatusApproved},
		{ID: ma, Kind: model.KindBudget, ParentID: &root, Name: "DHBW Mannheim", Status: model.StatusApproved},
		{ID: casLecture, Kind: model.KindBudget, ParentID: &cas, Name: "Vorlesung", Status: model.StatusApproved},
		{ID: maLecture, Kind: model.KindBudget, ParentID: &ma, Name: "Vorlesung", Status: model.StatusApproved},
	} {
		if err := svc.store.UpsertNode(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	nodes := []model.Node{
		{ID: "p1", Kind: model.KindProject, ParentID: &casLecture},
		{ID: "p2", Kind: model.KindProject, ParentID: &maLecture,
			Allocations: []model.Allocation{{BudgetID: ma, Limit: common.ProjectQuota{"cores": 1}}}},
		{ID: "top", Kind: model.KindBudget, ParentID: &root},
	}
	got, err := svc.attachParentNames(ctx, nodes, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]model.PathEntry{
		"p1": {{ID: cas, Name: "CAS"}, {ID: casLecture, Name: "Vorlesung"}},
		"p2": {{ID: ma, Name: "DHBW Mannheim"}, {ID: maLecture, Name: "Vorlesung"}},
	}
	for _, n := range got[:2] {
		if !reflect.DeepEqual(n.ParentPath, want[n.ID]) || n.ParentName != "Vorlesung" {
			t.Errorf("%s: parent %q path %+v, want %+v", n.ID, n.ParentName, n.ParentPath, want[n.ID])
		}
	}
	if a := got[1].Allocations[0]; a.BudgetName != "DHBW Mannheim" || !reflect.DeepEqual(a.BudgetPath, []model.PathEntry{{ID: ma, Name: "DHBW Mannheim"}}) {
		t.Errorf("allocation = %+v", a)
	}
	if len(got[2].ParentPath) != 0 || got[2].ParentName == "" {
		t.Errorf("under the root: name %q path %+v, want the name and no path", got[2].ParentName, got[2].ParentPath)
	}
}
