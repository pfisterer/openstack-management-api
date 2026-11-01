package tree_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/tree"
)

// The root holds the batch priority like any budget; the budgets below inherit it, may tighten it and may not loosen it, reads show what is inherited, and clearing a budget's own value leaves those above in force.
func TestBatchPriority_RootAndInheritance(t *testing.T) {
	f := newChangeFixture(t, nil, nil)
	guaranteed, preemptible := new(model.BatchPriorityGuaranteed), new(model.BatchPriorityPreemptible)
	root, err := f.svc.UpdateNode(model.RootNodeID, tree.UpdateNodeRequest{BatchPriority: guaranteed}, rootActor(), rootTokens)
	if err != nil {
		t.Fatalf("root admins set the root's value: %v", err)
	}
	if root.BatchPriority == nil || *root.BatchPriority != model.BatchPriorityGuaranteed {
		t.Fatalf("root batch_priority = %v", root.BatchPriority)
	}
	last := root.History[len(root.History)-1]
	if last.BatchPriorityFrom != nil || last.BatchPriorityTo == nil || *last.BatchPriorityTo != model.BatchPriorityGuaranteed {
		t.Errorf("the history should record the change, got %+v", last)
	}

	b := mustGet(t, f.svc, f.budget.ID)
	if b.InheritedBatchPriority == nil || *b.InheritedBatchPriority != model.BatchPriorityGuaranteed || b.BatchPriority != nil {
		t.Fatalf("the budget should inherit the root's value, got own %v inherited %v", b.BatchPriority, b.InheritedBatchPriority)
	}
	for _, bad := range []string{"", "high", "Preemptible"} {
		if _, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{BatchPriority: new(bad)}, rootActor(), rootTokens); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
	if _, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{BatchPriority: preemptible}, rootActor(), rootTokens); err != nil {
		t.Fatalf("tightening should be allowed: %v", err)
	}

	sub := mustCreate(t, f.svc, tree.CreateNodeRequest{ParentID: f.budget.ID, Kind: model.KindBudget, Limit: cores(1)})
	if sub.BatchPriority != nil {
		t.Errorf("nothing is copied down, got %v", *sub.BatchPriority)
	}
	if got := mustGet(t, f.svc, sub.ID).InheritedBatchPriority; got == nil || *got != model.BatchPriorityPreemptible {
		t.Errorf("inherited = %v, want the strictest of root and budget (preemptible)", got)
	}
	if _, err := f.svc.UpdateNode(sub.ID, tree.UpdateNodeRequest{BatchPriority: guaranteed}, rootActor(), rootTokens); err == nil || !strings.Contains(err.Error(), "preemptible") {
		t.Errorf("guaranteeing below a preemptible budget should be refused, got %v", err)
	}
	if _, err := f.svc.CreateNode(tree.CreateNodeRequest{ParentID: f.budget.ID, Kind: model.KindBudget, Name: "n", Reason: "t", Limit: cores(1),
		AdminScope: common.TokenList{"group:root"}, BatchPriority: guaranteed}, rootActor(), "root@x", rootTokens); err == nil {
		t.Error("creating a guaranteed sub-budget below a preemptible budget should be refused")
	}

	// Making the root preemptible as well changes nothing below; the budget's own value still applies.
	if _, err := f.svc.UpdateNode(model.RootNodeID, tree.UpdateNodeRequest{BatchPriority: preemptible}, rootActor(), rootTokens); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, f.svc, sub.ID).InheritedBatchPriority; got == nil || *got != model.BatchPriorityPreemptible {
		t.Errorf("inherited after changing the root = %v, want preemptible", got)
	}
	if _, err := f.svc.UpdateNode(model.RootNodeID, tree.UpdateNodeRequest{BatchPriority: guaranteed}, rootActor(), rootTokens); err != nil {
		t.Fatal(err)
	}

	cleared, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{ClearBatchPriority: true}, rootActor(), rootTokens)
	if err != nil || cleared.BatchPriority != nil {
		t.Fatalf("clearing: %v, %v", err, cleared.BatchPriority)
	}
	if got := mustGet(t, f.svc, sub.ID).InheritedBatchPriority; got == nil || *got != model.BatchPriorityGuaranteed {
		t.Errorf("inherited after clearing = %v, want the root's guaranteed", got)
	}
	if _, err := f.svc.UpdateNode(sub.ID, tree.UpdateNodeRequest{BatchPriority: guaranteed}, rootActor(), rootTokens); err != nil {
		t.Errorf("guaranteed below a guaranteed chain should be allowed: %v", err)
	}
}

// Who may set it: the budget's managers and those above, like the other policy fields; not those who may only request, and not on a project.
func TestBatchPriority_Authz(t *testing.T) {
	f := newChangeFixture(t, nil, nil)
	preemptible := new(model.BatchPriorityPreemptible)
	if _, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{BatchPriority: preemptible}, model.UIActor("stud@x"), studTokens); !errors.Is(err, common.ErrForbidden) {
		t.Errorf("a requester should not set it, got %v", err)
	}
	if _, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{ClearBatchPriority: true}, model.UIActor("stud@x"), studTokens); !errors.Is(err, common.ErrForbidden) {
		t.Errorf("a requester should not clear it, got %v", err)
	}
	mgr := common.TokenList{"user:mgr@x"}
	own := mustCreate(t, f.svc, tree.CreateNodeRequest{ParentID: f.budget.ID, Kind: model.KindBudget, Limit: cores(1), AdminScope: mgr})
	if _, err := f.svc.UpdateNode(own.ID, tree.UpdateNodeRequest{BatchPriority: preemptible}, model.UIActor("mgr@x"), mgr); err != nil {
		t.Errorf("a budget's own managers set it: %v", err)
	}
	p := f.approved(t, 1, nil)
	if _, err := f.svc.UpdateNode(p.ID, tree.UpdateNodeRequest{BatchPriority: preemptible}, rootActor(), rootTokens); err == nil {
		t.Error("batch_priority on a project should be refused")
	}
	if _, err := f.svc.CreateNode(tree.CreateNodeRequest{ParentID: f.budget.ID, Kind: model.KindProject, Name: "p", Reason: "t", Limit: cores(1), BatchPriority: preemptible}, rootActor(), "root@x", rootTokens); err == nil || !strings.Contains(err.Error(), "batch_priority is a budget setting") {
		t.Errorf("batch_priority on a new project should be refused, got %v", err)
	}
	if got := mustGet(t, f.svc, p.ID).InheritedBatchPriority; got != nil {
		t.Errorf("nothing set above the project, got %v", *got)
	}
}
