package tree_test

import (
	"context"
	"errors"
	"github.com/pfisterer/cloud-self-service-golib/day"
	"strings"
	"testing"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/tree"
)

func days(n int) *int { return &n }

// termNow is the clock of the term tests: half past midnight in Berlin on 11 November 2026, still 10 November in UTC, so every test here also checks that a term counts from the day in Berlin.
var termNow = time.Date(2026, 11, 10, 23, 30, 0, 0, time.UTC)

// fixClock sets the service's clock to termNow.
func fixClock(svc *tree.Service) { svc.SetClock(func() time.Time { return termNow }) }

// inDays is the day n days after termNow, counted in Berlin like every end date.
func inDays(n int) string {
	return day.Of(termNow).AddDays(n).String()
}

// newTermFixture is newChangeFixture whose budget lets projects run at most
// term days at a time.
func newTermFixture(t *testing.T, policy *model.AutoApprove, budgetEnd *string, term int) changeFixture {
	t.Helper()
	f := newChangeFixture(t, policy, budgetEnd)
	fixClock(f.svc)
	budget, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{MaxProjectTermDays: &term}, rootActor(), rootTokens)
	if err != nil {
		t.Fatalf("set max term: %v", err)
	}
	f.budget = budget
	return f
}

// A project asked for without an end gets the longest the term allows; one
// asked for past it is refused, and so it is for a manager.
func TestMaxTerm_CreateDefaultsAndRefuses(t *testing.T) {
	f := newTermFixture(t, &model.AutoApprove{}, nil, 180)

	if n := f.request(t, 1, nil); endOf(n) != inDays(180) {
		t.Fatalf("a project without an end should run the full term, got %s", endOf(n))
	}
	if n := f.request(t, 1, ptr(inDays(30))); endOf(n) != inDays(30) {
		t.Fatalf("a shorter end should be kept, got %s", endOf(n))
	}
	_, err := f.svc.CreateNode(tree.CreateNodeRequest{
		ParentID: f.budget.ID, Kind: model.KindProject, Name: "vm", Reason: "vm",
		Limit: cores(1), TerminationDate: ptr(inDays(181)),
	}, model.UIActor("stud@x"), "stud@x", studTokens)
	if err == nil || !strings.Contains(err.Error(), "180 days") {
		t.Fatalf("an end past the term should be refused naming it, got %v", err)
	}
	if _, err := f.svc.CreateNode(tree.CreateNodeRequest{
		ParentID: f.budget.ID, Kind: model.KindProject, Name: "vm", Reason: "vm",
		Limit: cores(1), TerminationDate: ptr(inDays(181)),
	}, rootActor(), "root@x", rootTokens); err == nil {
		t.Fatal("the term binds managers too")
	}
}

// The budget's end still wins where it comes first.
func TestMaxTerm_BudgetEndComesFirst(t *testing.T) {
	end := inDays(10)
	f := newTermFixture(t, &model.AutoApprove{}, &end, 180)
	if n := f.request(t, 1, nil); endOf(n) != end {
		t.Fatalf("a project should end with a budget ending before the term, got %s", endOf(n))
	}
}

// Without an end above and without a term, a project may run open-ended.
func TestMaxTerm_OpenEndedWithoutLimits(t *testing.T) {
	f := newChangeFixture(t, &model.AutoApprove{}, nil)
	fixClock(f.svc)
	if n := f.request(t, 1, nil); n.TerminationDate != nil {
		t.Fatalf("nothing bounds this project, so it should have no end, got %s", endOf(n))
	}
}

// A sub-budget inherits the term, may tighten it and may not loosen or drop it.
func TestMaxTerm_SubBudgets(t *testing.T) {
	f := newTermFixture(t, nil, nil, 180)

	sub := mustCreate(t, f.svc, tree.CreateNodeRequest{ParentID: f.budget.ID, Kind: model.KindBudget, Limit: cores(5)})
	if sub.MaxProjectTermDays == nil || *sub.MaxProjectTermDays != 180 {
		t.Fatalf("a sub-budget should inherit the term, got %v", sub.MaxProjectTermDays)
	}
	if _, err := f.svc.CreateNode(tree.CreateNodeRequest{
		ParentID: f.budget.ID, Kind: model.KindBudget, Name: "n", Reason: "t", Limit: cores(1),
		AdminScope: common.TokenList{"group:root"}, MaxProjectTermDays: days(365),
	}, rootActor(), "root@x", rootTokens); err == nil {
		t.Error("a sub-budget allowing a longer term should be refused")
	}
	if _, err := f.svc.UpdateNode(sub.ID, tree.UpdateNodeRequest{MaxProjectTermDays: days(365)}, rootActor(), rootTokens); err == nil {
		t.Error("loosening a sub-budget's term should be refused")
	}
	if _, err := f.svc.UpdateNode(sub.ID, tree.UpdateNodeRequest{ClearMaxProjectTermDays: true}, rootActor(), rootTokens); err == nil {
		t.Error("dropping a sub-budget's term under a budget with one should be refused")
	}
	if _, err := f.svc.UpdateNode(sub.ID, tree.UpdateNodeRequest{MaxProjectTermDays: days(0)}, rootActor(), rootTokens); err == nil {
		t.Error("a term of zero days should be refused")
	}
	tight := mustCreate(t, f.svc, tree.CreateNodeRequest{ParentID: f.budget.ID, Kind: model.KindBudget, Limit: cores(1), MaxProjectTermDays: days(30)})
	if p := mustCreate(t, f.svc, tree.CreateNodeRequest{ParentID: tight.ID, Kind: model.KindProject, Limit: cores(1)}); endOf(p) != inDays(30) {
		t.Errorf("a project should obey the tighter term, got %s", endOf(p))
	}
}

// Setting or lowering the term shortens what runs longer below — projects,
// waiting proposals and the terms of sub-budgets — and leaves the rest alone.
func TestMaxTerm_LoweringCascades(t *testing.T) {
	f := newChangeFixture(t, nil, nil)
	fixClock(f.svc)
	sub := mustCreate(t, f.svc, tree.CreateNodeRequest{ParentID: f.budget.ID, Kind: model.KindBudget, Limit: cores(5)})
	deep := mustCreate(t, f.svc, tree.CreateNodeRequest{ParentID: sub.ID, Kind: model.KindProject, Limit: cores(1)})
	open := f.approved(t, 1, nil)
	short := f.approved(t, 1, ptr(inDays(20)))
	proposing := f.change(t, short.ID, tree.ChangeNodeRequest{TerminationDate: ptr(inDays(400))})
	if proposing.Status != model.StatusChangePending {
		t.Fatalf("setup: extension without a policy should wait, got %q", proposing.Status)
	}

	if _, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{MaxProjectTermDays: days(90)}, rootActor(), rootTokens); err != nil {
		t.Fatalf("set term: %v", err)
	}

	if n := mustGet(t, f.svc, sub.ID); n.MaxProjectTermDays == nil || *n.MaxProjectTermDays != 90 {
		t.Errorf("the sub-budget should take the term, got %v", n.MaxProjectTermDays)
	}
	for _, id := range []string{deep.ID, open.ID} {
		n := mustGet(t, f.svc, id)
		if endOf(n) != inDays(90) {
			t.Errorf("%s should be shortened to the term, got %s", n.Name, endOf(n))
		}
		if last := n.History[len(n.History)-1]; last.Event != "end_shortened" || last.Reason == nil || !strings.Contains(*last.Reason, "90 days") {
			t.Errorf("%s should say in its history why it ends earlier, got %+v", n.Name, last)
		}
	}
	n := mustGet(t, f.svc, short.ID)
	if endOf(n) != inDays(20) || n.Pending == nil || *n.Pending.TerminationDate != inDays(90) {
		t.Errorf("a shorter project keeps its date, its waiting extension is cut to the term; got %s / %+v", endOf(n), n.Pending)
	}

	// Raising it again changes nothing below.
	if _, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{MaxProjectTermDays: days(365)}, rootActor(), rootTokens); err != nil {
		t.Fatalf("raise term: %v", err)
	}
	if n := mustGet(t, f.svc, open.ID); endOf(n) != inDays(90) {
		t.Errorf("raising the term should not extend projects, got %s", endOf(n))
	}
}

// An extension counts from today: within the term it follows the budget's
// policy, past it it is refused whoever asks.
func TestMaxTerm_Extensions(t *testing.T) {
	f := newTermFixture(t, &model.AutoApprove{}, nil, 180)
	p := f.approved(t, 1, ptr(inDays(30)))

	if n := f.change(t, p.ID, tree.ChangeNodeRequest{TerminationDate: ptr(inDays(180))}); n.Status != model.StatusApproved || endOf(n) != inDays(180) {
		t.Fatalf("an extension within the term should apply at once under auto-approve, got %q / %s", n.Status, endOf(n))
	}
	if _, err := f.svc.RequestChange(p.ID, tree.ChangeNodeRequest{TerminationDate: ptr(inDays(181))}, model.UIActor("stud@x"), studTokens); err == nil {
		t.Fatal("an extension past the term should be refused")
	}
	if _, err := f.svc.RequestChange(p.ID, tree.ChangeNodeRequest{TerminationDate: ptr(inDays(181))}, rootActor(), rootTokens); err == nil {
		t.Fatal("an extension past the term should be refused for a manager too")
	}
}

// A budget may leave extensions to its managers while it still grants new
// projects automatically — even with the hard limit on, an extension then
// waits instead of being refused.
func TestMaxTerm_ExtensionsWaitWhenNotAutoApproved(t *testing.T) {
	f := newHardLimitFixture(t, &model.AutoApprove{})
	fixClock(f.svc)
	no := false
	budget, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{AutoApproveExtensions: &no}, rootActor(), rootTokens)
	if err != nil {
		t.Fatalf("switch off auto-approved extensions: %v", err)
	}
	f.budget = budget

	p := f.approved(t, 1, ptr(inDays(30)))
	if p.Status != model.StatusApproved {
		t.Fatalf("a new project should still be granted, got %q", p.Status)
	}
	n := f.change(t, p.ID, tree.ChangeNodeRequest{TerminationDate: ptr(inDays(60))})
	if n.Status != model.StatusChangePending {
		t.Fatalf("an extension should wait for a manager, got %q", n.Status)
	}
	if n := f.change(t, p.ID, tree.ChangeNodeRequest{TerminationDate: ptr(inDays(10))}); n.Status != model.StatusApproved {
		t.Fatalf("an earlier end should still apply at once, got %q", n.Status)
	}
	if _, err := f.svc.RequestChange(p.ID, tree.ChangeNodeRequest{Limit: quota(cores(11))}, model.UIActor("stud@x"), studTokens); !errors.Is(err, common.ErrForbidden) {
		t.Fatalf("growth past the pool should still be refused, got %v", err)
	}
}

// Approval grants an end no later than the term allows on the day it is
// approved, as it does for a budget's end.
func TestMaxTerm_ApprovalCaps(t *testing.T) {
	f := newChangeFixture(t, nil, nil)
	fixClock(f.svc)
	p := f.request(t, 1, ptr(inDays(400)))
	if _, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{MaxProjectTermDays: days(60)}, rootActor(), rootTokens); err != nil {
		t.Fatalf("set term: %v", err)
	}
	// Lowering the term shortened the request, which is a new revision of it.
	shortened := f.get(t, p.ID)
	n, err := f.svc.ApproveNode(p.ID, tree.ApproveNodeRequest{ExpectedRevision: new(shortened.CurrentRevision())}, rootActor(), rootTokens)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if endOf(n) != inDays(60) {
		t.Fatalf("approval should grant at most the term, got %s", endOf(n))
	}
}

// Moving a project or a budget under a budget with a term applies it.
func TestMaxTerm_Reparent(t *testing.T) {
	svc, _ := newSvc(t, common.TokenList{"group:root"})
	fixClock(svc)
	from := mustCreate(t, svc, tree.CreateNodeRequest{ParentID: model.RootNodeID, Kind: model.KindBudget, Limit: cores(10)})
	to := mustCreate(t, svc, tree.CreateNodeRequest{ParentID: model.RootNodeID, Kind: model.KindBudget, Limit: cores(10), MaxProjectTermDays: days(30)})
	sub := mustCreate(t, svc, tree.CreateNodeRequest{ParentID: from.ID, Kind: model.KindBudget, Limit: cores(5)})
	deep := mustCreate(t, svc, tree.CreateNodeRequest{ParentID: sub.ID, Kind: model.KindProject, Limit: cores(1)})
	leaf := mustCreate(t, svc, tree.CreateNodeRequest{ParentID: from.ID, Kind: model.KindProject, Limit: cores(1)})

	if _, err := svc.ReparentNode(sub.ID, tree.ReparentNodeRequest{NewParentID: to.ID}, rootActor(), rootTokens); err != nil {
		t.Fatalf("reparent budget: %v", err)
	}
	if _, err := svc.ReparentNode(leaf.ID, tree.ReparentNodeRequest{NewParentID: to.ID}, rootActor(), rootTokens); err != nil {
		t.Fatalf("reparent project: %v", err)
	}
	if n := mustGet(t, svc, sub.ID); n.MaxProjectTermDays == nil || *n.MaxProjectTermDays != 30 {
		t.Errorf("the moved budget should take the term, got %v", n.MaxProjectTermDays)
	}
	for _, id := range []string{deep.ID, leaf.ID} {
		if n := mustGet(t, svc, id); endOf(n) != inDays(30) {
			t.Errorf("%s should be shortened to the term, got %s", n.Name, endOf(n))
		}
	}
}

// A term counts from the day in Berlin: at half past midnight there, while UTC still has the day before, a term of one day ends on the day after the Berlin one.
func TestMaxTerm_CountsFromTheDayInBerlin(t *testing.T) {
	f := newTermFixture(t, &model.AutoApprove{}, nil, 1)
	if n := f.request(t, 1, nil); endOf(n) != "2026-11-12" {
		t.Fatalf("a one-day term at 00:30 in Berlin on 11 November should end on 12 November, got %s", endOf(n))
	}
	f.svc.SetClock(func() time.Time { return termNow.Add(-time.Hour) }) // 23:30 in Berlin on 10 November
	if n := f.request(t, 1, nil); endOf(n) != "2026-11-11" {
		t.Fatalf("a one-day term at 23:30 in Berlin on 10 November should end on 11 November, got %s", endOf(n))
	}
}

// Promoting an import under a budget with a term gives it the longest end the term allows and refuses a later one.
func TestMaxTerm_Promote(t *testing.T) {
	svc, store := newSvc(t, common.TokenList{"group:root"})
	fixClock(svc)
	to := mustCreate(t, svc, tree.CreateNodeRequest{ParentID: model.RootNodeID, Kind: model.KindBudget, Limit: cores(10), MaxProjectTermDays: days(30)})
	importNode := func(id string) {
		t.Helper()
		if err := store.UpsertNode(context.Background(), model.Node{ID: id, Kind: model.KindProject, ParentID: new(model.UnassignedNodeID), Status: model.StatusImported, Name: id, Limit: cores(1)}); err != nil {
			t.Fatalf("import: %v", err)
		}
	}
	importNode("p_late")
	if _, err := svc.PromoteNode("p_late", tree.PromoteNodeRequest{NewParentID: to.ID, Owner: "stud@x", Reason: "t", TerminationDate: ptr(inDays(31))}, rootActor(), rootTokens); err == nil || !strings.Contains(err.Error(), "30 days") {
		t.Fatalf("an end past the term should be refused naming it, got %v", err)
	}
	importNode("p_open")
	n, err := svc.PromoteNode("p_open", tree.PromoteNodeRequest{NewParentID: to.ID, Owner: "stud@x", Reason: "t"}, rootActor(), rootTokens)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if endOf(n) != inDays(30) {
		t.Fatalf("a promoted project without an end should run the full term, got %s", endOf(n))
	}
}
