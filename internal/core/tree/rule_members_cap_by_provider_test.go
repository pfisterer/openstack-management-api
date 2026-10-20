package tree_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/nodestore"
	"github.com/pfisterer/openstack-management-api/internal/core/tree"
	"github.com/pfisterer/openstack-management-api/internal/dev"
)

// os22: the ceiling on a project's members protects OpenStack, where every member costs the reconciler Keystone work on every run. A project without anything in OpenStack (workspaces, LLM) has a ceiling of its own.

var memberCapResources = []common.Resource{
	{ID: "cores", Name: "Cores", Provider: common.ProviderOpenStack},
	{ID: "ws-storage-gb", Name: "Workspace storage", Provider: "aiaas", Area: common.AreaWorkspaces},
}

const (
	capOpenStack = 5
	capOther     = 12
)

type memberCapFixture struct {
	svc    *tree.Service
	uni    model.Node
	budget model.Node
}

var memberCapOwner = common.TokenList{"user:student@x"}

func newMemberCapFixture(t *testing.T) memberCapFixture {
	t.Helper()
	log := zap.NewNop().Sugar()
	svc := tree.NewService(nodestore.NewInMemoryStore(log), dev.NewMockRoleProvider(), memberCapResources, common.TokenList{"group:root"}, 5*time.Second, capOpenStack, testAccounting, log)
	svc.SetMaxAuthorizedUsersOther(capOther)
	if err := svc.Bootstrap(context.Background(), nil, nil); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	rootTokens := common.TokenList{"group:root"}
	uni, err := svc.CreateNode(tree.CreateNodeRequest{ParentID: model.RootNodeID, Kind: model.KindBudget, Name: "Uni", Reason: "t",
		Limit: common.ProjectQuota{"cores": 100, "ws-storage-gb": 1000}, AdminScope: rootTokens}, model.UIActor("root@x"), "root@x", rootTokens)
	if err != nil {
		t.Fatalf("create uni: %v", err)
	}
	budget, err := svc.CreateNode(tree.CreateNodeRequest{ParentID: uni.ID, Kind: model.KindBudget, Name: "Course", Reason: "t",
		Limit: common.ProjectQuota{"cores": 10, "ws-storage-gb": 500}, AdminScope: common.TokenList{"group:course-admins"}, EligibleRequesters: memberCapOwner},
		model.UIActor("root@x"), "root@x", rootTokens)
	if err != nil {
		t.Fatalf("create budget: %v", err)
	}
	return memberCapFixture{svc: svc, uni: uni, budget: budget}
}

func members(n int) []common.AuthorizedUser {
	out := make([]common.AuthorizedUser, n)
	for i := range out {
		out[i] = common.AuthorizedUser{Token: fmt.Sprintf("user:s%d@x.example", i), OpenstackRole: "member"}
	}
	return out
}

func (f memberCapFixture) create(limit common.ProjectQuota, n int) (model.Node, error) {
	return f.svc.CreateNode(tree.CreateNodeRequest{ParentID: f.budget.ID, Kind: model.KindProject, Name: "p", Reason: "t", Limit: limit, AuthorizedUsers: members(n)},
		model.UIActor("student@x"), "student@x", memberCapOwner)
}

func wantCapError(t *testing.T, err error, limit int, openstack bool) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected the ceiling of %d to apply, request was accepted", limit)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("at most %d authorized users", limit)) {
		t.Errorf("error does not name the ceiling %d: %v", limit, err)
	}
	if strings.Contains(err.Error(), "OpenStack") != openstack {
		t.Errorf("error should say whether it is the OpenStack ceiling (%v): %v", openstack, err)
	}
}

func TestMemberCap_WorkspaceProjectsHaveTheirOwnCeiling(t *testing.T) {
	f := newMemberCapFixture(t)
	ws := common.ProjectQuota{"ws-storage-gb": 50}

	if _, err := f.create(ws, capOther); err != nil {
		t.Fatalf("a workspace project with %d members (above the OpenStack ceiling) was refused: %v", capOther, err)
	}
	_, err := f.create(ws, capOther+1)
	wantCapError(t, err, capOther, false)
}

func TestMemberCap_OpenStackProjectsKeepTheirCeiling(t *testing.T) {
	f := newMemberCapFixture(t)
	if _, err := f.create(common.ProjectQuota{"cores": 2}, capOpenStack); err != nil {
		t.Fatalf("at the ceiling: %v", err)
	}
	_, err := f.create(common.ProjectQuota{"cores": 2}, capOpenStack+1)
	wantCapError(t, err, capOpenStack, true)
	// Both at once is an OpenStack project.
	_, err = f.create(common.ProjectQuota{"cores": 2, "ws-storage-gb": 5}, capOpenStack+1)
	wantCapError(t, err, capOpenStack, true)
	// A quantity of 0 brings nothing of OpenStack in.
	if _, err := f.create(common.ProjectQuota{"cores": 0, "ws-storage-gb": 5}, capOpenStack+1); err != nil {
		t.Errorf("cores 0 counted as OpenStack: %v", err)
	}
}

func TestMemberCap_ChangeRequests(t *testing.T) {
	f := newMemberCapFixture(t)
	p, err := f.create(common.ProjectQuota{"ws-storage-gb": 50}, capOpenStack+2)
	if err != nil {
		t.Fatal(err)
	}
	change := func(req tree.ChangeNodeRequest) error {
		_, err := f.svc.RequestChange(p.ID, req, model.UIActor("student@x"), memberCapOwner)
		return err
	}

	// Editing the members of a workspace project: its own ceiling.
	more := members(capOther)
	if err := change(tree.ChangeNodeRequest{AuthorizedUsers: &more}); err != nil {
		t.Fatalf("editing members of a workspace project: %v", err)
	}
	tooMany := members(capOther + 1)
	wantCapError(t, change(tree.ChangeNodeRequest{AuthorizedUsers: &tooMany}), capOther, false)

	// Asking for OpenStack resources brings the OpenStack ceiling — also for the members already there.
	withCores := common.ProjectQuota{"cores": 1, "ws-storage-gb": 50}
	wantCapError(t, change(tree.ChangeNodeRequest{Limit: &withCores}), capOpenStack, true)
	few := members(capOpenStack)
	if err := change(tree.ChangeNodeRequest{Limit: &withCores, AuthorizedUsers: &few}); err != nil {
		t.Fatalf("OpenStack resources with members within the ceiling: %v", err)
	}
	// Now an OpenStack project: more members than its ceiling are refused.
	more = members(capOpenStack + 1)
	wantCapError(t, change(tree.ChangeNodeRequest{AuthorizedUsers: &more}), capOpenStack, true)
}

// A change that has nothing to do with members or OpenStack does not trip over a list that is longer than today's ceiling.
func TestMemberCap_UnrelatedChangesPass(t *testing.T) {
	f := newMemberCapFixture(t)
	p, err := f.create(common.ProjectQuota{"ws-storage-gb": 50}, capOther)
	if err != nil {
		t.Fatal(err)
	}
	more := common.ProjectQuota{"ws-storage-gb": 60}
	if _, err := f.svc.RequestChange(p.ID, tree.ChangeNodeRequest{Limit: &more}, model.UIActor("student@x"), memberCapOwner); err != nil {
		t.Errorf("a workspace change with %d members was refused: %v", capOther, err)
	}
}

// OpenStack resources drawn from a budget further up make the project OpenStack's business too.
func TestMemberCap_AllocationBringingOpenStack(t *testing.T) {
	f := newMemberCapFixture(t)
	p, err := f.create(common.ProjectQuota{"ws-storage-gb": 50}, capOpenStack+1)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.SetAllocation(p.ID, tree.AllocationRequest{BudgetID: f.uni.ID, Limit: common.ProjectQuota{"cores": 2}, Reason: "lab"}, model.UIActor("root@x"), common.TokenList{"group:root"})
	wantCapError(t, err, capOpenStack, true)
	if _, err := f.svc.SetAllocation(p.ID, tree.AllocationRequest{BudgetID: f.uni.ID, Limit: common.ProjectQuota{"ws-storage-gb": 10}, Reason: "more space"}, model.UIActor("root@x"), common.TokenList{"group:root"}); err != nil {
		t.Errorf("a workspace allocation was refused: %v", err)
	}
}
