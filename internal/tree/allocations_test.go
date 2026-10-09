package tree_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/roleprovider"
	"github.com/pfisterer/openstack-management-api/internal/tree"
	"go.uber.org/zap"
)

var allocationResources = []common.ManagedProject{
	{ID: "cores", Name: "Cores"},
	{ID: "gpu", Name: "GPUs"},
	{ID: "ipv4", Name: "IPv4", Kind: common.KindBool},
}

var (
	uniTokens  = common.TokenList{"user:uni@x", "group:uni"}
	studMgr    = common.TokenList{"user:mgr@x", "group:stud"}
	ownerToken = common.TokenList{"user:stud@x"}
)

// allocationFixture: a university budget holding GPUs and IPv4, below it a
// student budget holding neither, and one student project of 4 cores in it.
type allocationFixture struct {
	svc       *tree.Service
	store     *tree.InMemoryStore
	uni, stud tree.Node
	project   tree.Node
}

func newAllocationFixture(t *testing.T) allocationFixture {
	t.Helper()
	log := zap.NewNop().Sugar()
	store := tree.NewInMemoryStore(log)
	svc := tree.NewService(store, roleprovider.NewMockRoleProvider(), allocationResources,
		common.TokenList{"group:root"}, 5*time.Second, common.DefaultMaxAuthorizedUsers, testAccounting, log)
	if err := svc.Bootstrap(context.Background(), nil, nil); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	create := func(req tree.CreateNodeRequest, email string, tokens common.TokenList) tree.Node {
		t.Helper()
		req.Reason = "t"
		if req.Name == "" {
			req.Name = "n"
		}
		n, err := svc.CreateNode(req, tree.UIActor(email), email, tokens)
		if err != nil {
			t.Fatalf("create %s: %v", req.Name, err)
		}
		return n
	}
	uni := create(tree.CreateNodeRequest{ParentID: tree.RootNodeID, Kind: tree.KindBudget, Name: "Uni",
		Limit: common.ProjectQuota{"cores": 100, "gpu": 4, "ipv4": 1}, AdminScope: common.TokenList{"group:uni"}}, "root@x", rootTokens)
	stud := create(tree.CreateNodeRequest{ParentID: uni.ID, Kind: tree.KindBudget, Name: "Students",
		Limit: common.ProjectQuota{"cores": 10}, AdminScope: common.TokenList{"group:stud"},
		EligibleRequesters: common.TokenList{"user:stud@x"},
		AutoApprove:        &tree.AutoApprove{PerRequesterLimit: common.ProjectQuota{"cores": 4}}}, "uni@x", uniTokens)
	p := create(tree.CreateNodeRequest{ParentID: stud.ID, Kind: tree.KindProject, Name: "Thesis",
		Limit: common.ProjectQuota{"cores": 4}}, "stud@x", ownerToken)
	if p.Status != tree.StatusApproved {
		t.Fatalf("setup: the project should be auto-approved, got %q", p.Status)
	}
	return allocationFixture{svc: svc, store: store, uni: uni, stud: stud, project: p}
}

func (f allocationFixture) allocate(t *testing.T, tokens common.TokenList, limit common.ProjectQuota, reason string) (tree.Node, error) {
	t.Helper()
	return f.svc.SetAllocation(f.project.ID, tree.AllocationRequest{BudgetID: f.uni.ID, Limit: limit, Reason: reason},
		tree.UIActor("someone@x"), tokens)
}

func usageOf(t *testing.T, svc *tree.Service, id string) common.ProjectQuota {
	t.Helper()
	n := mustGet(t, svc, id)
	return n.Usage.Total([]string{"cores", "gpu"})
}

// A manager of the budget above grants what the student budget does not hold;
// it is charged there and above, never to the student budget.
func TestAllocation_GrantedFromAboveAndChargedThere(t *testing.T) {
	f := newAllocationFixture(t)

	p, err := f.allocate(t, uniTokens, common.ProjectQuota{"cores": 12, "gpu": 1, "ipv4": 1}, "thesis with model training")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	total := p.EffectiveLimit()
	if total["cores"] != 16 || total["gpu"] != 1 || total["ipv4"] != 1 {
		t.Errorf("effective limit = %v, want 16 cores, 1 gpu, ipv4", total)
	}
	if p.Limit["cores"] != 4 || p.Limit["gpu"] != 0 {
		t.Errorf("the project's own limit must stay as it was, got %v", p.Limit)
	}
	if got := usageOf(t, f.svc, f.stud.ID); got["cores"] != 4 || got["gpu"] != 0 {
		t.Errorf("the student budget must carry only the project's own share, got %v", got)
	}
	if got := usageOf(t, f.svc, f.uni.ID); got["cores"] != 16 || got["gpu"] != 1 {
		t.Errorf("the university budget must carry both, got %v", got)
	}
	if out := mustGet(t, f.svc, f.uni.ID).AllocatedOut; out == nil || out.Projects != 1 || out.Limit["cores"] != 12 || out.Limit["ipv4"] != 1 {
		t.Errorf("allocated_out = %+v, want 1 project with 12 cores and ipv4", out)
	}
	if last := p.History[len(p.History)-1]; last.Event != "allocation_set" || last.AllocationFrom == nil || *last.AllocationFrom != f.uni.ID {
		t.Errorf("history should record the allocation and its budget, got %+v", last)
	}
}

// Who may do what: granting belongs to the budget that pays, giving back to
// the project's holders; the managers in between have no say either way.
func TestAllocation_WhoMay(t *testing.T) {
	f := newAllocationFixture(t)
	gpu := common.ProjectQuota{"gpu": 1}

	if _, err := f.allocate(t, studMgr, gpu, "x"); !errors.Is(err, common.ErrForbidden) {
		t.Errorf("a manager of the budget in between must not grant, got %v", err)
	}
	if _, err := f.allocate(t, ownerToken, gpu, "x"); !errors.Is(err, common.ErrForbidden) {
		t.Errorf("the owner must not grant themselves, got %v", err)
	}
	if _, err := f.allocate(t, uniTokens, gpu, " "); err == nil || !strings.Contains(err.Error(), "reason") {
		t.Errorf("granting without a reason must be refused, got %v", err)
	}
	if _, err := f.allocate(t, rootTokens, common.ProjectQuota{"gpu": 2}, "x"); err != nil {
		t.Fatalf("a manager further up may grant: %v", err)
	}
	if _, err := f.allocate(t, studMgr, gpu, ""); !errors.Is(err, common.ErrForbidden) {
		t.Errorf("a manager of the budget in between must not lower it either, got %v", err)
	}
	if _, err := f.allocate(t, ownerToken, common.ProjectQuota{"gpu": 3}, "x"); !errors.Is(err, common.ErrForbidden) {
		t.Errorf("the owner must not raise it, got %v", err)
	}
	if p, err := f.allocate(t, ownerToken, gpu, ""); err != nil || p.EffectiveLimit()["gpu"] != 1 {
		t.Fatalf("the owner may give part of it back: %v", err)
	}
	p, err := f.allocate(t, ownerToken, common.ProjectQuota{}, "")
	if err != nil || len(p.Allocations) != 0 {
		t.Fatalf("the owner may give it back entirely: %v, %+v", err, p.Allocations)
	}
	if p.History[len(p.History)-1].Event != "allocation_removed" {
		t.Errorf("removal should be recorded as such")
	}
}

// Only a budget above the project's own may allocate, and only what it has room for.
func TestAllocation_SourceAndCapacity(t *testing.T) {
	f := newAllocationFixture(t)

	if _, err := f.svc.SetAllocation(f.project.ID, tree.AllocationRequest{BudgetID: f.stud.ID, Limit: cores(2), Reason: "x"},
		tree.UIActor("mgr@x"), studMgr); err == nil {
		t.Error("the project's own budget must not allocate — that is a change request")
	}
	other := mustCreate(t, f.svc, tree.CreateNodeRequest{ParentID: tree.RootNodeID, Kind: tree.KindBudget, Limit: common.ProjectQuota{"gpu": 8}})
	if _, err := f.svc.SetAllocation(f.project.ID, tree.AllocationRequest{BudgetID: other.ID, Limit: common.ProjectQuota{"gpu": 1}, Reason: "x"},
		rootActor(), rootTokens); err == nil {
		t.Error("a budget that is not above the project must not allocate to it")
	}
	if _, err := f.allocate(t, uniTokens, common.ProjectQuota{"gpu": 5}, "x"); err == nil {
		t.Error("an allocation past the budget's room must be refused")
	}
	if _, err := f.allocate(t, uniTokens, common.ProjectQuota{"cores": 97}, "x"); err == nil {
		t.Error("cores past the room left beside the project's own 4 must be refused")
	}
	if _, err := f.allocate(t, uniTokens, common.ProjectQuota{"cores": 96}, "x"); err != nil {
		t.Errorf("exactly the room left must be granted: %v", err)
	}
}

// An availability comes from one place, so its withdrawal has one answer.
func TestAllocation_AvailabilityFromOnePlace(t *testing.T) {
	f := newAllocationFixture(t)
	if _, err := f.allocate(t, uniTokens, common.ProjectQuota{"ipv4": 1}, "public address"); err != nil {
		t.Fatalf("allocate ipv4: %v", err)
	}

	// The university cannot withdraw it while the project depends on it.
	if _, err := f.svc.UpdateNode(f.uni.ID, tree.UpdateNodeRequest{Limit: &common.ProjectQuota{"cores": 100, "gpu": 4}}, rootActor(), rootTokens); err == nil {
		t.Error("withdrawing an availability a project holds by allocation must be refused")
	}

	// The student budget gets ipv4 too; its own projects may not ask for it
	// again through their own limit while the allocation grants it.
	if _, err := f.svc.UpdateNode(f.stud.ID, tree.UpdateNodeRequest{Limit: &common.ProjectQuota{"cores": 10, "ipv4": 1}}, tree.UIActor("uni@x"), uniTokens); err != nil {
		t.Fatalf("give the student budget ipv4: %v", err)
	}
	if _, err := f.svc.RequestChange(f.project.ID, tree.ChangeNodeRequest{Limit: &common.ProjectQuota{"cores": 4, "ipv4": 1}}, tree.UIActor("stud@x"), ownerToken); err == nil {
		t.Error("asking for an availability the project already has by allocation must be refused")
	}
	// And the student budget may take it back: nothing of it depends on it.
	if _, err := f.svc.UpdateNode(f.stud.ID, tree.UpdateNodeRequest{Limit: &common.ProjectQuota{"cores": 10}}, tree.UIActor("uni@x"), uniTokens); err != nil {
		t.Errorf("the student budget's withdrawal must not be blocked by an allocation from above: %v", err)
	}
}

// The other way round: what the project holds itself is not allocated again.
func TestAllocation_RefusesAnAvailabilityTheProjectHolds(t *testing.T) {
	f := newAllocationFixture(t)
	if _, err := f.svc.UpdateNode(f.stud.ID, tree.UpdateNodeRequest{Limit: &common.ProjectQuota{"cores": 10, "ipv4": 1}}, tree.UIActor("uni@x"), uniTokens); err != nil {
		t.Fatalf("give the student budget ipv4: %v", err)
	}
	if _, err := f.svc.RequestChange(f.project.ID, tree.ChangeNodeRequest{Limit: &common.ProjectQuota{"cores": 4, "ipv4": 1}}, tree.UIActor("mgr@x"), studMgr); err != nil {
		t.Fatalf("project asks its own budget for ipv4: %v", err)
	}
	if _, err := f.allocate(t, uniTokens, common.ProjectQuota{"ipv4": 1}, "x"); err == nil || !strings.Contains(err.Error(), "already") {
		t.Errorf("an availability the project has from its own budget must not be allocated again, got %v", err)
	}
}

// The project's own rules stay with its own share: the person's auto-approve
// share counts the 4 cores, not the allocation.
func TestAllocation_OwnShareUnaffected(t *testing.T) {
	f := newAllocationFixture(t)
	if _, err := f.allocate(t, uniTokens, cores(20), "x"); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	n, err := f.svc.RequestChange(f.project.ID, tree.ChangeNodeRequest{Limit: &common.ProjectQuota{"cores": 3}}, tree.UIActor("stud@x"), ownerToken)
	if err != nil || n.Status != tree.StatusApproved {
		t.Fatalf("shrinking the own share should apply at once: %v %q", err, n.Status)
	}
	if n.EffectiveLimit()["cores"] != 23 || len(n.Allocations) != 1 {
		t.Errorf("a change request must touch only the own share, got %v / %+v", n.EffectiveLimit(), n.Allocations)
	}
}

// A move may not leave an allocation coming from a budget no longer above.
func TestAllocation_Move(t *testing.T) {
	f := newAllocationFixture(t)
	if _, err := f.allocate(t, uniTokens, common.ProjectQuota{"gpu": 1}, "x"); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	sibling := mustCreate(t, f.svc, tree.CreateNodeRequest{ParentID: f.uni.ID, Kind: tree.KindBudget, Limit: cores(10)})
	outside := mustCreate(t, f.svc, tree.CreateNodeRequest{ParentID: tree.RootNodeID, Kind: tree.KindBudget, Limit: cores(10)})

	if _, err := f.svc.ReparentNode(f.project.ID, tree.ReparentNodeRequest{NewParentID: outside.ID}, rootActor(), rootTokens); err == nil {
		t.Error("moving away from the allocating budget must be refused")
	}
	if _, err := f.svc.ReparentNode(f.stud.ID, tree.ReparentNodeRequest{NewParentID: outside.ID}, rootActor(), rootTokens); err == nil {
		t.Error("moving the student budget away must be refused too")
	}
	if _, err := f.svc.ReparentNode(f.project.ID, tree.ReparentNodeRequest{NewParentID: sibling.ID}, rootActor(), rootTokens); err != nil {
		t.Errorf("a move that keeps the allocating budget above is fine: %v", err)
	}
}

// The allocating budget finds its exceptions however deep they lie.
func TestAllocation_ListedAtTheAllocatingBudget(t *testing.T) {
	f := newAllocationFixture(t)
	other := mustCreate(t, f.svc, tree.CreateNodeRequest{ParentID: f.stud.ID, Kind: tree.KindProject, Limit: cores(1)})
	if _, err := f.allocate(t, uniTokens, common.ProjectQuota{"gpu": 1}, "x"); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	page, err := f.svc.ListChildren(f.uni.ID, tree.ChildFilter{Allocated: true}, uniTokens, 50, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if page.Total != 1 || page.Items[0].ID != f.project.ID {
		t.Errorf("want only the project with the allocation, got %d items (other is %s)", page.Total, other.ID)
	}
	if page.Items[0].Allocations[0].BudgetName != "Uni" {
		t.Errorf("the allocation should carry its budget's name, got %+v", page.Items[0].Allocations)
	}
}

// The form asks which budgets the viewer may allocate from: those above the
// project's own that they manage, nearest first — nothing for the managers in
// between or the owner.
func TestAllocation_Sources(t *testing.T) {
	f := newAllocationFixture(t)
	names := func(tokens common.TokenList) []string {
		t.Helper()
		nodes, err := f.svc.AllocationSources(f.project.ID, tokens)
		if err != nil {
			t.Fatalf("sources: %v", err)
		}
		var out []string
		for _, n := range nodes {
			out = append(out, n.Name)
		}
		return out
	}
	if got := names(uniTokens); len(got) != 1 || got[0] != "Uni" {
		t.Errorf("uni manager: got %v, want [Uni]", got)
	}
	if got := names(rootTokens); len(got) != 2 || got[0] != "Uni" {
		t.Errorf("root: got %v, want [Uni root…] nearest first", got)
	}
	if got := names(studMgr); len(got) != 0 {
		t.Errorf("a manager in between: got %v, want none", got)
	}
	if got := names(ownerToken); len(got) != 0 {
		t.Errorf("the owner: got %v, want none", got)
	}
	if _, err := f.svc.AllocationSources(f.project.ID, common.TokenList{"user:stranger@x"}); !errors.Is(err, common.ErrForbidden) {
		t.Errorf("a stranger must be refused, got %v", err)
	}
}
