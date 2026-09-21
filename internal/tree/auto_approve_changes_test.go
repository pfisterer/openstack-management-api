package tree_test

import (
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/tree"
)

// changeFixture is a budget of 10 cores that "stud@x" may request from, under
// the given auto-approve policy (nil: none), ending on budgetEnd (nil: never).
type changeFixture struct {
	svc    *tree.Service
	budget tree.Node
}

func newChangeFixture(t *testing.T, policy *tree.AutoApprove, budgetEnd *string) changeFixture {
	t.Helper()
	svc, _ := newSvc(t, common.TokenList{"group:root"})
	budget, err := svc.CreateNode(tree.CreateNodeRequest{
		ParentID: tree.RootNodeID, Kind: tree.KindBudget, Name: "Pool", Reason: "t",
		Limit: cores(10), AdminScope: common.TokenList{"group:root"},
		EligibleRequesters: common.TokenList{"user:stud@x"},
		AutoApprove:        policy,
		TerminationDate:    budgetEnd,
	}, tree.UIActor("root@x"), "root@x", common.TokenList{"user:root@x", "group:root"})
	if err != nil {
		t.Fatalf("create budget: %v", err)
	}
	return changeFixture{svc: svc, budget: budget}
}

var studTokens = common.TokenList{"user:stud@x"}

func (f changeFixture) request(t *testing.T, n int, end *string) tree.Node {
	t.Helper()
	node, err := f.svc.CreateNode(tree.CreateNodeRequest{
		ParentID: f.budget.ID, Kind: tree.KindProject, Name: "vm", Reason: "vm",
		Limit: cores(n), TerminationDate: end,
	}, tree.UIActor("stud@x"), "stud@x", studTokens)
	if err != nil {
		t.Fatalf("request project: %v", err)
	}
	return node
}

// approved returns an active project of n cores — auto-approved where the
// policy allows it, approved by the root admin otherwise.
func (f changeFixture) approved(t *testing.T, n int, end *string) tree.Node {
	t.Helper()
	node := f.request(t, n, end)
	if node.Status == tree.StatusApproved {
		return node
	}
	node, err := f.svc.ApproveNode(node.ID, tree.ApproveNodeRequest{}, tree.UIActor("root@x"), common.TokenList{"group:root"})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	return node
}

func (f changeFixture) change(t *testing.T, id string, req tree.ChangeNodeRequest) tree.Node {
	t.Helper()
	node, err := f.svc.RequestChange(id, req, tree.UIActor("stud@x"), studTokens)
	if err != nil {
		t.Fatalf("request change: %v", err)
	}
	return node
}

func quota(q common.ProjectQuota) *common.ProjectQuota { return &q }

// A pool policy — no per-person limit — grants whatever the budget has room
// for, and nothing beyond it.
func TestAutoApprove_PoolGrantsUpToTheBudget(t *testing.T) {
	f := newChangeFixture(t, &tree.AutoApprove{}, nil)

	if n := f.request(t, 8, nil); n.Status != tree.StatusApproved {
		t.Fatalf("8 of 10 cores from a pool should be granted at once, got %q", n.Status)
	}
	if n := f.request(t, 2, nil); n.Status != tree.StatusApproved {
		t.Fatalf("the last 2 cores should be granted at once, got %q", n.Status)
	}
	if n := f.request(t, 1, nil); n.Status != tree.StatusPending {
		t.Fatalf("a request beyond the budget should wait for a manager, got %q", n.Status)
	}
}
