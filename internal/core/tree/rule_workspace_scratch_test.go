package tree_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/tree"
)

// The root holds the local storage per workspace like any budget; the budgets below inherit it, may lower it and may not raise it, reads show what is inherited, and clearing a budget's own value leaves those above in force.
func TestWorkspaceScratch_RootAndInheritance(t *testing.T) {
	f := newChangeFixture(t, nil, nil)
	root, err := f.svc.UpdateNode(model.RootNodeID, tree.UpdateNodeRequest{WorkspaceScratchGB: days(20)}, rootActor(), rootTokens)
	if err != nil {
		t.Fatalf("root admins set the root's value: %v", err)
	}
	if root.WorkspaceScratchGB == nil || *root.WorkspaceScratchGB != 20 {
		t.Fatalf("root workspace_scratch_gb = %v", root.WorkspaceScratchGB)
	}
	last := root.History[len(root.History)-1]
	if last.WorkspaceScratchGBFrom != nil || last.WorkspaceScratchGBTo == nil || *last.WorkspaceScratchGBTo != 20 {
		t.Errorf("the history should record the change, got %+v", last)
	}

	b := mustGet(t, f.svc, f.budget.ID)
	if b.InheritedWorkspaceScratchGB == nil || *b.InheritedWorkspaceScratchGB != 20 || b.WorkspaceScratchGB != nil {
		t.Fatalf("the budget should inherit the root's value, got own %v inherited %v", b.WorkspaceScratchGB, b.InheritedWorkspaceScratchGB)
	}
	if _, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{WorkspaceScratchGB: days(30)}, rootActor(), rootTokens); err == nil || !strings.Contains(err.Error(), "at most 20 GB") {
		t.Errorf("raising it below the root should be refused, got %v", err)
	}
	for _, bad := range []int{0, -1, model.MaxWorkspaceScratchGB + 1} {
		if _, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{WorkspaceScratchGB: days(bad)}, rootActor(), rootTokens); err == nil {
			t.Errorf("%d GB should be refused", bad)
		}
	}
	if _, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{WorkspaceScratchGB: days(5)}, rootActor(), rootTokens); err != nil {
		t.Fatalf("lowering should be allowed: %v", err)
	}

	sub := mustCreate(t, f.svc, tree.CreateNodeRequest{ParentID: f.budget.ID, Kind: model.KindBudget, Limit: cores(1)})
	if sub.WorkspaceScratchGB != nil {
		t.Errorf("nothing is copied down, got %v", *sub.WorkspaceScratchGB)
	}
	if got := mustGet(t, f.svc, sub.ID).InheritedWorkspaceScratchGB; got == nil || *got != 5 {
		t.Errorf("inherited = %v, want the smallest of root and budget (5)", got)
	}
	if _, err := f.svc.CreateNode(tree.CreateNodeRequest{ParentID: f.budget.ID, Kind: model.KindBudget, Name: "n", Reason: "t", Limit: cores(1),
		AdminScope: common.TokenList{"group:root"}, WorkspaceScratchGB: days(10)}, rootActor(), "root@x", rootTokens); err == nil {
		t.Error("creating a sub-budget with more than the budget above should be refused")
	}

	// Raising the root's value later loosens nothing below: the budget's own 5 still applies.
	if _, err := f.svc.UpdateNode(model.RootNodeID, tree.UpdateNodeRequest{WorkspaceScratchGB: days(50)}, rootActor(), rootTokens); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, f.svc, sub.ID).InheritedWorkspaceScratchGB; got == nil || *got != 5 {
		t.Errorf("inherited after raising the root = %v, want 5", got)
	}

	cleared, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{ClearWorkspaceScratchGB: true}, rootActor(), rootTokens)
	if err != nil || cleared.WorkspaceScratchGB != nil {
		t.Fatalf("clearing: %v, %v", err, cleared.WorkspaceScratchGB)
	}
	if got := mustGet(t, f.svc, sub.ID).InheritedWorkspaceScratchGB; got == nil || *got != 50 {
		t.Errorf("inherited after clearing = %v, want the root's 50", got)
	}
}

// Who may set it: the budget's managers and those above, like the other policy fields; not those who may only request, and not on a project.
func TestWorkspaceScratch_Authz(t *testing.T) {
	f := newChangeFixture(t, nil, nil)
	if _, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{WorkspaceScratchGB: days(3)}, model.UIActor("stud@x"), studTokens); !errors.Is(err, common.ErrForbidden) {
		t.Errorf("a requester should not set it, got %v", err)
	}
	if _, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{ClearWorkspaceScratchGB: true}, model.UIActor("stud@x"), studTokens); !errors.Is(err, common.ErrForbidden) {
		t.Errorf("a requester should not clear it, got %v", err)
	}
	mgr := common.TokenList{"user:mgr@x"}
	own := mustCreate(t, f.svc, tree.CreateNodeRequest{ParentID: f.budget.ID, Kind: model.KindBudget, Limit: cores(1), AdminScope: mgr})
	if _, err := f.svc.UpdateNode(own.ID, tree.UpdateNodeRequest{WorkspaceScratchGB: days(3)}, model.UIActor("mgr@x"), mgr); err != nil {
		t.Errorf("a budget's own managers set it: %v", err)
	}
	p := f.approved(t, 1, nil)
	if _, err := f.svc.UpdateNode(p.ID, tree.UpdateNodeRequest{WorkspaceScratchGB: days(3)}, rootActor(), rootTokens); err == nil {
		t.Error("workspace_scratch_gb on a project should be refused")
	}
	if _, err := f.svc.CreateNode(tree.CreateNodeRequest{ParentID: f.budget.ID, Kind: model.KindProject, Name: "p", Reason: "t", Limit: cores(1), WorkspaceScratchGB: days(3)}, rootActor(), "root@x", rootTokens); err == nil {
		t.Error("workspace_scratch_gb on a new project should be refused")
	}
	if got := mustGet(t, f.svc, p.ID).InheritedWorkspaceScratchGB; got != nil {
		t.Errorf("nothing set above the project, got %v", *got)
	}
}
