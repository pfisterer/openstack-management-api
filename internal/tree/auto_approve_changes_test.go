package tree_test

import (
	"errors"
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

// Without any policy, giving back, ending sooner and changing members still
// take effect at once; growing and extending wait for a manager.
func TestChange_WithoutPolicy(t *testing.T) {
	end := "2027-06-30T00:00:00Z"
	f := newChangeFixture(t, nil, nil)
	p := f.approved(t, 4, &end)

	if n := f.change(t, p.ID, tree.ChangeNodeRequest{Limit: quota(cores(2))}); n.Status != tree.StatusApproved || n.Limit["cores"] != 2 {
		t.Fatalf("a shrink should apply at once, got %q with %v", n.Status, n.Limit)
	}
	sooner := "2027-03-31T00:00:00Z"
	if n := f.change(t, p.ID, tree.ChangeNodeRequest{TerminationDate: &sooner}); n.Status != tree.StatusApproved || *n.TerminationDate != sooner {
		t.Fatalf("an earlier end should apply at once, got %q", n.Status)
	}
	members := []common.AuthorizedUser{{Token: "user:friend@x", OpenstackRole: "member"}}
	if n := f.change(t, p.ID, tree.ChangeNodeRequest{AuthorizedUsers: &members}); n.Status != tree.StatusApproved || len(n.AuthorizedUsers) != 1 {
		t.Fatalf("a member change should apply at once, got %q with %v", n.Status, n.AuthorizedUsers)
	}

	if n := f.change(t, p.ID, tree.ChangeNodeRequest{Limit: quota(cores(3))}); n.Status != tree.StatusChangePending || n.Limit["cores"] != 2 {
		t.Fatalf("growth without a policy should wait and keep the old limit, got %q with %v", n.Status, n.Limit)
	}
	later := "2027-12-31T00:00:00Z"
	if n := f.change(t, p.ID, tree.ChangeNodeRequest{TerminationDate: &later}); n.Status != tree.StatusChangePending {
		t.Fatalf("an extension without a policy should wait, got %q", n.Status)
	}
}

// One part that needs a decision sends the whole proposal to a manager —
// nothing of it is applied behind their back.
func TestChange_MixedProposalWaitsAsAWhole(t *testing.T) {
	f := newChangeFixture(t, nil, nil)
	p := f.approved(t, 4, nil)

	members := []common.AuthorizedUser{{Token: "user:friend@x", OpenstackRole: "member"}}
	n := f.change(t, p.ID, tree.ChangeNodeRequest{Limit: quota(cores(6)), AuthorizedUsers: &members})
	if n.Status != tree.StatusChangePending || len(n.AuthorizedUsers) != 0 {
		t.Fatalf("growth plus members should wait as one, got %q with members %v", n.Status, n.AuthorizedUsers)
	}
}

// Under an individual limit, growth is granted while the person's total stays
// within it — the project's own current size counts once, not twice.
func TestChange_GrowthWithinIndividualLimit(t *testing.T) {
	f := newChangeFixture(t, &tree.AutoApprove{PerRequesterLimit: cores(4)}, nil)
	p := f.approved(t, 2, nil)

	if n := f.change(t, p.ID, tree.ChangeNodeRequest{Limit: quota(cores(4))}); n.Status != tree.StatusApproved || n.Limit["cores"] != 4 {
		t.Fatalf("growing to the personal limit should apply at once, got %q with %v", n.Status, n.Limit)
	}
	if n := f.change(t, p.ID, tree.ChangeNodeRequest{Limit: quota(cores(5))}); n.Status != tree.StatusChangePending || n.Limit["cores"] != 4 {
		t.Fatalf("growing past the personal limit should wait, got %q with %v", n.Status, n.Limit)
	}
}

// Under a pool, growth is bounded by the budget's free capacity only.
func TestChange_GrowthInPool(t *testing.T) {
	f := newChangeFixture(t, &tree.AutoApprove{}, nil)
	p := f.approved(t, 2, nil)
	f.approved(t, 5, nil)

	if n := f.change(t, p.ID, tree.ChangeNodeRequest{Limit: quota(cores(5))}); n.Status != tree.StatusApproved {
		t.Fatalf("growing into the pool's free room should apply at once, got %q", n.Status)
	}
	if n := f.change(t, p.ID, tree.ChangeNodeRequest{Limit: quota(cores(6))}); n.Status != tree.StatusChangePending {
		t.Fatalf("growing past the pool should wait, got %q", n.Status)
	}
}

// With a policy, a later end is granted up to the budget's own end.
func TestChange_ExtensionWithinBudgetEnd(t *testing.T) {
	budgetEnd := "2027-09-30T00:00:00Z"
	end := "2027-03-31T00:00:00Z"
	f := newChangeFixture(t, &tree.AutoApprove{}, &budgetEnd)
	p := f.approved(t, 1, &end)

	if n := f.change(t, p.ID, tree.ChangeNodeRequest{TerminationDate: &budgetEnd}); n.Status != tree.StatusApproved {
		t.Fatalf("extending to the budget's end should apply at once, got %q", n.Status)
	}
	beyond := "2027-10-31T00:00:00Z"
	if n := f.change(t, p.ID, tree.ChangeNodeRequest{TerminationDate: &beyond}); n.Status != tree.StatusChangePending {
		t.Fatalf("extending past the budget's end should wait, got %q", n.Status)
	}
}

// A direct change replaces a proposal that is still waiting: the node leaves
// change_pending and nothing of the old proposal survives.
func TestChange_DirectChangeReplacesWaitingProposal(t *testing.T) {
	f := newChangeFixture(t, nil, nil)
	p := f.approved(t, 4, nil)

	if n := f.change(t, p.ID, tree.ChangeNodeRequest{Limit: quota(cores(8))}); n.Status != tree.StatusChangePending {
		t.Fatalf("setup: growth should wait, got %q", n.Status)
	}
	n := f.change(t, p.ID, tree.ChangeNodeRequest{Limit: quota(cores(1))})
	if n.Status != tree.StatusApproved || n.Pending != nil || n.Limit["cores"] != 1 {
		t.Fatalf("a shrink should replace the waiting proposal, got %q, pending %v, limit %v", n.Status, n.Pending, n.Limit)
	}
}

// newHardLimitFixture is newChangeFixture with requests beyond the policy
// switched off: the policy is then a hard limit for the requesters.
func newHardLimitFixture(t *testing.T, policy *tree.AutoApprove) changeFixture {
	t.Helper()
	f := newChangeFixture(t, policy, nil)
	no := false
	budget, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{AllowRequestsBeyondAutoApprove: &no},
		tree.UIActor("root@x"), common.TokenList{"user:root@x", "group:root"})
	if err != nil {
		t.Fatalf("switch off requests beyond auto-approve: %v", err)
	}
	f.budget = budget
	return f
}

// With the hard limit, a new project beyond the policy is refused outright
// instead of waiting for a manager.
func TestHardLimit_RefusesNewProjectBeyondPolicy(t *testing.T) {
	f := newHardLimitFixture(t, &tree.AutoApprove{PerRequesterLimit: cores(4)})

	if n := f.request(t, 4, nil); n.Status != tree.StatusApproved {
		t.Fatalf("a request within the share should still be granted, got %q", n.Status)
	}
	_, err := f.svc.CreateNode(tree.CreateNodeRequest{
		ParentID: f.budget.ID, Kind: tree.KindProject, Name: "vm", Reason: "vm", Limit: cores(1),
	}, tree.UIActor("stud@x"), "stud@x", studTokens)
	if err == nil || !errors.Is(err, common.ErrForbidden) {
		t.Fatalf("a request beyond the share should be refused as forbidden, got %v", err)
	}
}

// Growth beyond the policy is refused too; giving back still works, and the
// budget's managers are not bound by the limit.
func TestHardLimit_Changes(t *testing.T) {
	f := newHardLimitFixture(t, &tree.AutoApprove{})
	p := f.approved(t, 8, nil)

	if _, err := f.svc.RequestChange(p.ID, tree.ChangeNodeRequest{Limit: quota(cores(11))}, tree.UIActor("stud@x"), studTokens); !errors.Is(err, common.ErrForbidden) {
		t.Fatalf("growth past the pool should be refused, got %v", err)
	}
	if n := f.change(t, p.ID, tree.ChangeNodeRequest{Limit: quota(cores(6))}); n.Status != tree.StatusApproved {
		t.Fatalf("a shrink should still apply at once, got %q", n.Status)
	}
	n, err := f.svc.RequestChange(p.ID, tree.ChangeNodeRequest{Limit: quota(cores(11))}, tree.UIActor("root@x"), common.TokenList{"user:root@x", "group:root"})
	if err != nil || n.Status != tree.StatusChangePending {
		t.Fatalf("a manager may still propose beyond the policy, got %v / %q", err, n.Status)
	}
}

// Without an auto-approve policy the switch has nothing to limit: requests
// keep waiting for a manager.
func TestHardLimit_NoEffectWithoutPolicy(t *testing.T) {
	f := newHardLimitFixture(t, nil)
	if n := f.request(t, 1, nil); n.Status != tree.StatusPending {
		t.Fatalf("without a policy a request should wait for a manager, got %q", n.Status)
	}
}
