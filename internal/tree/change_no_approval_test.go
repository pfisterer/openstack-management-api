package tree_test

import (
	"context"
	"testing"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/roleprovider"
	"github.com/pfisterer/openstack-management-api/internal/tree"
	"go.uber.org/zap"
)

// These tests need a second resource to express a mixed change (one resource
// down, another up), which the single-resource newSvc cannot.
var twoResources = []string{"cores", "ram"}

var (
	changeRootTokens    = common.TokenList{"user:root@x", "group:root"}
	changeStudentTokens = common.TokenList{"user:student@x"}
)

func quota(coresValue, ramValue int) common.ProjectQuota {
	return common.ProjectQuota{"cores": coresValue, "ram": ramValue}
}

func newTwoResourceSvc(t *testing.T) (*tree.Service, tree.Store) {
	t.Helper()
	log := zap.NewNop().Sugar()
	store := tree.NewInMemoryStore(log)
	svc := tree.NewService(store, roleprovider.NewMockRoleProvider(), twoResources,
		common.TokenList{"group:root"}, 5*time.Second, common.DefaultMaxAuthorizedUsers, log)
	if err := svc.Bootstrap(context.Background(), nil, nil); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return svc, store
}

// changeFixture returns a budget the student may request under.
func changeFixture(t *testing.T) (*tree.Service, tree.Store, tree.Node) {
	t.Helper()
	svc, store := newTwoResourceSvc(t)
	budget, err := svc.CreateNode(tree.CreateNodeRequest{
		ParentID:           tree.RootNodeID,
		Kind:               tree.KindBudget,
		Name:               "Course",
		Reason:             "course budget",
		Limit:              quota(100, 100),
		AdminScope:         common.TokenList{"group:root"},
		EligibleRequesters: common.TokenList{"user:student@x"},
	}, "root@x", "root@x", changeRootTokens)
	if err != nil {
		t.Fatalf("create budget: %v", err)
	}
	return svc, store, budget
}

// approvedLeaf requests a project as the student and has root approve it, so the
// leaf is approved AND owned by the student (a manager-created leaf would be
// owned by the manager).
func approvedLeaf(t *testing.T, svc *tree.Service, budgetID string, req tree.CreateNodeRequest) tree.Node {
	t.Helper()
	req.ParentID = budgetID
	req.Kind = tree.KindProject
	if req.Name == "" {
		req.Name = "vm"
	}
	if req.Reason == "" {
		req.Reason = "lab work"
	}
	node, err := svc.CreateNode(req, "student@x", "student@x", changeStudentTokens)
	if err != nil {
		t.Fatalf("create leaf: %v", err)
	}
	approved, err := svc.ApproveNode(node.ID, tree.ApproveNodeRequest{}, "root@x", changeRootTokens)
	if err != nil {
		t.Fatalf("approve leaf: %v", err)
	}
	return approved
}

func lastHistory(t *testing.T, node tree.Node) tree.HistoryEntry {
	t.Helper()
	if len(node.History) == 0 {
		t.Fatalf("node %s has no history", node.ID)
	}
	return node.History[len(node.History)-1]
}

// reload proves the immediate change was persisted, not only returned.
func reload(t *testing.T, store tree.Store, id string) tree.Node {
	t.Helper()
	node, err := store.GetNode(context.Background(), id)
	if err != nil || node == nil {
		t.Fatalf("reload %s: %v", id, err)
	}
	return *node
}

// Giving capacity back costs nobody anything, so it must not wait for a manager.
func TestRequestChange_ShrinkAppliesImmediately(t *testing.T) {
	svc, store, budget := changeFixture(t)
	leaf := approvedLeaf(t, svc, budget.ID, tree.CreateNodeRequest{Limit: quota(4, 8)})

	changed, err := svc.RequestChange(leaf.ID, tree.ChangeNodeRequest{
		Limit:  ptrQuota(quota(2, 8)), // cores down, ram unchanged
		Reason: ptrString("half the VMs are idle"),
	}, "student@x", changeStudentTokens)
	if err != nil {
		t.Fatalf("request change: %v", err)
	}

	if changed.Status != tree.StatusApproved {
		t.Errorf("status = %q, want %q", changed.Status, tree.StatusApproved)
	}
	if changed.Pending != nil {
		t.Errorf("no proposal may be left behind, got %+v", changed.Pending)
	}
	if changed.Limit["cores"] != 2 || changed.Limit["ram"] != 8 {
		t.Errorf("limit = %v, want cores 2 / ram 8", changed.Limit)
	}

	entry := lastHistory(t, changed)
	if entry.Event != "change_applied_without_approval" {
		t.Errorf("history event = %q, it must say that no approval was needed", entry.Event)
	}
	if entry.Actor != "student@x" {
		t.Errorf("history actor = %q, want the requester", entry.Actor)
	}
	if entry.StatusFrom == nil || *entry.StatusFrom != tree.StatusApproved || entry.StatusTo != tree.StatusApproved {
		t.Errorf("history status %v → %q, want approved → approved", entry.StatusFrom, entry.StatusTo)
	}
	if entry.LimitFrom == nil || (*entry.LimitFrom)["cores"] != 4 {
		t.Errorf("history limit_from = %v, want the old 4 cores", entry.LimitFrom)
	}
	if entry.LimitTo == nil || (*entry.LimitTo)["cores"] != 2 {
		t.Errorf("history limit_to = %v, want the new 2 cores", entry.LimitTo)
	}

	if stored := reload(t, store, leaf.ID); stored.Limit["cores"] != 2 || stored.Status != tree.StatusApproved {
		t.Errorf("stored node = %q / %v, want approved with 2 cores", stored.Status, stored.Limit)
	}
}

// The old, cheaper limit must stay in force until a manager says otherwise.
func TestRequestChange_GrowthStillNeedsApproval(t *testing.T) {
	svc, _, budget := changeFixture(t)
	leaf := approvedLeaf(t, svc, budget.ID, tree.CreateNodeRequest{Limit: quota(4, 8)})

	changed, err := svc.RequestChange(leaf.ID, tree.ChangeNodeRequest{
		Limit: ptrQuota(quota(5, 8)),
	}, "student@x", changeStudentTokens)
	if err != nil {
		t.Fatalf("request change: %v", err)
	}
	if changed.Status != tree.StatusChangePending {
		t.Fatalf("status = %q, want %q", changed.Status, tree.StatusChangePending)
	}
	if changed.Limit["cores"] != 4 {
		t.Errorf("the approved limit must not move before approval, got %v", changed.Limit)
	}
	if changed.Pending == nil || changed.Pending.Limit == nil || (*changed.Pending.Limit)["cores"] != 5 {
		t.Errorf("the proposal should carry the requested 5 cores, got %+v", changed.Pending)
	}
}

// A request is approved as a whole or not at all: one growing resource drags the
// shrinking one through the approval cycle with it.
func TestRequestChange_MixedChangeNeedsApproval(t *testing.T) {
	svc, _, budget := changeFixture(t)
	leaf := approvedLeaf(t, svc, budget.ID, tree.CreateNodeRequest{Limit: quota(4, 8)})

	changed, err := svc.RequestChange(leaf.ID, tree.ChangeNodeRequest{
		Limit: ptrQuota(quota(2, 16)), // cores down, ram up
	}, "student@x", changeStudentTokens)
	if err != nil {
		t.Fatalf("request change: %v", err)
	}
	if changed.Status != tree.StatusChangePending {
		t.Errorf("status = %q, want %q", changed.Status, tree.StatusChangePending)
	}
	if changed.Limit["cores"] != 4 {
		t.Errorf("nothing may be applied early, got %v", changed.Limit)
	}
}

// Restating the purpose changes no allocation, so there is nothing to approve.
func TestRequestChange_ReasonOnlyAppliesImmediately(t *testing.T) {
	svc, store, budget := changeFixture(t)
	leaf := approvedLeaf(t, svc, budget.ID, tree.CreateNodeRequest{Limit: quota(4, 8)})

	changed, err := svc.RequestChange(leaf.ID, tree.ChangeNodeRequest{
		Reason: ptrString("now used for the bachelor thesis"),
	}, "student@x", changeStudentTokens)
	if err != nil {
		t.Fatalf("request change: %v", err)
	}
	if changed.Status != tree.StatusApproved || changed.Pending != nil {
		t.Fatalf("status = %q / pending = %+v, want approved without a proposal", changed.Status, changed.Pending)
	}
	if changed.Reason != "now used for the bachelor thesis" {
		t.Errorf("the new purpose must be in effect, got %q", changed.Reason)
	}
	if changed.Limit["cores"] != 4 || changed.Limit["ram"] != 8 {
		t.Errorf("a metadata change must not touch the limit, got %v", changed.Limit)
	}
	if stored := reload(t, store, leaf.ID); stored.Reason != changed.Reason {
		t.Errorf("stored purpose = %q, want %q", stored.Reason, changed.Reason)
	}
}

// An empty request is still an error — the fast path must not swallow it.
func TestRequestChange_EmptyRequestIsRejected(t *testing.T) {
	svc, _, budget := changeFixture(t)
	leaf := approvedLeaf(t, svc, budget.ID, tree.CreateNodeRequest{Limit: quota(4, 8)})

	if _, err := svc.RequestChange(leaf.ID, tree.ChangeNodeRequest{}, "student@x", changeStudentTokens); err == nil {
		t.Fatal("a change request without any change must fail")
	}
}

// UnlimitedQuota (-1) is the LARGEST value: replacing it with a concrete number
// is a reduction, even though -1 compares as smaller than everything.
func TestRequestChange_UnlimitedToConcreteIsAShrink(t *testing.T) {
	svc, store, budget := changeFixture(t)

	// A leaf limit of -1 cannot be requested through the API (validateLeafLimit
	// refuses negatives), so seed one the way an old record would look.
	parentID := budget.ID
	seeded := tree.Node{
		ID:       "p_legacy_unlimited",
		Kind:     tree.KindProject,
		ParentID: &parentID,
		Status:   tree.StatusApproved,
		Name:     "legacy",
		Reason:   "seeded",
		Limit:    quota(common.UnlimitedQuota, common.UnlimitedQuota),
		Owner:    "user:student@x",
	}
	if err := store.UpsertNode(context.Background(), seeded); err != nil {
		t.Fatalf("seed leaf: %v", err)
	}

	changed, err := svc.RequestChange(seeded.ID, tree.ChangeNodeRequest{
		Limit: ptrQuota(quota(4, 8)),
	}, "student@x", changeStudentTokens)
	if err != nil {
		t.Fatalf("request change: %v", err)
	}
	if changed.Status != tree.StatusApproved {
		t.Errorf("status = %q, capping an unlimited leaf gives capacity back", changed.Status)
	}
	if changed.Limit["cores"] != 4 {
		t.Errorf("limit = %v, want the new cap applied", changed.Limit)
	}
}

// The other direction never reaches the fast path: a leaf may not be unlimited at
// all, so validation refuses it before the question is asked.
func TestRequestChange_ConcreteToUnlimitedIsRefused(t *testing.T) {
	svc, store, budget := changeFixture(t)
	leaf := approvedLeaf(t, svc, budget.ID, tree.CreateNodeRequest{Limit: quota(4, 8)})

	if _, err := svc.RequestChange(leaf.ID, tree.ChangeNodeRequest{
		Limit: ptrQuota(quota(common.UnlimitedQuota, 8)),
	}, "student@x", changeStudentTokens); err == nil {
		t.Fatal("an unlimited leaf limit must be refused")
	}
	if stored := reload(t, store, leaf.ID); stored.Status != tree.StatusApproved || stored.Limit["cores"] != 4 {
		t.Errorf("the refused request must leave the node alone, got %q / %v", stored.Status, stored.Limit)
	}
}

// Ending earlier hands the capacity back sooner; running longer keeps the claim
// on the parent budget alive and needs a decision.
func TestRequestChange_TerminationDate(t *testing.T) {
	cases := []struct {
		name       string
		current    *string
		proposed   string
		wantStatus string
	}{
		{"an earlier end date is a reduction", ptrString("2027-01-01T00:00:00Z"), "2026-09-01T00:00:00Z", tree.StatusApproved},
		{"a later end date is an extension", ptrString("2027-01-01T00:00:00Z"), "2028-01-01T00:00:00Z", tree.StatusChangePending},
		{"a bare calendar day is understood", ptrString("2027-01-01T00:00:00Z"), "2026-09-01", tree.StatusApproved},
		{"naming a day where there was none shortens an open-ended node", nil, "2027-01-01T00:00:00Z", tree.StatusApproved},
		{"an unparseable date is left to a manager", ptrString("2027-01-01T00:00:00Z"), "next semester", tree.StatusChangePending},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, budget := changeFixture(t)
			leaf := approvedLeaf(t, svc, budget.ID, tree.CreateNodeRequest{
				Limit: quota(4, 8), TerminationDate: tc.current,
			})
			changed, err := svc.RequestChange(leaf.ID, tree.ChangeNodeRequest{
				TerminationDate: ptrString(tc.proposed),
			}, "student@x", changeStudentTokens)
			if err != nil {
				t.Fatalf("request change: %v", err)
			}
			if changed.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", changed.Status, tc.wantStatus)
			}
		})
	}
}

// Membership is an access decision, not a budget one — even a shorter list is
// reviewed, because it may have swapped one person for another.
func TestRequestChange_AuthorizedUsersAlwaysNeedApproval(t *testing.T) {
	svc, _, budget := changeFixture(t)
	leaf := approvedLeaf(t, svc, budget.ID, tree.CreateNodeRequest{
		Limit:           quota(4, 8),
		AuthorizedUsers: []common.AuthorizedUser{{Token: "user:helper@x.example", OpenstackRole: "member"}},
	})

	changed, err := svc.RequestChange(leaf.ID, tree.ChangeNodeRequest{
		AuthorizedUsers: &[]common.AuthorizedUser{}, // removing everybody
	}, "student@x", changeStudentTokens)
	if err != nil {
		t.Fatalf("request change: %v", err)
	}
	if changed.Status != tree.StatusChangePending {
		t.Errorf("status = %q, want %q", changed.Status, tree.StatusChangePending)
	}
	if len(changed.AuthorizedUsers) != 1 {
		t.Errorf("the member list must not change before approval, got %v", changed.AuthorizedUsers)
	}
}

// Shrinking a BUDGET can strand approved children below it, so the fast path is
// for leaves only.
func TestRequestChange_BudgetShrinkStillNeedsApproval(t *testing.T) {
	svc, _, budget := changeFixture(t)
	sub, err := svc.CreateNode(tree.CreateNodeRequest{
		ParentID: budget.ID, Kind: tree.KindBudget, Name: "Sub", Reason: "test",
		Limit: quota(50, 50), AdminScope: common.TokenList{"group:root"},
	}, "root@x", "root@x", changeRootTokens)
	if err != nil {
		t.Fatalf("create sub-budget: %v", err)
	}

	changed, err := svc.RequestChange(sub.ID, tree.ChangeNodeRequest{
		Limit: ptrQuota(quota(10, 10)),
	}, "root@x", changeRootTokens)
	if err != nil {
		t.Fatalf("request change: %v", err)
	}
	if changed.Status != tree.StatusChangePending {
		t.Errorf("status = %q, want %q", changed.Status, tree.StatusChangePending)
	}
	if changed.Limit["cores"] != 50 {
		t.Errorf("the budget limit must not move before approval, got %v", changed.Limit)
	}
}

// A node whose proposal a manager is already looking at keeps its guard: a later
// shrink replaces the proposal instead of deciding half of it silently.
func TestRequestChange_ChangePendingNodeIsUnaffected(t *testing.T) {
	svc, _, budget := changeFixture(t)
	leaf := approvedLeaf(t, svc, budget.ID, tree.CreateNodeRequest{Limit: quota(4, 8)})

	if _, err := svc.RequestChange(leaf.ID, tree.ChangeNodeRequest{
		Limit: ptrQuota(quota(16, 8)),
	}, "student@x", changeStudentTokens); err != nil {
		t.Fatalf("first change request: %v", err)
	}

	changed, err := svc.RequestChange(leaf.ID, tree.ChangeNodeRequest{
		Limit: ptrQuota(quota(1, 8)),
	}, "student@x", changeStudentTokens)
	if err != nil {
		t.Fatalf("second change request: %v", err)
	}
	if changed.Status != tree.StatusChangePending {
		t.Errorf("status = %q, want %q", changed.Status, tree.StatusChangePending)
	}
	if changed.Limit["cores"] != 4 {
		t.Errorf("the approved limit must still stand, got %v", changed.Limit)
	}
	if changed.Pending == nil || (*changed.Pending.Limit)["cores"] != 1 {
		t.Errorf("the proposal should now be the 1-core one, got %+v", changed.Pending)
	}
}

func ptrString(s string) *string { return &s }
