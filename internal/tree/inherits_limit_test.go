package tree_test

import (
	"errors"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/tree"
)

// newGroup creates a budget for structure only under parent.
func (f changeFixture) newGroup(t *testing.T, parent, name string) tree.Node {
	t.Helper()
	n, err := f.svc.CreateNode(tree.CreateNodeRequest{
		ParentID: parent, Kind: tree.KindBudget, Name: name, Reason: "t",
		Limit: common.ProjectQuota{}, AdminScope: common.TokenList{"group:root"},
		EligibleRequesters: common.TokenList{"user:stud@x"}, InheritsLimit: true,
	}, rootActor(), "root@x", rootTokens)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return n
}

func (f changeFixture) get(t *testing.T, id string) tree.Node {
	t.Helper()
	n, err := f.svc.GetNode(id, rootTokens)
	if err != nil || n == nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return *n
}

// A budget that inherits stores no limit; it reads with the one that applies,
// through every level, and follows when that one changes.
func TestInheritsLimit_FollowsTheParent(t *testing.T) {
	f := newChangeFixture(t, nil, nil)
	campus := f.newGroup(t, f.budget.ID, "Campus")
	faculty := f.newGroup(t, campus.ID, "Faculty")
	if len(campus.Limit) != 0 || !campus.InheritsLimit {
		t.Fatalf("stored with a limit of its own %v, inherits %v", campus.Limit, campus.InheritsLimit)
	}
	if got := f.get(t, faculty.ID).Limit["cores"]; got != 10 {
		t.Fatalf("read with %d cores, want the pool's 10", got)
	}
	if _, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{Limit: ptrQuota(cores(20))}, rootActor(), rootTokens); err != nil {
		t.Fatalf("raise pool: %v", err)
	}
	for _, id := range []string{campus.ID, faculty.ID} {
		if got := f.get(t, id).Limit["cores"]; got != 20 {
			t.Errorf("%s has %d cores, want the pool's 20", id, got)
		}
	}
}

// What is used below a budget that inherits counts against the budget above,
// as for any sub-budget — siblings share one limit.
func TestInheritsLimit_SharesTheParentsCapacity(t *testing.T) {
	f := newChangeFixture(t, nil, nil)
	group := f.newGroup(t, f.budget.ID, "Group")
	if _, err := f.svc.CreateNode(tree.CreateNodeRequest{
		ParentID: group.ID, Kind: tree.KindProject, Name: "a", Reason: "t", Limit: cores(8),
	}, rootActor(), "root@x", rootTokens); err != nil {
		t.Fatalf("project below the group: %v", err)
	}
	if _, err := f.svc.CreateNode(tree.CreateNodeRequest{
		ParentID: f.budget.ID, Kind: tree.KindProject, Name: "b", Reason: "t", Limit: cores(3),
	}, rootActor(), "root@x", rootTokens); err == nil {
		t.Fatal("8 + 3 cores fit into a pool of 10")
	}
}

// Passing on the whole limit is a manager's decision.
func TestInheritsLimit_ManagersOnly(t *testing.T) {
	f := newChangeFixture(t, nil, nil)
	_, err := f.svc.CreateNode(tree.CreateNodeRequest{
		ParentID: f.budget.ID, Kind: tree.KindBudget, Name: "g", Reason: "t",
		Limit: common.ProjectQuota{}, AdminScope: common.TokenList{"user:stud@x"}, InheritsLimit: true,
	}, tree.UIActor("stud@x"), "stud@x", studTokens)
	if !errors.Is(err, common.ErrForbidden) {
		t.Fatalf("a requester created an inheriting budget: %v", err)
	}
	if _, err := f.svc.CreateNode(tree.CreateNodeRequest{
		ParentID: f.budget.ID, Kind: tree.KindProject, Name: "p", Reason: "t", Limit: cores(1), InheritsLimit: true,
	}, rootActor(), "root@x", rootTokens); err == nil {
		t.Fatal("a project inherits its limit")
	}
}

// An inherited limit is not set by hand; switched off, the budget keeps it
// and it can be changed again; switched on, it is the parent's once more.
func TestInheritsLimit_SwitchOffAndOn(t *testing.T) {
	f := newChangeFixture(t, nil, nil)
	group := f.newGroup(t, f.budget.ID, "Group")
	if _, err := f.svc.UpdateNode(group.ID, tree.UpdateNodeRequest{Limit: ptrQuota(cores(4))}, rootActor(), rootTokens); err == nil {
		t.Fatal("an inherited limit was set by hand")
	}
	off := false
	n, err := f.svc.UpdateNode(group.ID, tree.UpdateNodeRequest{InheritsLimit: &off}, rootActor(), rootTokens)
	if err != nil || n.InheritsLimit || n.Limit["cores"] != 10 {
		t.Fatalf("switch off: %v, inherits %v, limit %v", err, n.InheritsLimit, n.Limit)
	}
	if n, err = f.svc.UpdateNode(group.ID, tree.UpdateNodeRequest{Limit: ptrQuota(cores(4))}, rootActor(), rootTokens); err != nil || n.Limit["cores"] != 4 {
		t.Fatalf("own limit: %v, %v", err, n.Limit)
	}
	on := true
	if _, err = f.svc.UpdateNode(group.ID, tree.UpdateNodeRequest{InheritsLimit: &on}, rootActor(), rootTokens); err != nil {
		t.Fatalf("switch on: %v", err)
	}
	if got := f.get(t, group.ID).Limit["cores"]; got != 10 {
		t.Fatalf("switched on, it reads with %d cores, want 10", got)
	}
	if _, err := f.svc.RequestChange(group.ID, tree.ChangeNodeRequest{Limit: ptrQuota(cores(4))}, rootActor(), rootTokens); err == nil {
		t.Fatal("a change of an inherited limit was requested")
	}
}

// Moved, a budget that inherits is bound by the new parent's limit — as long
// as what is used below it fits there.
func TestInheritsLimit_Move(t *testing.T) {
	f := newChangeFixture(t, nil, nil)
	group := f.newGroup(t, f.budget.ID, "Group")
	small, err := f.svc.CreateNode(tree.CreateNodeRequest{
		ParentID: tree.RootNodeID, Kind: tree.KindBudget, Name: "Small", Reason: "t",
		Limit: cores(4), AdminScope: common.TokenList{"group:root"},
	}, rootActor(), "root@x", rootTokens)
	if err != nil {
		t.Fatalf("create small: %v", err)
	}
	if _, err := f.svc.ReparentNode(group.ID, tree.ReparentNodeRequest{NewParentID: small.ID}, rootActor(), rootTokens); err != nil {
		t.Fatalf("move: %v", err)
	}
	if got := f.get(t, group.ID).Limit["cores"]; got != 4 {
		t.Fatalf("moved, it reads with %d cores, want the new parent's 4", got)
	}
	if _, err := f.svc.CreateNode(tree.CreateNodeRequest{
		ParentID: group.ID, Kind: tree.KindProject, Name: "a", Reason: "t", Limit: cores(3),
	}, rootActor(), "root@x", rootTokens); err != nil {
		t.Fatalf("project: %v", err)
	}
	other, err := f.svc.CreateNode(tree.CreateNodeRequest{
		ParentID: tree.RootNodeID, Kind: tree.KindBudget, Name: "Tiny", Reason: "t",
		Limit: cores(2), AdminScope: common.TokenList{"group:root"},
	}, rootActor(), "root@x", rootTokens)
	if err != nil {
		t.Fatalf("create tiny: %v", err)
	}
	if _, err := f.svc.ReparentNode(group.ID, tree.ReparentNodeRequest{NewParentID: other.ID}, rootActor(), rootTokens); err == nil {
		t.Fatal("moved 3 cores of use into a budget of 2")
	}
}

// Below a budget that inherits, a sub-budget and a project are measured
// against the limit that applies there, not against an empty one.
func TestInheritsLimit_ChecksUseTheLimitAbove(t *testing.T) {
	f := newChangeFixture(t, nil, nil)
	group := f.newGroup(t, f.budget.ID, "Group")
	if _, err := f.svc.CreateNode(tree.CreateNodeRequest{
		ParentID: group.ID, Kind: tree.KindBudget, Name: "Course", Reason: "t",
		Limit: cores(10), AdminScope: common.TokenList{"group:root"},
	}, rootActor(), "root@x", rootTokens); err != nil {
		t.Fatalf("a sub-budget as large as the pool: %v", err)
	}
	if _, err := f.svc.CreateNode(tree.CreateNodeRequest{
		ParentID: group.ID, Kind: tree.KindBudget, Name: "Too big", Reason: "t",
		Limit: cores(11), AdminScope: common.TokenList{"group:root"},
	}, rootActor(), "root@x", rootTokens); err == nil {
		t.Fatal("a sub-budget larger than the pool above the group")
	}
	if _, err := f.svc.CreateNode(tree.CreateNodeRequest{
		ParentID: group.ID, Kind: tree.KindProject, Name: "p", Reason: "t", Limit: cores(2),
	}, rootActor(), "root@x", rootTokens); err != nil {
		t.Fatalf("a project directly below the group: %v", err)
	}
}
