package tree_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/tree"
)

func rmax(cpuIdle, cpuMax, gpuIdle, gpuMax *int) *model.ReleaseMax {
	return &model.ReleaseMax{CPU: model.ReleaseTimes{IdleOpenMinutes: cpuIdle, MaxHours: cpuMax}, GPU: model.ReleaseTimes{IdleOpenMinutes: gpuIdle, MaxHours: gpuMax}}
}

// The root holds release maximums like any budget; the budgets below inherit them, may tighten them and may not loosen them, and reads show what is inherited.
func TestReleaseMax_RootAndInheritance(t *testing.T) {
	f := newChangeFixture(t, nil, nil)
	root, err := f.svc.UpdateNode(model.RootNodeID, tree.UpdateNodeRequest{ReleaseMax: rmax(days(120), days(12), days(30), days(4))}, rootActor(), rootTokens)
	if err != nil {
		t.Fatalf("root admins set the root's maximums: %v", err)
	}
	if root.ReleaseMax == nil || *root.ReleaseMax.GPU.MaxHours != 4 {
		t.Fatalf("root release_max = %+v", root.ReleaseMax)
	}
	last := root.History[len(root.History)-1]
	if last.ReleaseMaxFrom != nil || last.ReleaseMaxTo == nil || *last.ReleaseMaxTo.CPU.IdleOpenMinutes != 120 {
		t.Errorf("the history should record the change, got %+v", last)
	}

	b := mustGet(t, f.svc, f.budget.ID)
	if b.InheritedReleaseMax == nil || *b.InheritedReleaseMax.CPU.MaxHours != 12 || b.ReleaseMax != nil {
		t.Fatalf("the budget should inherit the root's maximums, got own %+v inherited %+v", b.ReleaseMax, b.InheritedReleaseMax)
	}
	if _, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{ReleaseMax: rmax(nil, days(24), nil, nil)}, rootActor(), rootTokens); err == nil || !strings.Contains(err.Error(), "at most 12 hours") {
		t.Errorf("loosening below the root should be refused, got %v", err)
	}
	if _, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{ReleaseMax: rmax(nil, nil, nil, days(0))}, rootActor(), rootTokens); err == nil {
		t.Error("no upper bound under a root with one should be refused")
	}
	if _, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{ReleaseMax: rmax(days(60), nil, days(15), days(2))}, rootActor(), rootTokens); err != nil {
		t.Fatalf("tightening should be allowed: %v", err)
	}

	sub := mustCreate(t, f.svc, tree.CreateNodeRequest{ParentID: f.budget.ID, Kind: model.KindBudget, Limit: cores(1)})
	if sub.ReleaseMax != nil {
		t.Errorf("nothing is copied down, got %+v", sub.ReleaseMax)
	}
	got := mustGet(t, f.svc, sub.ID).InheritedReleaseMax
	if got == nil || *got.CPU.IdleOpenMinutes != 60 || *got.CPU.MaxHours != 12 || *got.GPU.IdleOpenMinutes != 15 || *got.GPU.MaxHours != 2 {
		t.Errorf("inherited = %+v, want the strictest of root and budget", got)
	}
	if _, err := f.svc.CreateNode(tree.CreateNodeRequest{ParentID: f.budget.ID, Kind: model.KindBudget, Name: "n", Reason: "t", Limit: cores(1),
		AdminScope: common.TokenList{"group:root"}, ReleaseMax: rmax(days(90), nil, nil, nil)}, rootActor(), "root@x", rootTokens); err == nil {
		t.Error("creating a sub-budget with a longer idle time should be refused")
	}

	// An empty object removes the budget's own maximums.
	cleared, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{ReleaseMax: &model.ReleaseMax{}}, rootActor(), rootTokens)
	if err != nil || cleared.ReleaseMax != nil {
		t.Errorf("clearing: %v, %+v", err, cleared.ReleaseMax)
	}
}

// Who may set them: the budget's managers and those above, like the other policy fields; not those who may only request, and not on a project.
func TestReleaseMax_Authz(t *testing.T) {
	f := newChangeFixture(t, nil, nil)
	if _, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{ReleaseMax: rmax(days(10), nil, nil, nil)}, model.UIActor("stud@x"), studTokens); !errors.Is(err, common.ErrForbidden) {
		t.Errorf("a requester should not set them, got %v", err)
	}
	if _, err := f.svc.UpdateNode(model.RootNodeID, tree.UpdateNodeRequest{ReleaseMax: rmax(days(10), nil, nil, nil)}, model.UIActor("stud@x"), studTokens); !errors.Is(err, common.ErrForbidden) {
		t.Errorf("only root admins set the root's, got %v", err)
	}
	mgr := common.TokenList{"user:mgr@x"}
	own := mustCreate(t, f.svc, tree.CreateNodeRequest{ParentID: f.budget.ID, Kind: model.KindBudget, Limit: cores(1), AdminScope: mgr})
	if _, err := f.svc.UpdateNode(own.ID, tree.UpdateNodeRequest{ReleaseMax: rmax(days(10), nil, nil, nil)}, model.UIActor("mgr@x"), mgr); err != nil {
		t.Errorf("a budget's own managers set them: %v", err)
	}
	p := f.approved(t, 1, nil)
	if _, err := f.svc.UpdateNode(p.ID, tree.UpdateNodeRequest{ReleaseMax: rmax(days(10), nil, nil, nil)}, rootActor(), rootTokens); err == nil {
		t.Error("release_max on a project should be refused")
	}
	if _, err := f.svc.CreateNode(tree.CreateNodeRequest{ParentID: f.budget.ID, Kind: model.KindProject, Name: "p", Reason: "t", Limit: cores(1), ReleaseMax: rmax(days(10), nil, nil, nil)}, rootActor(), "root@x", rootTokens); err == nil {
		t.Error("release_max on a new project should be refused")
	}
	if got := mustGet(t, f.svc, p.ID).InheritedReleaseMax; got != nil {
		t.Errorf("nothing set above the project, got %+v", got)
	}
}
