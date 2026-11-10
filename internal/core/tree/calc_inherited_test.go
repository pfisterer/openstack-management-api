package tree_test

import (
	"reflect"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/tree"
)

// The inherited_caps of a response are what model.ResolveCaps gives over the budgets above the node, loaded one by one as the checks load them: for the root, a budget, a budget that inherits its limit, a sub-budget and a project.
func TestInherited_SameAsResolveOverTheChain(t *testing.T) {
	svc, _ := newSvc(t, rootTokens)
	if err := svc.SetCaps(testCaps); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateNode(model.RootNodeID, tree.UpdateNodeRequest{Caps: set("jobs", 4, "hours", 24)}, rootActor(), rootTokens); err != nil {
		t.Fatalf("root batch jobs: %v", err)
	}
	a := mustCreate(t, svc, tree.CreateNodeRequest{ParentID: model.RootNodeID, Kind: model.KindBudget, Limit: cores(10), Caps: common.Caps{"scratch": 10, "prio": 1, "jobs": 2, "hours": 12, "cpu-max": 60}})
	pass := mustCreate(t, svc, tree.CreateNodeRequest{ParentID: a.ID, Kind: model.KindBudget, InheritsLimit: true, Caps: common.Caps{"prio": 0}})
	b := mustCreate(t, svc, tree.CreateNodeRequest{ParentID: pass.ID, Kind: model.KindBudget, Limit: cores(5), Caps: common.Caps{"scratch": 5, "jobs": 1, "hours": 12, "cpu-max": 30, "gpu-max": 8}})
	p := mustCreate(t, svc, tree.CreateNodeRequest{ParentID: b.ID, Kind: model.KindProject, Limit: cores(1)})

	for _, id := range []string{model.RootNodeID, a.ID, pass.ID, b.ID, p.ID} {
		n := mustGet(t, svc, id)
		var above []model.Node
		for parent := n.ParentID; parent != nil; {
			up := mustGet(t, svc, *parent)
			above = append(above, up)
			parent = up.ParentID
		}
		if got, want := n.InheritedCaps, model.ResolveCaps(above, testCaps); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: inherited_caps = %+v, want %+v", n.Name, got, want)
		}
	}
	// And the values themselves, so the test does not pass on two nils.
	want := common.Caps{"scratch": 5, "prio": 0, "jobs": 1, "hours": 12, "cpu-max": 30, "gpu-max": 8}
	if got := mustGet(t, svc, p.ID).InheritedCaps; !reflect.DeepEqual(got, want) {
		t.Errorf("project: %+v, want %+v", got, want)
	}
}
