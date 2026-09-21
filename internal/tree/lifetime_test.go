package tree_test

import (
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/tree"
)

var rootTokens = common.TokenList{"user:root@x", "group:root"}

func rootActor() tree.Actor { return tree.UIActor("root@x") }

func mustCreate(t *testing.T, svc *tree.Service, req tree.CreateNodeRequest) tree.Node {
	t.Helper()
	if req.Kind == tree.KindBudget && req.AdminScope == nil {
		req.AdminScope = common.TokenList{"group:root"}
	}
	req.Reason = "t"
	if req.Name == "" {
		req.Name = "n"
	}
	n, err := svc.CreateNode(req, rootActor(), "root@x", rootTokens)
	if err != nil {
		t.Fatalf("create %s: %v", req.Kind, err)
	}
	return n
}

func mustGet(t *testing.T, svc *tree.Service, id string) tree.Node {
	t.Helper()
	n, err := svc.GetNode(id, rootTokens)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return *n
}

func endOf(n tree.Node) string {
	if n.TerminationDate == nil {
		return "<none>"
	}
	return *n.TerminationDate
}

const (
	dec2026 = "2026-12-31T00:00:00Z"
	mar2027 = "2027-03-31T00:00:00Z"
	sep2027 = "2027-09-30T00:00:00Z"
)

func ptr(s string) *string { return &s }

// Under a budget that ends, a node asked for without an end gets the budget's,
// and one asked for past it is refused — for projects and sub-budgets alike,
// and however far up the end is set.
func TestLifetime_CreateInheritsAndRefuses(t *testing.T) {
	f := newChangeFixture(t, &tree.AutoApprove{}, ptr(sep2027))

	if n := f.request(t, 1, nil); endOf(n) != sep2027 {
		t.Fatalf("a project without an end should end with its budget, got %s", endOf(n))
	}
	if _, err := f.svc.CreateNode(tree.CreateNodeRequest{
		ParentID: f.budget.ID, Kind: tree.KindProject, Name: "vm", Reason: "vm",
		Limit: cores(1), TerminationDate: ptr("2030-01-01T00:00:00Z"),
	}, tree.UIActor("stud@x"), "stud@x", studTokens); err == nil {
		t.Fatal("a project outliving its budget should be refused")
	}

	sub := mustCreate(t, f.svc, tree.CreateNodeRequest{ParentID: f.budget.ID, Kind: tree.KindBudget, Limit: cores(2)})
	if endOf(sub) != sep2027 {
		t.Fatalf("a sub-budget without an end should end with its budget, got %s", endOf(sub))
	}
	// The grandparent's end binds too.
	if n := mustCreate(t, f.svc, tree.CreateNodeRequest{ParentID: sub.ID, Kind: tree.KindProject, Limit: cores(1)}); endOf(n) != sep2027 {
		t.Fatalf("a project two levels down should end with the budget above, got %s", endOf(n))
	}
}

// Shortening a budget takes everything below it along: sub-budgets, their
// projects, requests still waiting and proposals still waiting. Nodes ending
// earlier already keep their date.
func TestLifetime_ShorteningCascades(t *testing.T) {
	f := newChangeFixture(t, nil, ptr(sep2027))
	sub := mustCreate(t, f.svc, tree.CreateNodeRequest{ParentID: f.budget.ID, Kind: tree.KindBudget, Limit: cores(5)})
	deep := mustCreate(t, f.svc, tree.CreateNodeRequest{ParentID: sub.ID, Kind: tree.KindProject, Limit: cores(1)})
	early := f.approved(t, 1, ptr(dec2026))
	waiting := f.request(t, 1, ptr(sep2027))
	proposing := f.approved(t, 1, ptr(dec2026))
	proposing = f.change(t, proposing.ID, tree.ChangeNodeRequest{TerminationDate: ptr(sep2027)})
	if proposing.Status != tree.StatusChangePending {
		t.Fatalf("setup: extension without a policy should wait, got %q", proposing.Status)
	}

	if _, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{TerminationDate: ptr(mar2027)}, rootActor(), rootTokens); err != nil {
		t.Fatalf("shorten budget: %v", err)
	}

	for _, id := range []string{sub.ID, deep.ID, waiting.ID} {
		n := mustGet(t, f.svc, id)
		if endOf(n) != mar2027 {
			t.Errorf("%s should end with the budget, got %s", n.Name, endOf(n))
		}
		last := n.History[len(n.History)-1]
		if last.Event != "end_shortened" || last.Reason == nil {
			t.Errorf("%s should say in its history why it ends earlier, got %+v", n.Name, last)
		}
	}
	if n := mustGet(t, f.svc, early.ID); endOf(n) != dec2026 {
		t.Errorf("a project ending earlier should keep its date, got %s", endOf(n))
	}
	n := mustGet(t, f.svc, proposing.ID)
	if endOf(n) != dec2026 || n.Pending == nil || *n.Pending.TerminationDate != mar2027 {
		t.Errorf("a waiting extension should be cut to the budget's end, got %s / %+v", endOf(n), n.Pending)
	}
}

// Extending a budget leaves the dates below it alone.
func TestLifetime_ExtendingLeavesChildren(t *testing.T) {
	f := newChangeFixture(t, nil, ptr(mar2027))
	p := f.approved(t, 1, nil)

	if _, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{TerminationDate: ptr(sep2027)}, rootActor(), rootTokens); err != nil {
		t.Fatalf("extend budget: %v", err)
	}
	if n := mustGet(t, f.svc, p.ID); endOf(n) != mar2027 {
		t.Fatalf("extending the budget should not extend its projects, got %s", endOf(n))
	}
}

// Giving a budget without an end one is a shortening too.
func TestLifetime_SettingAnEndCascades(t *testing.T) {
	f := newChangeFixture(t, nil, nil)
	p := f.approved(t, 1, nil)

	if _, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{TerminationDate: ptr(mar2027)}, rootActor(), rootTokens); err != nil {
		t.Fatalf("set budget end: %v", err)
	}
	if n := mustGet(t, f.svc, p.ID); endOf(n) != mar2027 {
		t.Fatalf("a project without an end should get the budget's new one, got %s", endOf(n))
	}
}

// A sub-budget cannot be edited to outlive the budget above, nor lose its end.
func TestLifetime_EditingABudgetStaysWithin(t *testing.T) {
	f := newChangeFixture(t, nil, ptr(mar2027))
	sub := mustCreate(t, f.svc, tree.CreateNodeRequest{ParentID: f.budget.ID, Kind: tree.KindBudget, Limit: cores(2)})

	if _, err := f.svc.UpdateNode(sub.ID, tree.UpdateNodeRequest{TerminationDate: ptr(sep2027)}, rootActor(), rootTokens); err == nil {
		t.Error("a sub-budget outliving its budget should be refused")
	}
	if _, err := f.svc.UpdateNode(sub.ID, tree.UpdateNodeRequest{ClearTerminationDate: true}, rootActor(), rootTokens); err == nil {
		t.Error("removing a sub-budget's end under a budget that ends should be refused")
	}
}

// Moving a subtree under a budget that ends sooner shortens it on the way.
func TestLifetime_ReparentShortens(t *testing.T) {
	svc, _ := newSvc(t, common.TokenList{"group:root"})
	from := mustCreate(t, svc, tree.CreateNodeRequest{ParentID: tree.RootNodeID, Kind: tree.KindBudget, Limit: cores(10)})
	to := mustCreate(t, svc, tree.CreateNodeRequest{ParentID: tree.RootNodeID, Kind: tree.KindBudget, Limit: cores(10), TerminationDate: ptr(mar2027)})
	sub := mustCreate(t, svc, tree.CreateNodeRequest{ParentID: from.ID, Kind: tree.KindBudget, Limit: cores(5)})
	p := mustCreate(t, svc, tree.CreateNodeRequest{ParentID: sub.ID, Kind: tree.KindProject, Limit: cores(1)})

	if _, err := svc.ReparentNode(sub.ID, tree.ReparentNodeRequest{NewParentID: to.ID}, rootActor(), rootTokens); err != nil {
		t.Fatalf("reparent: %v", err)
	}
	for _, id := range []string{sub.ID, p.ID} {
		if n := mustGet(t, svc, id); endOf(n) != mar2027 {
			t.Errorf("%s should end with its new budget, got %s", n.Name, endOf(n))
		}
	}
}
