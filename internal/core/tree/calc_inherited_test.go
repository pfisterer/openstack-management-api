package tree_test

import (
	"reflect"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/tree"
)

// The inherited_* fields of a response are what model.Resolve* gives over the budgets above the node, loaded one by one as the checks load them: for the root, a budget, a budget that inherits its limit, a sub-budget and a project.
func TestInherited_SameAsResolveOverTheChain(t *testing.T) {
	svc, _ := newSvc(t, rootTokens)
	if _, err := svc.UpdateNode(model.RootNodeID, tree.UpdateNodeRequest{BatchJobs: &model.BatchJobs{JobsPerPerson: 4, MaxHours: 24}}, rootActor(), rootTokens); err != nil {
		t.Fatalf("root batch jobs: %v", err)
	}
	a := mustCreate(t, svc, tree.CreateNodeRequest{ParentID: model.RootNodeID, Kind: model.KindBudget, Limit: cores(10),
		WorkspaceScratchGB: new(10), BatchPriority: new(model.BatchPriorityGuaranteed), BatchJobs: &model.BatchJobs{JobsPerPerson: 2, MaxHours: 12},
		ReleaseMax: &model.ReleaseMax{CPU: model.ReleaseTimes{IdleOpenMinutes: new(60)}}})
	pass := mustCreate(t, svc, tree.CreateNodeRequest{ParentID: a.ID, Kind: model.KindBudget, InheritsLimit: true, BatchPriority: new(model.BatchPriorityPreemptible)})
	b := mustCreate(t, svc, tree.CreateNodeRequest{ParentID: pass.ID, Kind: model.KindBudget, Limit: cores(5),
		WorkspaceScratchGB: new(5), BatchJobs: &model.BatchJobs{JobsPerPerson: 1, MaxHours: 12},
		ReleaseMax: &model.ReleaseMax{CPU: model.ReleaseTimes{IdleOpenMinutes: new(30), MaxHours: new(8)}}})
	p := mustCreate(t, svc, tree.CreateNodeRequest{ParentID: b.ID, Kind: model.KindProject, Limit: cores(1)})

	for _, id := range []string{model.RootNodeID, a.ID, pass.ID, b.ID, p.ID} {
		n := mustGet(t, svc, id)
		var above []model.Node
		for parent := n.ParentID; parent != nil; {
			up := mustGet(t, svc, *parent)
			above = append(above, up)
			parent = up.ParentID
		}
		if got, want := n.InheritedReleaseMax, model.ResolveReleaseMax(above); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: inherited_release_max = %+v, want %+v", n.Name, got, want)
		}
		if got, want := n.InheritedWorkspaceScratchGB, model.ResolveWorkspaceScratchGB(above); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: inherited_workspace_scratch_gb = %v, want %v", n.Name, got, want)
		}
		if got, want := n.InheritedBatchPriority, model.ResolveBatchPriority(above); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: inherited_batch_priority = %v, want %v", n.Name, got, want)
		}
		if got, want := n.InheritedBatchJobs, model.ResolveBatchJobs(above); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: inherited_batch_jobs = %+v, want %+v", n.Name, got, want)
		}
	}
	// And the values themselves, so the test does not pass on two nils.
	if got := mustGet(t, svc, p.ID); got.InheritedWorkspaceScratchGB == nil || *got.InheritedWorkspaceScratchGB != 5 || got.InheritedBatchPriority == nil || *got.InheritedBatchPriority != model.BatchPriorityPreemptible || got.InheritedBatchJobs == nil || got.InheritedBatchJobs.JobsPerPerson != 1 {
		t.Errorf("project: scratch %v, priority %v, batch jobs %+v", got.InheritedWorkspaceScratchGB, got.InheritedBatchPriority, got.InheritedBatchJobs)
	}
}
