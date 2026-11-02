package tree_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/tree"
)

// Batch jobs are there or not, like a resource: the root sets the most, a budget below may switch them on only where the budget above has them and with values at most those; one without them has none, and neither have the projects below; clearing switches them off below.
func TestBatchJobs_RootAndInheritance(t *testing.T) {
	f := newChangeFixture(t, nil, nil)
	jobs := func(n, h int) *model.BatchJobs { return &model.BatchJobs{JobsPerPerson: n, MaxHours: h} }

	// Nothing set anywhere: none, and a budget cannot switch them on.
	if got := mustGet(t, f.svc, f.budget.ID).InheritedBatchJobs; got != nil {
		t.Fatalf("without the root's: inherited %+v, want none", got)
	}
	if _, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{BatchJobs: jobs(1, 1)}, rootActor(), rootTokens); err == nil || !strings.Contains(err.Error(), "has none") {
		t.Errorf("switching on below a root without them should be refused, got %v", err)
	}

	// The root sets the most: any number of jobs, at most MaxJobHours.
	if _, err := f.svc.UpdateNode(model.RootNodeID, tree.UpdateNodeRequest{BatchJobs: jobs(4, model.MaxJobHours+1)}, rootActor(), rootTokens); err == nil {
		t.Error("more than MaxJobHours should be refused at the root too")
	}
	root, err := f.svc.UpdateNode(model.RootNodeID, tree.UpdateNodeRequest{BatchJobs: jobs(40, 12)}, rootActor(), rootTokens)
	if err != nil {
		t.Fatalf("root admins set the root's value: %v", err)
	}
	if last := root.History[len(root.History)-1]; last.BatchJobsFrom != nil || last.BatchJobsTo == nil || *last.BatchJobsTo != *jobs(40, 12) {
		t.Errorf("the history should record the change, got %+v", last)
	}

	b := mustGet(t, f.svc, f.budget.ID)
	if b.InheritedBatchJobs == nil || *b.InheritedBatchJobs != *jobs(40, 12) || b.BatchJobs != nil {
		t.Fatalf("the budget sees the root's as its bound, got own %+v inherited %+v", b.BatchJobs, b.InheritedBatchJobs)
	}
	// The budget has none of its own, so the projects below have none either.
	p := f.approved(t, 1, nil)
	if got := mustGet(t, f.svc, p.ID).InheritedBatchJobs; got != nil {
		t.Errorf("a project below a budget without them: %+v, want none", got)
	}
	for name, bad := range map[string]*model.BatchJobs{"more jobs": jobs(41, 4), "longer": jobs(2, 24), "no run time": jobs(2, 0), "no jobs": jobs(0, 4)} {
		if _, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{BatchJobs: bad}, rootActor(), rootTokens); err == nil {
			t.Errorf("%s should be refused", name)
		}
	}
	if _, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{BatchJobs: jobs(2, 4)}, rootActor(), rootTokens); err != nil {
		t.Fatalf("switching on within the root's should be allowed: %v", err)
	}
	if got := mustGet(t, f.svc, p.ID).InheritedBatchJobs; got == nil || *got != *jobs(2, 4) {
		t.Errorf("the project gets the budget's: %+v", got)
	}

	sub := mustCreate(t, f.svc, tree.CreateNodeRequest{ParentID: f.budget.ID, Kind: model.KindBudget, Limit: cores(1)})
	if sub.BatchJobs != nil {
		t.Errorf("nothing is copied down, got %+v", *sub.BatchJobs)
	}
	if _, err := f.svc.UpdateNode(sub.ID, tree.UpdateNodeRequest{BatchJobs: jobs(2, 8)}, rootActor(), rootTokens); err == nil || !strings.Contains(err.Error(), "at most 4 hours") {
		t.Errorf("a longer run time below should be refused, got %v", err)
	}
	if _, err := f.svc.CreateNode(tree.CreateNodeRequest{ParentID: f.budget.ID, Kind: model.KindBudget, Name: "n", Reason: "t", Limit: cores(1),
		AdminScope: common.TokenList{"group:root"}, BatchJobs: jobs(3, 4)}, rootActor(), "root@x", rootTokens); err == nil {
		t.Error("creating a sub-budget with more jobs than above should be refused")
	}

	// Lowering above holds below at once, without carrying anything down.
	if _, err := f.svc.UpdateNode(model.RootNodeID, tree.UpdateNodeRequest{BatchJobs: jobs(1, 12)}, rootActor(), rootTokens); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, f.svc, p.ID).InheritedBatchJobs; got == nil || *got != *jobs(1, 4) {
		t.Errorf("after lowering the root: %+v, want 1 job and 4 hours", got)
	}

	// Clearing switches them off below.
	cleared, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{ClearBatchJobs: true}, rootActor(), rootTokens)
	if err != nil || cleared.BatchJobs != nil {
		t.Fatalf("clearing: %v, %+v", err, cleared.BatchJobs)
	}
	if got := mustGet(t, f.svc, p.ID).InheritedBatchJobs; got != nil {
		t.Errorf("after clearing: %+v, want none", got)
	}
}

// A budget that inherits its limit passes on the batch jobs above and sets none of its own; switched on it drops its own, switched off it keeps what it passed on.
func TestBatchJobs_InheritingBudget(t *testing.T) {
	f := newChangeFixture(t, nil, nil)
	jobs := func(n, h int) *model.BatchJobs { return &model.BatchJobs{JobsPerPerson: n, MaxHours: h} }
	if _, err := f.svc.UpdateNode(model.RootNodeID, tree.UpdateNodeRequest{BatchJobs: jobs(4, 12)}, rootActor(), rootTokens); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{BatchJobs: jobs(3, 8)}, rootActor(), rootTokens); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CreateNode(tree.CreateNodeRequest{ParentID: f.budget.ID, Kind: model.KindBudget, Name: "s", Reason: "t", InheritsLimit: true,
		AdminScope: common.TokenList{"group:root"}, BatchJobs: jobs(1, 1)}, rootActor(), "root@x", rootTokens); err == nil {
		t.Error("an inheriting budget with batch jobs of its own should be refused")
	}
	structure := mustCreate(t, f.svc, tree.CreateNodeRequest{ParentID: f.budget.ID, Kind: model.KindBudget, InheritsLimit: true})
	course := mustCreate(t, f.svc, tree.CreateNodeRequest{ParentID: structure.ID, Kind: model.KindBudget, Limit: cores(1)})
	if got := mustGet(t, f.svc, course.ID).InheritedBatchJobs; got == nil || *got != *jobs(3, 8) {
		t.Fatalf("below an inheriting budget: %+v, want the budget's above it", got)
	}
	if _, err := f.svc.UpdateNode(course.ID, tree.UpdateNodeRequest{BatchJobs: jobs(3, 8)}, rootActor(), rootTokens); err != nil {
		t.Errorf("switching on below an inheriting budget should be allowed: %v", err)
	}
	if _, err := f.svc.UpdateNode(structure.ID, tree.UpdateNodeRequest{BatchJobs: jobs(1, 1)}, rootActor(), rootTokens); err == nil {
		t.Error("batch jobs of its own on an inheriting budget should be refused")
	}

	// Switched off, it keeps what it passed on.
	off, err := f.svc.UpdateNode(structure.ID, tree.UpdateNodeRequest{InheritsLimit: new(false)}, rootActor(), rootTokens)
	if err != nil {
		t.Fatal(err)
	}
	if off.BatchJobs == nil || *off.BatchJobs != *jobs(3, 8) {
		t.Errorf("switched off inheriting: own %+v, want what it passed on", off.BatchJobs)
	}
	// Switched on again, it drops its own.
	again, err := f.svc.UpdateNode(structure.ID, tree.UpdateNodeRequest{InheritsLimit: new(true)}, rootActor(), rootTokens)
	if err != nil {
		t.Fatal(err)
	}
	if again.BatchJobs != nil {
		t.Errorf("switched on inheriting: own %+v, want none", again.BatchJobs)
	}
}

// Who may set them: the budget's managers and those above, like the other policy fields; not those who may only request, and not on a project.
func TestBatchJobs_Authz(t *testing.T) {
	f := newChangeFixture(t, nil, nil)
	jobs := &model.BatchJobs{JobsPerPerson: 1, MaxHours: 2}
	if _, err := f.svc.UpdateNode(model.RootNodeID, tree.UpdateNodeRequest{BatchJobs: &model.BatchJobs{JobsPerPerson: 4, MaxHours: 24}}, rootActor(), rootTokens); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{BatchJobs: jobs}, model.UIActor("stud@x"), studTokens); !errors.Is(err, common.ErrForbidden) {
		t.Errorf("a requester should not set them, got %v", err)
	}
	if _, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{ClearBatchJobs: true}, model.UIActor("stud@x"), studTokens); !errors.Is(err, common.ErrForbidden) {
		t.Errorf("a requester should not clear them, got %v", err)
	}
	if _, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{BatchJobs: jobs}, rootActor(), rootTokens); err != nil {
		t.Fatal(err)
	}
	mgr := common.TokenList{"user:mgr@x"}
	own := mustCreate(t, f.svc, tree.CreateNodeRequest{ParentID: f.budget.ID, Kind: model.KindBudget, Limit: cores(1), AdminScope: mgr})
	if _, err := f.svc.UpdateNode(own.ID, tree.UpdateNodeRequest{BatchJobs: jobs}, model.UIActor("mgr@x"), mgr); err != nil {
		t.Errorf("a budget's own managers set them: %v", err)
	}
	p := f.approved(t, 1, nil)
	if _, err := f.svc.UpdateNode(p.ID, tree.UpdateNodeRequest{BatchJobs: jobs}, rootActor(), rootTokens); err == nil {
		t.Error("batch_jobs on a project should be refused")
	}
	if _, err := f.svc.CreateNode(tree.CreateNodeRequest{ParentID: f.budget.ID, Kind: model.KindProject, Name: "p", Reason: "t", Limit: cores(1), BatchJobs: jobs}, rootActor(), "root@x", rootTokens); err == nil || !strings.Contains(err.Error(), "batch_jobs is a budget setting") {
		t.Errorf("batch_jobs on a new project should be refused, got %v", err)
	}
}
