package tree_test

import (
	"errors"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/tree"
)

// A project's admin scope names the people who administer it with the owner.
// They act as the owner does — rename, request changes, edit the list, release
// — but they are no manager of it: approving its requests and handing it to
// someone else stay with the budgets above.
func TestProjectAdmins(t *testing.T) {
	svc, _ := newSvc(t, common.TokenList{"group:root"})
	rootTokens := common.TokenList{"user:root@x", "group:root"}
	ownerTokens := common.TokenList{"user:owner@x"}
	coTokens := common.TokenList{"user:co@x"}
	chairTokens := common.TokenList{"user:member@x", "group:chair"}
	strangerTokens := common.TokenList{"user:stranger@x"}

	budget, err := svc.CreateNode(tree.CreateNodeRequest{
		ParentID: tree.RootNodeID, Kind: tree.KindBudget, Name: "Chair", Reason: "test",
		Limit: cores(10), AdminScope: common.TokenList{"group:root"},
		EligibleRequesters: common.TokenList{"user:owner@x"},
	}, tree.UIActor("root@x"), "root@x", rootTokens)
	if err != nil {
		t.Fatalf("create budget: %v", err)
	}
	project, err := svc.CreateNode(tree.CreateNodeRequest{
		ParentID: budget.ID, Kind: tree.KindProject, Name: "Lab", Reason: "lab", Limit: cores(2),
		AdminScope: common.TokenList{"user:co@x", "group:chair"},
	}, tree.UIActor("owner@x"), "owner@x", ownerTokens)
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := svc.ApproveNode(project.ID, tree.ApproveNodeRequest{}, tree.UIActor("root@x"), rootTokens); err != nil {
		t.Fatalf("approve: %v", err)
	}

	// "My Projects" finds it for the owner, an admin by name and one by group.
	for who, tokens := range map[string]common.TokenList{"owner@x": ownerTokens, "co@x": coTokens, "member@x": chairTokens} {
		mine, err := svc.ListMine(who, tokens, 0, 0)
		if err != nil {
			t.Fatalf("list mine for %s: %v", who, err)
		}
		if mine.Total != 1 || mine.Items[0].ID != project.ID {
			t.Errorf("%s: expected the project in My Projects, got %d", who, mine.Total)
		}
	}
	if mine, _ := svc.ListMine("stranger@x", strangerTokens, 0, 0); mine.Total != 0 {
		t.Errorf("a stranger must not see the project, got %d", mine.Total)
	}

	// Rename and the admin list are direct edits: no approval, status unchanged.
	name := "Lab 2"
	admins := common.TokenList{"user:co@x", "user:new@x"}
	updated, err := svc.UpdateNode(project.ID, tree.UpdateNodeRequest{Name: &name, AdminScope: &admins}, tree.UIActor("co@x"), coTokens)
	if err != nil {
		t.Fatalf("admin edits name and admins: %v", err)
	}
	if updated.Status != tree.StatusApproved || updated.Name != name || len(updated.AdminScope) != 2 {
		t.Errorf("unexpected result: status %s, name %q, admins %v", updated.Status, updated.Name, updated.AdminScope)
	}
	if _, err := svc.UpdateNode(project.ID, tree.UpdateNodeRequest{Name: &name}, tree.UIActor("stranger@x"), strangerTokens); !errors.Is(err, common.ErrForbidden) {
		t.Errorf("a stranger must not rename, got %v", err)
	}

	// Resources still go through a request, which an admin may file but not decide.
	more := cores(3)
	if _, err := svc.UpdateNode(project.ID, tree.UpdateNodeRequest{Limit: &more}, tree.UIActor("co@x"), coTokens); err == nil {
		t.Error("a limit must not be a direct edit on a project")
	}
	if _, err := svc.RequestChange(project.ID, tree.ChangeNodeRequest{Limit: &more}, tree.UIActor("co@x"), coTokens); err != nil {
		t.Fatalf("admin requests a change: %v", err)
	}
	if _, err := svc.ApproveNode(project.ID, tree.ApproveNodeRequest{}, tree.UIActor("co@x"), coTokens); !errors.Is(err, common.ErrForbidden) {
		t.Errorf("an admin must not approve the project's own request, got %v", err)
	}
	if _, err := svc.ApproveNode(project.ID, tree.ApproveNodeRequest{}, tree.UIActor("root@x"), rootTokens); err != nil {
		t.Fatalf("approve change: %v", err)
	}

	if _, err := svc.TransferOwner(project.ID, tree.TransferOwnerRequest{NewOwner: "co@x"}, tree.UIActor("co@x"), coTokens); !errors.Is(err, common.ErrForbidden) {
		t.Errorf("an admin must not transfer the project, got %v", err)
	}

	if _, err := svc.ReleaseNode(project.ID, tree.UIActor("stranger@x"), strangerTokens); !errors.Is(err, common.ErrForbidden) {
		t.Errorf("a stranger must not release, got %v", err)
	}
	released, err := svc.ReleaseNode(project.ID, tree.UIActor("co@x"), coTokens)
	if err != nil {
		t.Fatalf("admin releases: %v", err)
	}
	if released.Status != tree.StatusReleased {
		t.Errorf("expected released, got %s", released.Status)
	}
}
