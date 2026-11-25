package tree_test

import (
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/tree"
	"github.com/pfisterer/openstack-management-api/internal/dev"
)

// privacyFixture is a project with an owner, an admin, a person and a group as members and two more people, under a budget with a manager of its own.
type privacyFixture struct {
	svc                                                         *tree.Service
	budget, project                                             model.Node
	rootT, managerT, ownerT, adminT, memberT, groupT, strangerT common.TokenList
}

func newPrivacyFixture(t *testing.T) privacyFixture {
	t.Helper()
	svc, _ := newSvc(t, common.TokenList{"group:root"})
	f := privacyFixture{
		svc:       svc,
		rootT:     common.TokenList{"user:root@example.org", "group:root"},
		managerT:  common.TokenList{"user:manager@example.org", "group:chair"},
		ownerT:    common.TokenList{"user:owner@example.org"},
		adminT:    common.TokenList{"user:co@example.org"},
		memberT:   common.TokenList{"user:member@example.org"},
		groupT:    common.TokenList{"user:student@example.org", dev.CourseGroup},
		strangerT: common.TokenList{"user:stranger@example.org"},
	}
	budget, err := svc.CreateNode(tree.CreateNodeRequest{
		ParentID: model.RootNodeID, Kind: model.KindBudget, Name: "Chair", Reason: "test",
		Limit: cores(20), AdminScope: common.TokenList{"group:chair"},
		EligibleRequesters: common.TokenList{"user:owner@example.org"},
	}, model.UIActor("root@example.org"), "root@example.org", f.rootT)
	if err != nil {
		t.Fatalf("create budget: %v", err)
	}
	project, err := svc.CreateNode(tree.CreateNodeRequest{
		ParentID: budget.ID, Kind: model.KindProject, Name: "Lab", Reason: "lab", Limit: cores(2),
		AdminScope: common.TokenList{"user:co@example.org"},
		AuthorizedUsers: []common.AuthorizedUser{
			{Token: "user:member@example.org", Role: "member"},
			{Token: dev.CourseGroup, Role: "member"},
			{Token: "user:other1@example.org", Role: "member"},
			{Token: "user:other2@example.org", Role: "reader"},
		},
	}, model.UIActor("owner@example.org"), "owner@example.org", f.ownerT)
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if project, err = svc.ApproveNode(project.ID, tree.ApproveNodeRequest{ExpectedRevision: new(project.CurrentRevision())}, model.UIActor("manager@example.org"), f.managerT); err != nil {
		t.Fatalf("approve: %v", err)
	}
	f.budget, f.project = budget, project
	return f
}

// assertFull says the node shows every member, the admins and the history.
func assertFull(t *testing.T, who string, n model.Node) {
	t.Helper()
	if len(n.AuthorizedUsers) != 4 || n.MembersHidden != 0 || len(n.AdminScope) != 1 || len(n.History) == 0 || n.ViewerRelation == "member" {
		t.Errorf("%s: expected the full project, got %d members (%d hidden), admins %v, %d history entries, relation %q", who, len(n.AuthorizedUsers), n.MembersHidden, n.AdminScope, len(n.History), n.ViewerRelation)
	}
}

// assertRedacted says the node shows only the caller's own entry and a count of the others.
func assertRedacted(t *testing.T, who, ownToken string, n model.Node, revision int) {
	t.Helper()
	if len(n.AuthorizedUsers) != 1 || n.AuthorizedUsers[0].Token != ownToken || n.MembersHidden != 3 {
		t.Errorf("%s: expected only %s and 3 hidden, got %v (%d hidden)", who, ownToken, n.AuthorizedUsers, n.MembersHidden)
	}
	if n.AdminScope != nil || n.History != nil || n.CreatedBy != "" || n.ViewerRelation != "member" {
		t.Errorf("%s: admins %v, %d history entries, created_by %q, relation %q must be gone", who, n.AdminScope, len(n.History), n.CreatedBy, n.ViewerRelation)
	}
	if n.Owner != "user:owner@example.org" {
		t.Errorf("%s: the owner must stay as the contact, got %q", who, n.Owner)
	}
	if n.CurrentRevision() != revision {
		t.Errorf("%s: revision changed from %d to %d", who, revision, n.CurrentRevision())
	}
	for _, a := range n.Allocations {
		if a.CreatedBy != "" || a.Reason != "" || a.Attributes != nil {
			t.Errorf("%s: allocation %s keeps who asked for it: %+v", who, a.ID, a)
		}
		if a.BudgetName != "Chair" || a.Limit["cores"] != 2 {
			t.Errorf("%s: allocation %s lost budget or limit: %+v", who, a.ID, a)
		}
	}
}

func TestMemberPrivacy_GetNode(t *testing.T) {
	f := newPrivacyFixture(t)
	rev := f.project.CurrentRevision()
	for who, tokens := range map[string]common.TokenList{"owner": f.ownerT, "admin": f.adminT, "budget manager": f.managerT, "root admin": f.rootT} {
		n, err := f.svc.GetNode(f.project.ID, tokens)
		if err != nil {
			t.Fatalf("%s: get: %v", who, err)
		}
		assertFull(t, who, *n)
	}
	for who, c := range map[string]struct {
		tokens common.TokenList
		own    string
	}{"member": {f.memberT, "user:member@example.org"}, "member by group": {f.groupT, dev.CourseGroup}} {
		n, err := f.svc.GetNode(f.project.ID, c.tokens)
		if err != nil {
			t.Fatalf("%s: get: %v", who, err)
		}
		assertRedacted(t, who, c.own, *n, rev)
	}
	if _, err := f.svc.GetNode(f.project.ID, f.strangerT); err == nil {
		t.Error("a stranger must not read the project")
	}
}

func TestMemberPrivacy_ListMine(t *testing.T) {
	f := newPrivacyFixture(t)
	mine, err := f.svc.ListMine("member@example.org", f.memberT, 0, 0)
	if err != nil || mine.Total != 1 {
		t.Fatalf("member: %v, %d", err, mine.Total)
	}
	assertRedacted(t, "member", "user:member@example.org", mine.Items[0], f.project.CurrentRevision())
	for who, c := range map[string]struct {
		email  string
		tokens common.TokenList
	}{"owner": {"owner@example.org", f.ownerT}, "admin": {"co@example.org", f.adminT}} {
		mine, err := f.svc.ListMine(c.email, c.tokens, 0, 0)
		if err != nil || mine.Total != 1 {
			t.Fatalf("%s: %v, %d", who, err, mine.Total)
		}
		assertFull(t, who, mine.Items[0])
	}
}

// A budget manager who is also listed as a member sees the project as a manager.
func TestMemberPrivacy_ManagerWhoIsMember(t *testing.T) {
	f := newPrivacyFixture(t)
	tokens := append(common.TokenList{dev.CourseGroup}, f.managerT...)
	n, err := f.svc.GetNode(f.project.ID, tokens)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	assertFull(t, "manager and member", *n)
}

// An allocation carries its project: a member reading it gets the member's view of both.
func TestMemberPrivacy_Allocation(t *testing.T) {
	f := newPrivacyFixture(t)
	if len(f.project.Allocations) != 1 {
		t.Fatalf("expected one allocation, got %d", len(f.project.Allocations))
	}
	allocID := f.project.Allocations[0].ID
	a, err := f.svc.GetNode(allocID, f.memberT)
	if err != nil {
		t.Fatalf("member reads allocation: %v", err)
	}
	if a.Project == nil {
		t.Fatal("the allocation must carry its project")
	}
	if a.History != nil || a.CreatedBy != "" || a.Reason != "" {
		t.Errorf("allocation keeps history %d, creator %q, reason %q", len(a.History), a.CreatedBy, a.Reason)
	}
	assertRedacted(t, "member via allocation", "user:member@example.org", *a.Project, a.Project.CurrentRevision())
	for who, tokens := range map[string]common.TokenList{"owner": f.ownerT, "budget manager": f.managerT, "root admin": f.rootT} {
		a, err := f.svc.GetNode(allocID, tokens)
		if err != nil {
			t.Fatalf("%s: %v", who, err)
		}
		if a.Project == nil {
			t.Fatalf("%s: no project on the allocation", who)
		}
		// The embedded project never carries history (attachAllocations), so look at the members.
		if len(a.Project.AuthorizedUsers) != 4 || a.Project.MembersHidden != 0 || len(a.History) == 0 {
			t.Errorf("%s: expected the full allocation, got %d members, %d history entries", who, len(a.Project.AuthorizedUsers), len(a.History))
		}
	}
}

// The managers' listings (children, search, to-manage) show projects in full.
func TestMemberPrivacy_ManagerListings(t *testing.T) {
	f := newPrivacyFixture(t)
	children, err := f.svc.ListChildren(f.budget.ID, tree.ChildFilter{}, f.managerT, 0, 0)
	if err != nil || len(children.Items) == 0 {
		t.Fatalf("children: %v, %d", err, len(children.Items))
	}
	for _, c := range children.Items {
		if c.Project != nil && (len(c.Project.AuthorizedUsers) != 4 || c.Project.ViewerRelation == "member") {
			t.Errorf("children: the manager must see every member, got %v", c.Project.AuthorizedUsers)
		}
	}
	found, err := f.svc.SearchNodes(f.managerT, "Lab", 0, 0)
	if err != nil || found.Total == 0 {
		t.Fatalf("search: %v, %d", err, found.Total)
	}
	for _, n := range found.Items {
		p := n
		if n.Project != nil {
			p = *n.Project
		}
		if p.Kind == model.KindProject && len(p.AuthorizedUsers) != 4 {
			t.Errorf("search: the manager must see every member, got %v", p.AuthorizedUsers)
		}
	}
	// A second request waits for the manager, who sees its members.
	if _, err := f.svc.CreateNode(tree.CreateNodeRequest{
		ParentID: f.budget.ID, Kind: model.KindProject, Name: "Lab 2", Reason: "lab", Limit: cores(1),
		AuthorizedUsers: []common.AuthorizedUser{{Token: "user:member@example.org", Role: "member"}, {Token: "user:other1@example.org", Role: "member"}},
	}, model.UIActor("owner@example.org"), "owner@example.org", f.ownerT); err != nil {
		t.Fatalf("create second project: %v", err)
	}
	waiting, err := f.svc.ListToManage(f.managerT, false, 0, 0)
	if err != nil || waiting.Total == 0 {
		t.Fatalf("to-manage: %v, %d", err, waiting.Total)
	}
	for _, n := range waiting.Items {
		if n.Project != nil && (len(n.Project.AuthorizedUsers) != 2 || n.Project.MembersHidden != 0) {
			t.Errorf("to-manage: the manager must see every member, got %v", n.Project.AuthorizedUsers)
		}
	}
	// The member of that request sees it redacted in their own list.
	mine, err := f.svc.ListMine("member@example.org", f.memberT, 0, 0)
	if err != nil || mine.Total != 2 {
		t.Fatalf("member list: %v, %d", err, mine.Total)
	}
	for _, n := range mine.Items {
		if n.ViewerRelation != "member" || len(n.AuthorizedUsers) != 1 || n.MembersHidden == 0 {
			t.Errorf("member list: %s not redacted: %v (%d hidden)", n.Name, n.AuthorizedUsers, n.MembersHidden)
		}
	}
}
