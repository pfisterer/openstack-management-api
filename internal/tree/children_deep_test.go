package tree_test

import (
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/tree"
)

// A budget whose projects all live in sub-budgets lists none of its own; with
// Deep it lists those of the whole subtree, still filtered as asked.
func TestListChildrenDeep(t *testing.T) {
	f := newAllocationFixture(t)
	own := mustCreate(t, f.svc, tree.CreateNodeRequest{ParentID: f.uni.ID, Kind: tree.KindProject, Name: "Own", Limit: cores(1)})

	page, err := f.svc.ListChildren(f.uni.ID, tree.ChildFilter{Kind: tree.KindProject}, uniTokens, 50, 0)
	if err != nil || page.Total != 1 || page.Items[0].ID != own.ID {
		t.Fatalf("direct children: %v, %+v", err, page)
	}
	page, err = f.svc.ListChildren(f.uni.ID, tree.ChildFilter{Kind: tree.KindProject, Deep: true}, uniTokens, 50, 0)
	if err != nil || page.Total != 2 {
		t.Fatalf("deep: want the own project and the one in the student budget, got %v, %d", err, page.Total)
	}
	for _, n := range page.Items {
		if n.Kind != tree.KindProject {
			t.Errorf("deep must list projects only, got %s", n.Kind)
		}
		if n.ID == f.project.ID && n.ParentName != "Students" {
			t.Errorf("a project from a sub-budget should name it, got %q", n.ParentName)
		}
	}
	page, err = f.svc.ListChildren(f.uni.ID, tree.ChildFilter{Kind: tree.KindProject, Deep: true, Query: "thesis"}, uniTokens, 50, 0)
	if err != nil || page.Total != 1 || page.Items[0].ID != f.project.ID {
		t.Errorf("deep with a text filter: %v, %+v", err, page.Items)
	}
}
