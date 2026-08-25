package tree_test

import (
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/tree"
)

// my-budgets carries the ancestor chain, and the reason is a bug that only
// appears when the chain SKIPS a level.
//
// A client holding the budgets it manages cannot tell which of them are the
// top-most ones from ParentID alone: the nodes between two managed budgets
// belong to someone else and are not in the list. Comparing parents therefore
// reads a budget two levels under another managed budget as a root of its own,
// and the tree draws it twice — once at the top, once in its real place.
//
// Observed on staging on 2026-08-25: a root admin also managed "WI-Budget",
// which hangs under "Mannheim" (managed by someone else) under the root. That
// is the shape built here.
func TestListMyBudgets_AncestorsSpanUnmanagedLevels(t *testing.T) {
	admin := common.TokenList{"group:root-admin"}
	svc, _ := newSvc(t, admin)

	other := common.TokenList{"user:clemens@dhbw.de"}
	mine := common.TokenList{"group:root-admin"}

	// root ─ Mannheim (someone else's) ─ WI-Budget (also mine)
	mannheim, err := svc.CreateNode(tree.CreateNodeRequest{
		Kind: tree.KindBudget, ParentID: tree.RootNodeID, Name: "Mannheim",
		Reason: "faculty", Limit: cores(64), AdminScope: other,
	}, "root@dhbw.de", "root@dhbw.de", admin)
	if err != nil {
		t.Fatalf("create Mannheim: %v", err)
	}

	wi, err := svc.CreateNode(tree.CreateNodeRequest{
		Kind: tree.KindBudget, ParentID: mannheim.ID, Name: "WI-Budget",
		Reason: "course", Limit: cores(16), AdminScope: mine,
	}, "root@dhbw.de", "root@dhbw.de", admin)
	if err != nil {
		t.Fatalf("create WI-Budget: %v", err)
	}

	page, err := svc.ListMyBudgets(admin, 100, 0)
	if err != nil {
		t.Fatalf("list my budgets: %v", err)
	}

	byID := map[string]tree.Node{}
	for _, n := range page.Items {
		byID[n.ID] = n
	}

	// The root itself is managed and has nowhere to sit above it.
	if got := byID[tree.RootNodeID].AncestorIDs; len(got) != 0 {
		t.Fatalf("root ancestors = %v, want none", got)
	}

	// The whole point: WI-Budget's PARENT is not in the caller's list, but its
	// grandparent is — and only the chain says so.
	got := byID[wi.ID].AncestorIDs
	want := []string{tree.RootNodeID, mannheim.ID}
	if len(got) != len(want) {
		t.Fatalf("WI-Budget ancestors = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("WI-Budget ancestors = %v, want %v (root-most first)", got, want)
		}
	}

	// And the client's actual question, answered with the chain: exactly one of
	// these is a root to draw at the top.
	managed := map[string]bool{}
	for _, n := range page.Items {
		managed[n.ID] = true
	}
	var roots []string
	for _, n := range page.Items {
		top := true
		for _, a := range n.AncestorIDs {
			if managed[a] {
				top = false
				break
			}
		}
		if top {
			roots = append(roots, n.ID)
		}
	}
	if len(roots) != 1 || roots[0] != tree.RootNodeID {
		t.Fatalf("top-most managed budgets = %v, want [%s]", roots, tree.RootNodeID)
	}
}

// The unmanaged middle node must not leak into the response beyond its ID: the
// caller is not allowed to see it, and an ID is the least that answers the
// question. Anything more would be a quiet widening of what my-budgets exposes.
func TestListMyBudgets_ReturnsOnlyManagedNodes(t *testing.T) {
	admin := common.TokenList{"group:root-admin"}
	svc, _ := newSvc(t, admin)

	mannheim, err := svc.CreateNode(tree.CreateNodeRequest{
		Kind: tree.KindBudget, ParentID: tree.RootNodeID, Name: "Mannheim",
		Reason: "faculty", Limit: cores(64), AdminScope: common.TokenList{"user:clemens@dhbw.de"},
	}, "root@dhbw.de", "root@dhbw.de", admin)
	if err != nil {
		t.Fatalf("create Mannheim: %v", err)
	}

	page, err := svc.ListMyBudgets(common.TokenList{"user:clemens@dhbw.de"}, 100, 0)
	if err != nil {
		t.Fatalf("list my budgets: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != mannheim.ID {
		t.Fatalf("items = %v, want only %s", page.Items, mannheim.ID)
	}
	// Its own ancestor is the root, which this caller does NOT manage — so it is
	// top-most for them, and drawn once.
	if got := page.Items[0].AncestorIDs; len(got) != 1 || got[0] != tree.RootNodeID {
		t.Fatalf("ancestors = %v, want [%s]", got, tree.RootNodeID)
	}
}
