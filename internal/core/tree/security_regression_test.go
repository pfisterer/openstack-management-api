package tree_test

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/nodestore"
	"github.com/pfisterer/openstack-management-api/internal/core/tree"
	"github.com/pfisterer/openstack-management-api/internal/dev"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// A request for math.MaxInt cores used to wrap the summed usage of a budget negative, so the capacity check passed and an auto-approving budget granted it: unlimited cores for a student. Every count of every limit is bounded now, for new requests, change requests and auto-approve policies alike.
func TestOverflow_HugeLimitsAreRefused(t *testing.T) {
	for _, policy := range map[string]*model.AutoApprove{"pool": {}, "per requester": {PerRequesterLimit: cores(4)}} {
		f := newChangeFixture(t, policy, nil)
		huge := cores(math.MaxInt)
		if n, err := f.svc.CreateNode(tree.CreateNodeRequest{
			ParentID: f.budget.ID, Kind: model.KindProject, Name: "vm", Reason: "vm", Limit: huge,
		}, model.UIActor("stud@x"), "stud@x", studTokens); err == nil {
			t.Fatalf("a request for %d cores was accepted with status %q", math.MaxInt, n.Status)
		}
		// Two requests that each fit the bound but would overflow together are not possible either: the bound keeps every sum far from the end of the range.
		if _, err := f.svc.CreateNode(tree.CreateNodeRequest{
			ParentID: f.budget.ID, Kind: model.KindProject, Name: "vm", Reason: "vm", Limit: cores(common.MaxQuotaValue + 1),
		}, model.UIActor("stud@x"), "stud@x", studTokens); err == nil {
			t.Fatal("a request above the global bound was accepted")
		}

		p := f.approved(t, 1, nil)
		if _, err := f.svc.RequestChange(p.ID, tree.ChangeNodeRequest{Limit: quota(huge)}, model.UIActor("stud@x"), studTokens); err == nil {
			t.Fatal("a change request for MaxInt cores was accepted")
		}
		if got := f.get(t, p.ID); got.Limit["cores"] != 1 || got.Status != model.StatusApproved {
			t.Fatalf("the project changed: %v %s", got.Limit, got.Status)
		}
	}
}

// Budgets keep the unlimited sentinel but are bounded like everything else; an auto-approve policy is bounded too.
func TestOverflow_BudgetAndPolicyBounds(t *testing.T) {
	svc, _ := newSvc(t, common.TokenList{"group:root"})
	create := func(limit common.ProjectQuota, policy *model.AutoApprove) error {
		_, err := svc.CreateNode(tree.CreateNodeRequest{
			ParentID: model.RootNodeID, Kind: model.KindBudget, Name: "b", Reason: "r", Limit: limit,
			AdminScope: common.TokenList{"group:root"}, AutoApprove: policy,
		}, rootActor(), "root@x", rootTokens)
		return err
	}
	if err := create(cores(common.UnlimitedQuota), nil); err != nil {
		t.Fatalf("an unlimited budget: %v", err)
	}
	if err := create(cores(math.MaxInt), nil); err == nil {
		t.Fatal("a budget of MaxInt cores was accepted")
	}
	if err := create(cores(10), &model.AutoApprove{PerRequesterLimit: cores(math.MaxInt)}); err == nil {
		t.Fatal("a per-requester limit of MaxInt cores was accepted")
	}
}

// Where the catalogue names a maximum for a resource, a project is held to it; a budget, which holds many projects, is not.
func TestOverflow_CatalogueMaximumBindsProjects(t *testing.T) {
	log := zap.NewNop().Sugar()
	resources := []common.Resource{{ID: "cores", Name: "Cores"}, {ID: "ws-gpus", Name: "GPUs", Max: 64, Provider: "aiaas", Area: common.AreaWorkspaces}}
	svc := tree.NewService(nodestore.NewInMemoryStore(log), dev.NewMockRoleProvider(), resources, common.TokenList{"group:root"}, 5*time.Second, common.DefaultMaxAuthorizedUsers, testAccounting, log)
	if err := svc.Bootstrap(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	budget, err := svc.CreateNode(tree.CreateNodeRequest{
		ParentID: model.RootNodeID, Kind: model.KindBudget, Name: "b", Reason: "r",
		Limit: common.ProjectQuota{"cores": 10, "ws-gpus": 1000}, AdminScope: common.TokenList{"group:root"},
	}, rootActor(), "root@x", rootTokens)
	if err != nil {
		t.Fatalf("a budget above the per-project maximum: %v", err)
	}
	_, err = svc.CreateNode(tree.CreateNodeRequest{
		ParentID: budget.ID, Kind: model.KindProject, Name: "p", Reason: "r", Limit: common.ProjectQuota{"ws-gpus": 65},
	}, rootActor(), "root@x", rootTokens)
	if err == nil || !strings.Contains(err.Error(), "must not exceed 64") {
		t.Fatalf("a project above the catalogue's maximum: %v", err)
	}
}

// Every person may hold only so many live projects in one budget through requests; managers are not counted against it.
func TestProjectsPerOwner_Capped(t *testing.T) {
	f := newChangeFixture(t, &model.AutoApprove{}, nil)
	f.svc.SetMaxProjectsPerOwner(3)
	for range 3 {
		f.request(t, 1, nil)
	}
	_, err := f.svc.CreateNode(tree.CreateNodeRequest{
		ParentID: f.budget.ID, Kind: model.KindProject, Name: "vm", Reason: "vm", Limit: cores(1),
	}, model.UIActor("stud@x"), "stud@x", studTokens)
	if !errors.Is(err, common.ErrForbidden) || !strings.Contains(err.Error(), "at most 3") {
		t.Fatalf("a fourth project was granted: %v", err)
	}
	if _, err := f.svc.CreateNode(tree.CreateNodeRequest{
		ParentID: f.budget.ID, Kind: model.KindProject, Name: "vm", Reason: "vm", Limit: cores(1),
	}, rootActor(), "root@x", rootTokens); err != nil {
		t.Fatalf("a manager creating directly is not counted: %v", err)
	}
}

// A request for nothing is not auto-approved: it would still be an OpenStack project, created on demand without anybody looking.
func TestAutoApprove_NotForNothing(t *testing.T) {
	f := newChangeFixture(t, &model.AutoApprove{}, nil)
	for _, limit := range []common.ProjectQuota{{}, cores(0)} {
		n, err := f.svc.CreateNode(tree.CreateNodeRequest{
			ParentID: f.budget.ID, Kind: model.KindProject, Name: "empty", Reason: "r", Limit: limit,
		}, model.UIActor("stud@x"), "stud@x", studTokens)
		if err != nil {
			t.Fatalf("an empty request: %v", err)
		}
		if n.Status != model.StatusPending {
			t.Fatalf("an empty request was %s, want pending", n.Status)
		}
	}
}

// An approval names the revision the approver saw; a request amended since is not approved unseen.
func TestApprove_RefusesARequestThatChanged(t *testing.T) {
	f := newChangeFixture(t, nil, nil)
	seen := f.request(t, 1, nil)
	amended, err := f.svc.RequestChange(seen.ID, tree.ChangeNodeRequest{Limit: quota(cores(9))}, model.UIActor("stud@x"), studTokens)
	if err != nil {
		t.Fatalf("amend: %v", err)
	}
	if amended.CurrentRevision() == seen.CurrentRevision() {
		t.Fatalf("an amendment did not change the revision (%d)", seen.CurrentRevision())
	}
	_, err = f.svc.ApproveNode(seen.ID, tree.ApproveNodeRequest{ExpectedRevision: new(seen.CurrentRevision())}, rootActor(), rootTokens)
	if !errors.Is(err, common.ErrConflict) {
		t.Fatalf("approving what the approver had not seen: %v", err)
	}
	if got := f.get(t, seen.ID); got.Status != model.StatusPending {
		t.Fatalf("the request is %s", got.Status)
	}
	if _, err := f.svc.ApproveNode(seen.ID, tree.ApproveNodeRequest{ExpectedRevision: new(amended.CurrentRevision())}, rootActor(), rootTokens); err != nil {
		t.Fatalf("approving the request as it is: %v", err)
	}
}

// Approving and rejecting the same request at once: exactly one of them decides it. Rejecting used to run outside the lock and write a stale copy, so both could succeed and the second overwrote the first.
func TestApproveAndReject_DecideOnce(t *testing.T) {
	f := newChangeFixture(t, nil, nil)
	f.svc.SetMaxProjectsPerOwner(0)
	for range 30 {
		p := f.request(t, 1, nil)
		var wg sync.WaitGroup
		var approveErr, rejectErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, approveErr = f.svc.ApproveNode(p.ID, tree.ApproveNodeRequest{ExpectedRevision: new(p.CurrentRevision())}, rootActor(), rootTokens)
		}()
		go func() {
			defer wg.Done()
			_, rejectErr = f.svc.RejectNode(p.ID, tree.RejectNodeRequest{}, rootActor(), rootTokens)
		}()
		wg.Wait()
		if (approveErr == nil) == (rejectErr == nil) {
			t.Fatalf("approve %v, reject %v: exactly one must decide", approveErr, rejectErr)
		}
		// Approved ones fill the pool of 10 cores; give them back.
		if approveErr == nil {
			if _, err := f.svc.ReleaseNode(p.ID, tree.ReleaseNodeRequest{}, model.UIActor("stud@x"), studTokens); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// Deleting a budget while a project is created in it: either the project is refused or the deletion is, never both done with the project lost.
func TestDeleteBudget_DoesNotRaceCreate(t *testing.T) {
	svc, store := newSvc(t, common.TokenList{"group:root"})
	for range 30 {
		b, err := svc.CreateNode(tree.CreateNodeRequest{
			ParentID: model.RootNodeID, Kind: model.KindBudget, Name: "b", Reason: "r", Limit: cores(10), AdminScope: common.TokenList{"group:root"},
		}, rootActor(), "root@x", rootTokens)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var created model.Node
		var createErr, deleteErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			created, createErr = svc.CreateNode(tree.CreateNodeRequest{ParentID: b.ID, Kind: model.KindProject, Name: "p", Reason: "r", Limit: cores(1)}, rootActor(), "root@x", rootTokens)
		}()
		go func() {
			defer wg.Done()
			deleteErr = svc.DeleteNode(b.ID, rootActor(), rootTokens)
		}()
		wg.Wait()
		if createErr == nil && deleteErr == nil {
			left, _ := store.GetNode(context.Background(), created.ID)
			t.Fatalf("both the deletion and the creation succeeded; the project is %v", left)
		}
	}
}

// Who deleted a budget is recorded: nothing of the subtree remains to carry a history entry, so it is the log.
func TestDeleteBudget_RecordsTheActor(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	log := zap.New(core).Sugar()
	svc := tree.NewService(nodestore.NewInMemoryStore(log), dev.NewMockRoleProvider(), testResources, common.TokenList{"group:root"}, 5*time.Second, common.DefaultMaxAuthorizedUsers, testAccounting, log)
	if err := svc.Bootstrap(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	b, err := svc.CreateNode(tree.CreateNodeRequest{
		ParentID: model.RootNodeID, Kind: model.KindBudget, Name: "b", Reason: "r", Limit: cores(1), AdminScope: common.TokenList{"group:root"},
	}, rootActor(), "root@x", rootTokens)
	if err != nil {
		t.Fatal(err)
	}
	actor := model.Actor{Email: "person@x", ActedBy: "admin@x"}
	if err := svc.DeleteNode(b.ID, actor, rootTokens); err != nil {
		t.Fatal(err)
	}
	entries := logs.FilterMessage("budget subtree deleted").All()
	if len(entries) != 1 {
		t.Fatalf("want one audit entry, got %d", len(entries))
	}
	fields := entries[0].ContextMap()
	if fields["actor"] != "person@x" || fields["acted_by"] != "admin@x" || fields["node_id"] != b.ID {
		t.Fatalf("audit entry %v", fields)
	}
}

// Names and reasons are bounded.
func TestTextLengths_Bounded(t *testing.T) {
	f := newChangeFixture(t, nil, nil)
	long := strings.Repeat("x", common.MaxNameLength+1)
	if _, err := f.svc.CreateNode(tree.CreateNodeRequest{ParentID: f.budget.ID, Kind: model.KindProject, Name: long, Reason: "r", Limit: cores(1)}, model.UIActor("stud@x"), "stud@x", studTokens); err == nil {
		t.Fatal("an overlong name was accepted")
	}
	if _, err := f.svc.CreateNode(tree.CreateNodeRequest{ParentID: f.budget.ID, Kind: model.KindProject, Name: "n", Reason: strings.Repeat("x", common.MaxReasonLength+1), Limit: cores(1)}, model.UIActor("stud@x"), "stud@x", studTokens); err == nil {
		t.Fatal("an overlong reason was accepted")
	}
	p := f.request(t, 1, nil)
	if _, err := f.svc.UpdateNode(p.ID, tree.UpdateNodeRequest{Name: &long}, model.UIActor("stud@x"), studTokens); err == nil {
		t.Fatal("an overlong rename was accepted")
	}
	longReason := strings.Repeat("x", common.MaxReasonLength+1)
	if _, err := f.svc.RequestChange(p.ID, tree.ChangeNodeRequest{Reason: &longReason}, model.UIActor("stud@x"), studTokens); err == nil {
		t.Fatal("an overlong reason on a change was accepted")
	}
	if _, err := f.svc.RejectNode(p.ID, tree.RejectNodeRequest{Reason: &longReason}, rootActor(), rootTokens); err == nil {
		t.Fatal("an overlong rejection reason was accepted")
	}
}

// Amending a request in a loop does not grow its history without bound: the oldest entries are dropped, and the revision keeps counting.
func TestHistory_Capped(t *testing.T) {
	f := newChangeFixture(t, nil, nil)
	p := f.request(t, 1, nil)
	const amendments = common.MaxHistoryEntries + 50
	for i := range amendments {
		reason := strings.Repeat("y", i%5+1)
		if _, err := f.svc.RequestChange(p.ID, tree.ChangeNodeRequest{Reason: &reason}, model.UIActor("stud@x"), studTokens); err != nil {
			t.Fatalf("amend %d: %v", i, err)
		}
	}
	got := f.get(t, p.ID)
	if len(got.History) != common.MaxHistoryEntries {
		t.Fatalf("history holds %d entries, want %d", len(got.History), common.MaxHistoryEntries)
	}
	if want := 1 + amendments; got.CurrentRevision() != want {
		t.Fatalf("revision %d, want %d", got.CurrentRevision(), want)
	}
	if _, err := f.svc.ApproveNode(p.ID, tree.ApproveNodeRequest{ExpectedRevision: new(got.CurrentRevision())}, rootActor(), rootTokens); err != nil {
		t.Fatalf("approve after trimming: %v", err)
	}
}

// Moving a budget that inherits its limit changes the limit that applies to the budgets below it; those must fit the new one, or a sub-budget of 8 cores ends up under a parent of 4.
func TestInheritsLimit_MoveChecksTheBudgetsBelow(t *testing.T) {
	f := newChangeFixture(t, nil, nil)
	group := f.newGroup(t, f.budget.ID, "Group")
	inner := f.newGroup(t, group.ID, "Inner")
	for _, parent := range []string{group.ID, inner.ID} {
		if _, err := f.svc.CreateNode(tree.CreateNodeRequest{
			ParentID: parent, Kind: model.KindBudget, Name: "Course", Reason: "t", Limit: cores(8), AdminScope: common.TokenList{"group:root"},
		}, rootActor(), "root@x", rootTokens); err != nil {
			t.Fatalf("course under %s: %v", parent, err)
		}
	}
	small, err := f.svc.CreateNode(tree.CreateNodeRequest{
		ParentID: model.RootNodeID, Kind: model.KindBudget, Name: "Small", Reason: "t", Limit: cores(4), AdminScope: common.TokenList{"group:root"},
	}, rootActor(), "root@x", rootTokens)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ReparentNode(group.ID, tree.ReparentNodeRequest{NewParentID: small.ID}, rootActor(), rootTokens); err == nil {
		t.Fatal("moved a sub-budget of 8 cores under a budget of 4")
	}
	if _, err := f.svc.ReparentNode(inner.ID, tree.ReparentNodeRequest{NewParentID: small.ID}, rootActor(), rootTokens); err == nil {
		t.Fatal("moved a nested sub-budget of 8 cores under a budget of 4")
	}
	large, err := f.svc.CreateNode(tree.CreateNodeRequest{
		ParentID: model.RootNodeID, Kind: model.KindBudget, Name: "Large", Reason: "t", Limit: cores(16), AdminScope: common.TokenList{"group:root"},
	}, rootActor(), "root@x", rootTokens)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ReparentNode(group.ID, tree.ReparentNodeRequest{NewParentID: large.ID}, rootActor(), rootTokens); err != nil {
		t.Fatalf("a move where everything fits: %v", err)
	}
}

// A failure of the store is reported as an internal error, not as a message about the request.
func TestStoreFailure_IsInternal(t *testing.T) {
	log := zap.NewNop().Sugar()
	store := failingStore{Store: nodestore.NewInMemoryStore(log)}
	svc := tree.NewService(store, dev.NewMockRoleProvider(), testResources, common.TokenList{"group:root"}, 5*time.Second, common.DefaultMaxAuthorizedUsers, testAccounting, log)
	_, err := svc.GetNode("anything", rootTokens)
	if !errors.Is(err, common.ErrInternal) {
		t.Fatalf("a failing store gave %v", err)
	}
}

type failingStore struct{ nodestore.Store }

func (failingStore) GetNode(context.Context, string) (*model.Node, error) {
	return nil, errors.New("pq: connection refused to 10.0.0.5")
}
