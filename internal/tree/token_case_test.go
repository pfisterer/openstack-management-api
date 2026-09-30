package tree_test

import (
	"context"
	"slices"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/tree"
)

// An address typed with capitals into a budget's lists is the same person as
// the lowercase login: the lists are stored in canonical spelling, so the
// requester finds the budget and may request under it.
func TestTokenListsIgnoreEmailCase(t *testing.T) {
	svc, _ := newSvc(t, common.TokenList{"group:root"})
	rootTokens := common.TokenList{"user:root@x", "group:root"}
	userTokens := common.TokenList{"user:friedemann.schwenkreis@x"}

	budget, err := svc.CreateNode(tree.CreateNodeRequest{
		ParentID: tree.RootNodeID, Kind: tree.KindBudget, Name: "Prof", Reason: "test",
		Limit: cores(10), AdminScope: common.TokenList{"group:root"},
		EligibleRequesters: common.TokenList{" user:Friedemann.Schwenkreis@X ", "user:friedemann.schwenkreis@x"},
	}, tree.UIActor("root@x"), "root@x", rootTokens)
	if err != nil {
		t.Fatalf("create budget: %v", err)
	}
	if !slices.Equal(budget.EligibleRequesters, common.TokenList{"user:friedemann.schwenkreis@x"}) {
		t.Errorf("eligible requesters = %v, want one lowercase entry", budget.EligibleRequesters)
	}

	eligible, err := svc.ListEligibleForMe(userTokens, 0, 0)
	if err != nil || eligible.Total != 1 {
		t.Fatalf("expected the budget to be offered, got %d (%v)", eligible.Total, err)
	}

	admins := common.TokenList{"user:Co.Admin@X"}
	project, err := svc.CreateNode(tree.CreateNodeRequest{
		ParentID: budget.ID, Kind: tree.KindProject, Name: "Lab", Reason: "lab", Limit: cores(2), AdminScope: admins,
	}, tree.UIActor("friedemann.schwenkreis@x"), "friedemann.schwenkreis@x", userTokens)
	if err != nil {
		t.Fatalf("request project: %v", err)
	}
	if !slices.Equal(project.AdminScope, common.TokenList{"user:co.admin@x"}) {
		t.Errorf("project admins = %v", project.AdminScope)
	}

	renamed := common.TokenList{"user:Other@X"}
	updated, err := svc.UpdateNode(project.ID, tree.UpdateNodeRequest{AdminScope: &renamed},
		tree.UIActor("friedemann.schwenkreis@x"), userTokens)
	if err != nil {
		t.Fatalf("update admins: %v", err)
	}
	if !slices.Equal(updated.AdminScope, common.TokenList{"user:other@x"}) {
		t.Errorf("updated admins = %v", updated.AdminScope)
	}
}

// Tokens stored before the canonical spelling existed are rewritten at start,
// so a lookup in canonical spelling finds them.
func TestBootstrapCanonicalizesStoredTokens(t *testing.T) {
	svc, store := newSvc(t, common.TokenList{"group:root"})
	ctx := context.Background()
	parent := tree.RootNodeID
	if err := store.UpsertNode(ctx, tree.Node{
		ID: "p_legacy", Kind: tree.KindProject, ParentID: &parent, Status: tree.StatusApproved, Name: "Legacy",
		Owner:           "user:Old.Owner@X",
		AdminScope:      common.TokenList{"group:Leiter-ZWR", "user:Co@X"},
		AuthorizedUsers: []common.AuthorizedUser{{Token: "group:WWI23SEB", OpenstackRole: "member"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Bootstrap(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	n, err := store.GetNode(ctx, "p_legacy")
	if err != nil || n == nil {
		t.Fatalf("get: %v", err)
	}
	if n.Owner != "user:old.owner@x" ||
		!slices.Equal(n.AdminScope, common.TokenList{"group:leiter-zwr", "user:co@x"}) ||
		n.AuthorizedUsers[0].Token != "group:wwi23seb" {
		t.Errorf("not canonical: owner %q, admins %v, users %v", n.Owner, n.AdminScope, n.AuthorizedUsers)
	}
}
