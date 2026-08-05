package tree_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/roleprovider"
	"github.com/pfisterer/openstack-management-api/internal/tree"
	"go.uber.org/zap"
)

var testResources = []string{"cores"}

func newSvc(t *testing.T, rootAdmins common.TokenList) (*tree.Service, tree.Store) {
	t.Helper()
	log := zap.NewNop().Sugar()
	store := tree.NewInMemoryStore(log)
	svc := tree.NewService(store, roleprovider.NewMockRoleProvider(), testResources, rootAdmins, 5*time.Second, log)
	if err := svc.Bootstrap(context.Background(), nil, nil); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return svc, store
}

func cores(n int) common.ProjectQuota { return common.ProjectQuota{"cores": n} }

// TestBootstrap_SyncsRootAdminScope verifies the root node's admin scope follows
// ROOT_ADMIN_TOKENS across restarts (the old model only set it on first creation,
// letting removed admins keep — and new admins never gain — the top-level scope).
func TestBootstrap_SyncsRootAdminScope(t *testing.T) {
	log := zap.NewNop().Sugar()
	store := tree.NewInMemoryStore(log)

	svc1 := tree.NewService(store, roleprovider.NewMockRoleProvider(), testResources, common.TokenList{"group:admins-v1"}, 5*time.Second, log)
	if err := svc1.Bootstrap(context.Background(), nil, nil); err != nil {
		t.Fatalf("bootstrap v1: %v", err)
	}

	root, err := store.GetNode(context.Background(), tree.RootNodeID)
	if err != nil || root == nil {
		t.Fatalf("root node missing after bootstrap: %v", err)
	}
	if len(root.AdminScope) != 1 || root.AdminScope[0] != "group:admins-v1" {
		t.Fatalf("root admin scope = %v, want [group:admins-v1]", root.AdminScope)
	}
	unassigned, err := store.GetNode(context.Background(), tree.UnassignedNodeID)
	if err != nil || unassigned == nil {
		t.Fatalf("unassigned node missing after bootstrap: %v", err)
	}
	if unassigned.Limit["cores"] != 0 {
		t.Errorf("unassigned limit should be 0, got %d", unassigned.Limit["cores"])
	}

	// "Restart" with a changed configuration → the scope is synced, not preserved.
	svc2 := tree.NewService(store, roleprovider.NewMockRoleProvider(), testResources, common.TokenList{"group:admins-v2"}, 5*time.Second, log)
	if err := svc2.Bootstrap(context.Background(), nil, nil); err != nil {
		t.Fatalf("bootstrap v2: %v", err)
	}
	root, _ = store.GetNode(context.Background(), tree.RootNodeID)
	if len(root.AdminScope) != 1 || root.AdminScope[0] != "group:admins-v2" {
		t.Fatalf("root admin scope after restart = %v, want [group:admins-v2]", root.AdminScope)
	}
}

// TestAutoApprove_CountsPerOwnerNotPerGroup is the F-1 regression test: two
// requesters sharing every group token each get their own per-requester budget,
// and one requester's consumption never counts against the other.
func TestAutoApprove_CountsPerOwnerNotPerGroup(t *testing.T) {
	svc, _ := newSvc(t, common.TokenList{"group:root"})
	rootTokens := common.TokenList{"user:root@x", "group:root"}

	// Root creates an auto-approve budget: total 10, 2 per requester.
	budget, err := svc.CreateNode(tree.CreateNodeRequest{
		ParentID:           tree.RootNodeID,
		Kind:               tree.KindBudget,
		Name:               "Selfservice",
		Reason:             "test",
		Limit:              cores(10),
		AdminScope:         common.TokenList{"group:root"},
		EligibleRequesters: common.TokenList{"group:students"},
		AutoApprove:        &tree.AutoApprove{PerRequesterLimit: cores(2)},
	}, "root@x", "root@x", rootTokens)
	if err != nil {
		t.Fatalf("create budget: %v", err)
	}

	// Both students carry the SAME group tokens — only their email differs.
	annaTokens := common.TokenList{"user:anna@x", "group:students"}
	benTokens := common.TokenList{"user:ben@x", "group:students"}

	req := func(email string, tokens common.TokenList, n int) (tree.Node, error) {
		return svc.CreateNode(tree.CreateNodeRequest{
			ParentID: budget.ID,
			Kind:     tree.KindProject,
			Reason:   "vm",
			Limit:    cores(n),
		}, email, email, tokens)
	}

	// Anna exhausts her cap.
	n1, err := req("anna@x", annaTokens, 2)
	if err != nil || n1.Status != tree.StatusApproved {
		t.Fatalf("anna's first request should auto-approve, got %v / %v", n1.Status, err)
	}
	// Anna's next request exceeds HER cumulative cap.
	n2, err := req("anna@x", annaTokens, 1)
	if err != nil || n2.Status != tree.StatusPending {
		t.Fatalf("anna's second request should stay pending, got %v / %v", n2.Status, err)
	}
	// Ben still gets his own full cap — Anna's usage must not count against him.
	n3, err := req("ben@x", benTokens, 2)
	if err != nil || n3.Status != tree.StatusApproved {
		t.Fatalf("ben's request should auto-approve despite anna's usage, got %v / %v", n3.Status, err)
	}

	// And Ben cannot touch Anna's leaf: he is neither owner nor manager.
	if _, err := svc.ReleaseNode(n1.ID, "ben@x", benTokens); err == nil {
		t.Fatalf("ben releasing anna's leaf must fail")
	}
	if _, err := svc.RequestChange(n1.ID, tree.ChangeNodeRequest{Limit: ptrQuota(cores(1))}, "ben@x", benTokens); err == nil {
		t.Fatalf("ben changing anna's leaf must fail")
	}
	// Anna herself can.
	if _, err := svc.ReleaseNode(n1.ID, "anna@x", annaTokens); err != nil {
		t.Fatalf("anna releasing her own leaf: %v", err)
	}
}

// TestAutoApprove_BudgetTotalLimitCaps verifies the auto-approve budget's own
// total limit is enforced even when every requester is within their personal cap
// (the old model had no total for allowances at all).
func TestAutoApprove_BudgetTotalLimitCaps(t *testing.T) {
	svc, _ := newSvc(t, common.TokenList{"group:root"})
	rootTokens := common.TokenList{"user:root@x", "group:root"}

	budget, err := svc.CreateNode(tree.CreateNodeRequest{
		ParentID:           tree.RootNodeID,
		Kind:               tree.KindBudget,
		Name:               "Small selfservice",
		Reason:             "test",
		Limit:              cores(3), // total for everyone
		AdminScope:         common.TokenList{"group:root"},
		EligibleRequesters: common.TokenList{"group:students"},
		AutoApprove:        &tree.AutoApprove{PerRequesterLimit: cores(2)},
	}, "root@x", "root@x", rootTokens)
	if err != nil {
		t.Fatalf("create budget: %v", err)
	}

	req := func(email string, n int) tree.Node {
		t.Helper()
		node, err := svc.CreateNode(tree.CreateNodeRequest{
			ParentID: budget.ID, Kind: tree.KindProject, Reason: "vm", Limit: cores(n),
		}, email, email, common.TokenList{"user:" + email, "group:students"})
		if err != nil {
			t.Fatalf("request by %s: %v", email, err)
		}
		return node
	}

	if n := req("a@x", 2); n.Status != tree.StatusApproved {
		t.Fatalf("first 2 cores should auto-approve, got %q", n.Status)
	}
	// b is within their personal cap (2), but the budget only has 1 core left.
	if n := req("b@x", 2); n.Status != tree.StatusPending {
		t.Fatalf("request beyond the budget total should stay pending, got %q", n.Status)
	}
	// A fitting request still auto-approves.
	if n := req("c@x", 1); n.Status != tree.StatusApproved {
		t.Fatalf("1 core within the remaining total should auto-approve, got %q", n.Status)
	}
}

// TestReparent_CycleGuard ensures a budget cannot be moved into its own subtree.
func TestReparent_CycleGuard(t *testing.T) {
	svc, _ := newSvc(t, common.TokenList{"group:root"})
	rootTokens := common.TokenList{"user:root@x", "group:root"}

	outer, err := svc.CreateNode(tree.CreateNodeRequest{
		ParentID: tree.RootNodeID, Kind: tree.KindBudget, Name: "Outer", Reason: "t",
		Limit: cores(10), AdminScope: common.TokenList{"group:root"},
	}, "root@x", "root@x", rootTokens)
	if err != nil {
		t.Fatalf("create outer: %v", err)
	}
	inner, err := svc.CreateNode(tree.CreateNodeRequest{
		ParentID: outer.ID, Kind: tree.KindBudget, Name: "Inner", Reason: "t",
		Limit: cores(5), AdminScope: common.TokenList{"group:root"},
	}, "root@x", "root@x", rootTokens)
	if err != nil {
		t.Fatalf("create inner: %v", err)
	}

	if _, err := svc.ReparentNode(outer.ID, tree.ReparentNodeRequest{NewParentID: inner.ID}, "root@x", rootTokens); err == nil {
		t.Fatalf("moving a budget into its own subtree must fail")
	}
}

func ptrQuota(q common.ProjectQuota) *common.ProjectQuota { return &q }

// A budget without an admin scope would be invisible: "My Budgets" matches
// AdminScope directly, so nobody could find it, and requests under it would
// surface at the nearest ancestor manager instead.
func TestCreateBudget_RejectsEmptyAdminScope(t *testing.T) {
	svc, _ := newSvc(t, common.TokenList{"group:root"})
	rootTokens := common.TokenList{"user:root@x", "group:root"}

	_, err := svc.CreateNode(tree.CreateNodeRequest{
		ParentID: tree.RootNodeID,
		Kind:     tree.KindBudget,
		Name:     "Orphan",
		Reason:   "test",
		Limit:    cores(10),
	}, "root@x", "root@x", rootTokens)
	if err == nil {
		t.Fatal("creating a budget without admin_scope should fail")
	}
	if !strings.Contains(err.Error(), "admin_scope") {
		t.Errorf("error should name the offending field, got: %v", err)
	}

	// Projects are unaffected: they are owned by their requester, not managed.
	if _, err := svc.CreateNode(tree.CreateNodeRequest{
		ParentID: tree.RootNodeID,
		Kind:     tree.KindProject,
		Reason:   "test",
		Limit:    cores(1),
	}, "root@x", "root@x", rootTokens); err != nil {
		t.Fatalf("create project: %v", err)
	}
}

// A budget may accept project requests while refusing sub-budget requests —
// "students may ask for a VM here, but not carve out their own budget".
func TestAllowSubBudgetRequests(t *testing.T) {
	svc, _ := newSvc(t, common.TokenList{"group:root"})
	rootTokens := common.TokenList{"user:root@x", "group:root"}
	studentTokens := common.TokenList{"user:s1@x", "group:students"}
	no := false

	budget, err := svc.CreateNode(tree.CreateNodeRequest{
		ParentID:               tree.RootNodeID,
		Kind:                   tree.KindBudget,
		Name:                   "Course",
		Reason:                 "test",
		Limit:                  cores(10),
		AdminScope:             common.TokenList{"group:root"},
		EligibleRequesters:     common.TokenList{"group:students"},
		AllowSubBudgetRequests: &no,
	}, "root@x", "root@x", rootTokens)
	if err != nil {
		t.Fatalf("create budget: %v", err)
	}

	// The requester may still ask for a project.
	if _, err := svc.CreateNode(tree.CreateNodeRequest{
		ParentID: budget.ID, Kind: tree.KindProject, Reason: "vm", Limit: cores(1),
	}, "s1@x", "s1@x", studentTokens); err != nil {
		t.Fatalf("project request should be allowed: %v", err)
	}

	// ... but not for a sub-budget.
	_, err = svc.CreateNode(tree.CreateNodeRequest{
		ParentID: budget.ID, Kind: tree.KindBudget, Name: "Mine", Reason: "test",
		Limit: cores(1), AdminScope: common.TokenList{"user:s1@x"},
	}, "s1@x", "s1@x", studentTokens)
	if err == nil {
		t.Fatal("sub-budget request should be refused")
	}

	// A manager is not restricted by the flag — they shape the tree.
	if _, err := svc.CreateNode(tree.CreateNodeRequest{
		ParentID: budget.ID, Kind: tree.KindBudget, Name: "Manager's",
		Reason: "test", Limit: cores(1), AdminScope: common.TokenList{"group:root"},
	}, "root@x", "root@x", rootTokens); err != nil {
		t.Fatalf("manager should still create sub-budgets: %v", err)
	}

	// Omitting the field keeps the old behaviour: requests are allowed.
	open, err := svc.CreateNode(tree.CreateNodeRequest{
		ParentID: tree.RootNodeID, Kind: tree.KindBudget, Name: "Open", Reason: "test",
		Limit: cores(10), AdminScope: common.TokenList{"group:root"},
		EligibleRequesters: common.TokenList{"group:students"},
	}, "root@x", "root@x", rootTokens)
	if err != nil {
		t.Fatalf("create open budget: %v", err)
	}
	if _, err := svc.CreateNode(tree.CreateNodeRequest{
		ParentID: open.ID, Kind: tree.KindBudget, Name: "Sub", Reason: "test",
		Limit: cores(1), AdminScope: common.TokenList{"user:s1@x"},
	}, "s1@x", "s1@x", studentTokens); err != nil {
		t.Fatalf("sub-budget request should be allowed by default: %v", err)
	}
}

// The tree renders children lazily, so a budget only gets an expand control when
// the server says it has children — an empty budget must report zero.
func TestChildCountIsAttached(t *testing.T) {
	svc, _ := newSvc(t, common.TokenList{"group:root"})
	rootTokens := common.TokenList{"user:root@x", "group:root"}

	parent, err := svc.CreateNode(tree.CreateNodeRequest{
		ParentID: tree.RootNodeID, Kind: tree.KindBudget, Name: "Parent", Reason: "test",
		Limit: cores(10), AdminScope: common.TokenList{"group:root"},
	}, "root@x", "root@x", rootTokens)
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	empty, err := svc.CreateNode(tree.CreateNodeRequest{
		ParentID: parent.ID, Kind: tree.KindBudget, Name: "Empty", Reason: "test",
		Limit: cores(1), AdminScope: common.TokenList{"group:root"},
	}, "root@x", "root@x", rootTokens)
	if err != nil {
		t.Fatalf("create empty child: %v", err)
	}

	page, err := svc.ListChildren(parent.ID, rootTokens, 0, 0)
	if err != nil {
		t.Fatalf("list children: %v", err)
	}
	children := page.Items
	if len(children) != 1 || page.Total != 1 {
		t.Fatalf("expected 1 child (total 1), got %d (total %d)", len(children), page.Total)
	}
	if children[0].ChildCount != 0 {
		t.Errorf("the empty budget should report child_count 0, got %d", children[0].ChildCount)
	}

	// Give it a leaf and the count follows.
	if _, err := svc.CreateNode(tree.CreateNodeRequest{
		ParentID: empty.ID, Kind: tree.KindProject, Reason: "vm", Limit: cores(1),
	}, "root@x", "root@x", rootTokens); err != nil {
		t.Fatalf("create leaf: %v", err)
	}
	page, err = svc.ListChildren(parent.ID, rootTokens, 0, 0)
	if err != nil {
		t.Fatalf("list children: %v", err)
	}
	if page.Items[0].ChildCount != 1 {
		t.Errorf("expected child_count 1 after adding a leaf, got %d", children[0].ChildCount)
	}
}

// "My Projects" names the budget each project is paid from. The name travels
// with the node so the client does not fetch every parent on its own.
func TestParentNameIsAttached(t *testing.T) {
	svc, _ := newSvc(t, common.TokenList{"group:root"})
	rootTokens := common.TokenList{"user:root@x", "group:root"}

	budget, err := svc.CreateNode(tree.CreateNodeRequest{
		ParentID: tree.RootNodeID, Kind: tree.KindBudget, Name: "Course WI", Reason: "test",
		Limit: cores(10), AdminScope: common.TokenList{"group:root"},
		EligibleRequesters: common.TokenList{"user:student@x"},
	}, "root@x", "root@x", rootTokens)
	if err != nil {
		t.Fatalf("create budget: %v", err)
	}
	if _, err := svc.CreateNode(tree.CreateNodeRequest{
		ParentID: budget.ID, Kind: tree.KindProject, Reason: "vm", Limit: cores(1),
	}, "student@x", "student@x", common.TokenList{"user:student@x"}); err != nil {
		t.Fatalf("create leaf: %v", err)
	}

	mine, err := svc.ListMine("student@x", 0, 0)
	if err != nil {
		t.Fatalf("list mine: %v", err)
	}
	if len(mine.Items) != 1 {
		t.Fatalf("expected 1 project, got %d", len(mine.Items))
	}
	if mine.Items[0].ParentName != "Course WI" {
		t.Errorf("expected parent_name %q, got %q", "Course WI", mine.Items[0].ParentName)
	}

	// The root itself has no parent to name.
	root, err := svc.GetNode(tree.RootNodeID, rootTokens)
	if err != nil {
		t.Fatalf("get root: %v", err)
	}
	if root.ParentName != "" {
		t.Errorf("the root has no parent, got parent_name %q", root.ParentName)
	}
}

// An end date can be removed again. A nil TerminationDate means "leave as is",
// so without the explicit flag the UI's "no end date" switch would silently do
// nothing on a budget that has one.
func TestClearTerminationDate(t *testing.T) {
	svc, _ := newSvc(t, common.TokenList{"group:root"})
	rootTokens := common.TokenList{"user:root@x", "group:root"}
	ends := "2027-01-01T00:00:00Z"

	budget, err := svc.CreateNode(tree.CreateNodeRequest{
		ParentID:        tree.RootNodeID,
		Kind:            tree.KindBudget,
		Name:            "Course",
		Reason:          "test",
		Limit:           cores(10),
		AdminScope:      common.TokenList{"group:root"},
		TerminationDate: &ends,
	}, "root@x", "root@x", rootTokens)
	if err != nil {
		t.Fatalf("create budget: %v", err)
	}
	if budget.TerminationDate == nil {
		t.Fatal("budget should have been created with an end date")
	}

	// Without the flag the date survives — that is what "leave as is" means.
	untouched, err := svc.UpdateNode(budget.ID, tree.UpdateNodeRequest{}, "root@x", rootTokens)
	if err != nil {
		t.Fatalf("update without changes: %v", err)
	}
	if untouched.TerminationDate == nil {
		t.Error("an update that says nothing about the end date must not remove it")
	}

	cleared, err := svc.UpdateNode(budget.ID, tree.UpdateNodeRequest{
		ClearTerminationDate: true,
	}, "root@x", rootTokens)
	if err != nil {
		t.Fatalf("clear termination date: %v", err)
	}
	if cleared.TerminationDate != nil {
		t.Errorf("end date should be gone, got %q", *cleared.TerminationDate)
	}
}
