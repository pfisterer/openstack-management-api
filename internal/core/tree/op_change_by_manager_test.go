package tree_test

import (
	"context"
	"strings"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/nodestore"
	"github.com/pfisterer/openstack-management-api/internal/core/tree"
)

// managerChangeFixture is a department budget "Lehre" of 10 cores managed by lect@x, with a course budget "Kurs" of 6 cores below it managed by ta@x, and an active project of 2 cores owned by stud@x in the course. No auto-approve anywhere: whatever applies at once does so because of who asks.
type managerChangeFixture struct {
	svc     *tree.Service
	store   nodestore.Store
	course  model.Node
	project model.Node
}

var (
	lectTokens = common.TokenList{"user:lect@x"}
	taTokens   = common.TokenList{"user:ta@x"}
)

func newManagerChangeFixture(t *testing.T) managerChangeFixture {
	t.Helper()
	svc, store := newSvc(t, common.TokenList{"group:root"})
	dept, err := svc.CreateNode(tree.CreateNodeRequest{
		ParentID: model.RootNodeID, Kind: model.KindBudget, Name: "Lehre", Reason: "t",
		Limit: cores(10), AdminScope: common.TokenList{"user:lect@x"},
	}, model.UIActor("root@x"), "root@x", rootTokens)
	if err != nil {
		t.Fatalf("create department: %v", err)
	}
	course, err := svc.CreateNode(tree.CreateNodeRequest{
		ParentID: dept.ID, Kind: model.KindBudget, Name: "Kurs", Reason: "t",
		Limit: cores(6), AdminScope: common.TokenList{"user:ta@x"},
		EligibleRequesters: common.TokenList{"user:stud@x"},
	}, model.UIActor("lect@x"), "lect@x", lectTokens)
	if err != nil {
		t.Fatalf("create course: %v", err)
	}
	p, err := svc.CreateNode(tree.CreateNodeRequest{
		ParentID: course.ID, Kind: model.KindProject, Name: "vm", Reason: "vm", Limit: cores(2),
	}, model.UIActor("stud@x"), "stud@x", studTokens)
	if err != nil {
		t.Fatalf("request project: %v", err)
	}
	p, err = svc.ApproveNode(p.ID, tree.ApproveNodeRequest{ExpectedRevision: new(p.CurrentRevision())}, model.UIActor("ta@x"), taTokens)
	if err != nil {
		t.Fatalf("approve project: %v", err)
	}
	return managerChangeFixture{svc: svc, store: store, course: course, project: p}
}

func (f managerChangeFixture) stored(t *testing.T) model.Node {
	t.Helper()
	n, err := f.store.GetNode(context.Background(), f.project.ID)
	if err != nil || n == nil {
		t.Fatalf("load project: %v", err)
	}
	return *n
}

// A manager of a budget above the project — here the one above its own budget — could approve the change, so it applies at once, recorded as their approval rather than the policy's.
func TestChangeByManager_AboveAppliesAtOnce(t *testing.T) {
	f := newManagerChangeFixture(t)
	n, err := f.svc.RequestChange(f.project.ID, tree.ChangeNodeRequest{Limit: quota(cores(5))}, model.UIActor("lect@x"), lectTokens)
	if err != nil {
		t.Fatalf("change: %v", err)
	}
	if n.Status != model.StatusApproved || n.Limit["cores"] != 5 || n.Pending != nil {
		t.Fatalf("want approved with 5 cores and nothing pending, got %q %v %+v", n.Status, n.Limit, n.Pending)
	}
	if got := f.stored(t); got.Status != model.StatusApproved || got.Limit["cores"] != 5 {
		t.Fatalf("stored: %q %v", got.Status, got.Limit)
	}
	h := n.History
	asked, approved := h[len(h)-2], h[len(h)-1]
	if asked.Event != "change_requested" || asked.LimitTo == nil || (*asked.LimitTo)["cores"] != 5 {
		t.Fatalf("the change should be recorded with what it asked for, got %+v", asked)
	}
	if approved.Event != "approved" || approved.Actor != "lect@x" || approved.Reason == nil || !strings.Contains(*approved.Reason, "verwaltet") {
		t.Fatalf("the approval should be the caller's, got %+v", approved)
	}
}

// Managing the project's own budget is managing the chain above the project: ApproveNode lets them decide, so their change applies at once too.
func TestChangeByManager_OwnBudgetAppliesAtOnce(t *testing.T) {
	f := newManagerChangeFixture(t)
	n, err := f.svc.RequestChange(f.project.ID, tree.ChangeNodeRequest{Limit: quota(cores(6))}, model.UIActor("ta@x"), taTokens)
	if err != nil || n.Status != model.StatusApproved || n.Limit["cores"] != 6 {
		t.Fatalf("want approved with 6 cores, got %v %q %v", err, n.Status, n.Limit)
	}
}

// What an approval would refuse is refused with the same message, and nothing is stored: no request is filed in its place.
func TestChangeByManager_ShortIsRefused(t *testing.T) {
	f := newManagerChangeFixture(t)
	before := f.stored(t)
	_, err := f.svc.RequestChange(f.project.ID, tree.ChangeNodeRequest{Limit: quota(cores(7))}, model.UIActor("lect@x"), lectTokens)
	if err == nil || !strings.Contains(err.Error(), "Kurs") || !strings.Contains(err.Error(), "reicht nicht aus") {
		t.Fatalf("want the course named as short, got %v", err)
	}
	after := f.stored(t)
	if after.Status != model.StatusApproved || after.Limit["cores"] != 2 || after.Pending != nil || len(after.History) != len(before.History) {
		t.Fatalf("nothing should be stored, got %q %v %+v, %d history entries (was %d)", after.Status, after.Limit, after.Pending, len(after.History), len(before.History))
	}
}

// The owner, who manages nothing above, still asks: the change waits for a manager.
func TestChangeByManager_OwnerStillRequests(t *testing.T) {
	f := newManagerChangeFixture(t)
	n, err := f.svc.RequestChange(f.project.ID, tree.ChangeNodeRequest{Limit: quota(cores(4))}, model.UIActor("stud@x"), studTokens)
	if err != nil || n.Status != model.StatusChangePending || n.Pending == nil || n.Limit["cores"] != 2 {
		t.Fatalf("want a pending change, got %v %q %v %+v", err, n.Status, n.Limit, n.Pending)
	}
}

// A manager's change replaces a proposal still waiting on the project, as any new proposal does, and the project is active again.
func TestChangeByManager_ReplacesPendingProposal(t *testing.T) {
	f := newManagerChangeFixture(t)
	if _, err := f.svc.RequestChange(f.project.ID, tree.ChangeNodeRequest{Limit: quota(cores(5))}, model.UIActor("stud@x"), studTokens); err != nil {
		t.Fatalf("owner's request: %v", err)
	}
	n, err := f.svc.RequestChange(f.project.ID, tree.ChangeNodeRequest{Limit: quota(cores(4))}, model.UIActor("lect@x"), lectTokens)
	if err != nil || n.Status != model.StatusApproved || n.Limit["cores"] != 4 || n.Pending != nil {
		t.Fatalf("want approved with 4 cores and the proposal gone, got %v %q %v %+v", err, n.Status, n.Limit, n.Pending)
	}
}

// A later end is a decision of the managers when no policy covers extensions; theirs applies at once.
func TestChangeByManager_ExtensionAppliesAtOnce(t *testing.T) {
	f := newManagerChangeFixture(t)
	end := "2099-12-31"
	n, err := f.svc.RequestChange(f.project.ID, tree.ChangeNodeRequest{TerminationDate: &end}, model.UIActor("ta@x"), taTokens)
	if err != nil || n.Status != model.StatusApproved || n.TerminationDate == nil || *n.TerminationDate != end {
		t.Fatalf("want the new end applied, got %v %q %v", err, n.Status, n.TerminationDate)
	}
	if owner, err := f.svc.RequestChange(f.project.ID, tree.ChangeNodeRequest{TerminationDate: new("2100-01-31")}, model.UIActor("stud@x"), studTokens); err != nil || owner.Status != model.StatusChangePending {
		t.Fatalf("the owner's extension should wait, got %v %q", err, owner.Status)
	}
}

// Root admins manage the top of every chain.
func TestChangeByManager_RootAdmin(t *testing.T) {
	f := newManagerChangeFixture(t)
	n, err := f.svc.RequestChange(f.project.ID, tree.ChangeNodeRequest{Limit: quota(cores(6))}, model.UIActor("root@x"), rootTokens)
	if err != nil || n.Status != model.StatusApproved || n.Limit["cores"] != 6 {
		t.Fatalf("want approved with 6 cores, got %v %q %v", err, n.Status, n.Limit)
	}
}

// A request not decided yet is amended, not approved: approving it stays a decision of its own.
func TestChangeByManager_PendingRequestIsAmended(t *testing.T) {
	f := newManagerChangeFixture(t)
	p, err := f.svc.CreateNode(tree.CreateNodeRequest{
		ParentID: f.course.ID, Kind: model.KindProject, Name: "vm2", Reason: "vm", Limit: cores(1),
	}, model.UIActor("stud@x"), "stud@x", studTokens)
	if err != nil {
		t.Fatalf("request project: %v", err)
	}
	n, err := f.svc.RequestChange(p.ID, tree.ChangeNodeRequest{Limit: quota(cores(2))}, model.UIActor("lect@x"), lectTokens)
	if err != nil || n.Status != model.StatusPending || n.Limit["cores"] != 2 {
		t.Fatalf("want the request amended and still pending, got %v %q %v", err, n.Status, n.Limit)
	}
}

// Auto-approve is unchanged: an owner's growth the policy covers is the policy's approval, not a person's.
func TestChangeByManager_AutoApproveUnchanged(t *testing.T) {
	f := newChangeFixture(t, &model.AutoApprove{}, nil)
	p := f.approved(t, 2, nil)
	n := f.change(t, p.ID, tree.ChangeNodeRequest{Limit: quota(cores(4))})
	last := n.History[len(n.History)-1]
	if n.Status != model.StatusApproved || last.Actor != "system:auto-approval" {
		t.Fatalf("want an auto-approval, got %q by %q", n.Status, last.Actor)
	}
}
