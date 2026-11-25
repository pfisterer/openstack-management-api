package tree

import (
	"context"
	"testing"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/nodestore"
)

// asAllocations turns leaves written as projects of the former model (a limit and what OpenStack measured, under a budget) into allocations, each the home allocation of a project of its own holding the same: the shape the rollup takes since allocation_leaves.
func asAllocations(leaves []model.Node) ([]model.Node, map[string]model.Node) {
	out := make([]model.Node, 0, len(leaves))
	projects := map[string]model.Node{}
	for _, l := range leaves {
		p := model.Node{ID: "proj-" + l.ID, Kind: model.KindProject, Status: l.Status, Limit: l.Limit, Bindings: l.Bindings, HomeAllocationID: l.ID, HomeBudgetID: l.ParentID}
		projects[p.ID] = p
		a := l
		a.Kind = model.KindAllocation
		a.ProjectID = p.ID
		a.Bindings = nil
		out = append(out, a)
	}
	return out, projects
}

// seedLegacy writes nodes given as the former model — projects in the tree, extra allocations in Allocations — as they are stored since allocation_leaves.
func seedLegacy(t *testing.T, store nodestore.Store, nodes ...model.Node) {
	t.Helper()
	migrated, err := nodestore.MigrateNodes(nodes, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range migrated {
		if err := store.UpsertNode(context.Background(), n); err != nil {
			t.Fatal(err)
		}
	}
}
