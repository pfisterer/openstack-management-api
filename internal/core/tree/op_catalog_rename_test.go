package tree

import (
	"context"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
)

// A GPU class of the AI service that replaced one from before pools: every stored amount moves to the new id, once.
func TestRenameResources(t *testing.T) {
	svc := availabilityService(t)
	ctx := context.Background()
	parent := "b"
	seed := []model.Node{
		{ID: "b", Kind: model.KindBudget, Status: model.StatusApproved, Name: "Budget", Limit: common.ProjectQuota{"cores": 10, "ws-gpus-l4-3q": 4, "ws-gpus-allgemein-klein": 1}},
		{ID: "p", Kind: model.KindProject, Status: model.StatusApproved, Name: "Projekt", ParentID: &parent, Limit: common.ProjectQuota{"cores": 2, "ws-gpus-l4-3q": 2}},
	}
	for _, n := range seed {
		if err := svc.store.UpsertNode(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	renames := map[string]string{"ws-gpus-l4-3q": "ws-gpus-allgemein-klein"}
	if err := svc.RenameResources(ctx, renames); err != nil {
		t.Fatal(err)
	}
	b, _ := svc.store.GetNode(ctx, "b")
	p, _ := svc.store.GetNode(ctx, "p")
	if _, old := b.Limit["ws-gpus-l4-3q"]; old || b.Limit["ws-gpus-allgemein-klein"] != 1 {
		t.Errorf("budget with both keeps the new one: %v", b.Limit)
	}
	if _, old := p.Limit["ws-gpus-l4-3q"]; old || p.Limit["ws-gpus-allgemein-klein"] != 2 || p.Limit["cores"] != 2 {
		t.Errorf("project moved: %v", p.Limit)
	}
	if err := svc.RenameResources(ctx, renames); err != nil {
		t.Errorf("second run: %v", err)
	}
}
