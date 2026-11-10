package tree_test

import (
	"errors"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/tree"
)

// Management rights are decided on the chain an operation loads (managesInChain): a budget's own managers manage it and everything below, but decide nothing about it; the managers above decide. Checked on each operation that asks on the loaded chain.
func TestAuthz_ManagementOnTheLoadedChain(t *testing.T) {
	svc, _ := newSvc(t, rootTokens)
	uniT := common.TokenList{"user:uni@x", "group:uni"}
	deptT := common.TokenList{"user:dept@x", "group:dept"}
	otherT := common.TokenList{"user:other@x"}
	studT := common.TokenList{"user:stud@x"}

	uni := mustCreate(t, svc, tree.CreateNodeRequest{ParentID: model.RootNodeID, Kind: model.KindBudget, Name: "uni", Limit: cores(20), AdminScope: common.TokenList{"group:uni"}})
	dept := mustCreate(t, svc, tree.CreateNodeRequest{ParentID: uni.ID, Kind: model.KindBudget, Name: "dept", Limit: cores(10), AdminScope: common.TokenList{"group:dept"}, EligibleRequesters: common.TokenList{"user:stud@x"}})
	other := mustCreate(t, svc, tree.CreateNodeRequest{ParentID: uni.ID, Kind: model.KindBudget, Name: "other", Limit: cores(5), AdminScope: common.TokenList{"group:uni"}})

	// CreateNode: a manager further up creates directly, an eligible requester asks, anybody else is refused.
	byUni, err := svc.CreateNode(tree.CreateNodeRequest{ParentID: dept.ID, Kind: model.KindProject, Name: "p", Reason: "t", Limit: cores(1)}, model.UIActor("uni@x"), "uni@x", uniT)
	if err != nil || byUni.Status != model.StatusApproved {
		t.Fatalf("a manager above should create directly: %v / %q", err, byUni.Status)
	}
	byStud, err := svc.CreateNode(tree.CreateNodeRequest{ParentID: dept.ID, Kind: model.KindProject, Name: "q", Reason: "t", Limit: cores(1)}, model.UIActor("stud@x"), "stud@x", studT)
	if err != nil || byStud.Status != model.StatusPending {
		t.Fatalf("an eligible requester should ask: %v / %q", err, byStud.Status)
	}
	if _, err := svc.CreateNode(tree.CreateNodeRequest{ParentID: dept.ID, Kind: model.KindProject, Name: "r", Reason: "t", Limit: cores(1)}, model.UIActor("other@x"), "other@x", otherT); !errors.Is(err, common.ErrForbidden) {
		t.Fatalf("a stranger should be refused, got %v", err)
	}

	// UpdateNode: the budget's own managers edit its policy, not its limit; those above edit both.
	if _, err := svc.UpdateNode(dept.ID, tree.UpdateNodeRequest{Name: new("dept 2")}, model.UIActor("dept@x"), deptT); err != nil {
		t.Fatalf("own managers rename: %v", err)
	}
	if _, err := svc.UpdateNode(dept.ID, tree.UpdateNodeRequest{Limit: quota(cores(11))}, model.UIActor("dept@x"), deptT); !errors.Is(err, common.ErrForbidden) {
		t.Fatalf("own managers must not raise their limit, got %v", err)
	}
	if _, err := svc.UpdateNode(dept.ID, tree.UpdateNodeRequest{Limit: quota(cores(11))}, model.UIActor("uni@x"), uniT); err != nil {
		t.Fatalf("a manager above sets the limit: %v", err)
	}
	if _, err := svc.UpdateNode(dept.ID, tree.UpdateNodeRequest{Name: new("x")}, model.UIActor("other@x"), otherT); !errors.Is(err, common.ErrForbidden) {
		t.Fatalf("a stranger must not edit, got %v", err)
	}

	// ApproveNode: not the budget's own managers for a request of the budget, the managers above.
	change, err := svc.RequestChange(dept.ID, tree.ChangeNodeRequest{Limit: quota(cores(12))}, model.UIActor("dept@x"), deptT)
	if err != nil || change.Status != model.StatusChangePending {
		t.Fatalf("own managers ask for more: %v / %q", err, change.Status)
	}
	if _, err := svc.RequestChange(dept.ID, tree.ChangeNodeRequest{Limit: quota(cores(12))}, model.UIActor("other@x"), otherT); !errors.Is(err, common.ErrForbidden) {
		t.Fatalf("a stranger must not ask for a budget, got %v", err)
	}
	if _, err := svc.ApproveNode(dept.ID, tree.ApproveNodeRequest{ExpectedRevision: new(change.CurrentRevision())}, model.UIActor("dept@x"), deptT); !errors.Is(err, common.ErrForbidden) {
		t.Fatalf("own managers must not approve their own request, got %v", err)
	}
	if _, err := svc.ApproveNode(dept.ID, tree.ApproveNodeRequest{ExpectedRevision: new(change.CurrentRevision())}, model.UIActor("uni@x"), uniT); err != nil {
		t.Fatalf("a manager above approves: %v", err)
	}
	// The budget's managers approve what is asked below it.
	if _, err := svc.ApproveNode(byStud.ID, tree.ApproveNodeRequest{ExpectedRevision: new(byStud.CurrentRevision())}, model.UIActor("dept@x"), deptT); err != nil {
		t.Fatalf("the budget's managers approve a project in it: %v", err)
	}

	// RequestChange on a project: its owner, and the managers above, may ask; the budget's managers decide whether members are a manager's choice.
	if _, err := svc.RequestChange(byStud.ID, tree.ChangeNodeRequest{Limit: quota(cores(1)), Reason: new("same")}, model.UIActor("other@x"), otherT); !errors.Is(err, common.ErrForbidden) {
		t.Fatalf("a stranger must not ask for a project, got %v", err)
	}
	if _, err := svc.RequestChange(byStud.ID, tree.ChangeNodeRequest{TerminationDate: ptr(dec2026)}, model.UIActor("dept@x"), deptT); err != nil {
		t.Fatalf("a manager of the budget may ask for a project in it: %v", err)
	}

	// Moving: taking the project away needs the chain above it, putting it somewhere the target and above.
	if _, err := svc.ReparentNode(byUni.ID, tree.ReparentNodeRequest{NewParentID: other.ID}, model.UIActor("dept@x"), deptT); !errors.Is(err, common.ErrForbidden) {
		t.Fatalf("the source budget's managers must not move it to a budget they do not manage, got %v", err)
	}
	if _, err := svc.ReparentNode(byUni.ID, tree.ReparentNodeRequest{NewParentID: other.ID}, model.UIActor("uni@x"), uniT); err != nil {
		t.Fatalf("a manager of both moves it: %v", err)
	}

	// AllocationSources: a stranger is refused, the owner gets none.
	if _, err := svc.AllocationSources(byStud.ID, otherT); !errors.Is(err, common.ErrForbidden) {
		t.Fatalf("a stranger must not list allocation sources, got %v", err)
	}
	if got, err := svc.AllocationSources(byStud.ID, studT); err != nil || len(got) != 0 {
		t.Fatalf("the owner gets no sources: %v / %d", err, len(got))
	}
	if got, err := svc.AllocationSources(byStud.ID, uniT); err != nil || len(got) != 1 || got[0].ID != uni.ID {
		t.Fatalf("a manager above gets the budget it manages above the project's own: %v / %+v", err, got)
	}
}
