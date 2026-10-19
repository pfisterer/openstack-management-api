package tree

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/nodestore"
	"go.uber.org/zap"
)

// A budget shows what the projects below measurably use, summed over its whole subtree and across providers, apart from what they were handed out; a resource nobody measures stays absent.
func TestSubtreeInUse(t *testing.T) {
	log := zap.NewNop().Sugar()
	store := nodestore.NewInMemoryStore(log)
	svc := NewService(store, noRoles{}, []common.Resource{
		{ID: "cores", Name: "Cores", Accounts: common.AccountsCores},
		{ID: "gpu-hours", Name: "GPU hours"},
		{ID: "ram", Name: "RAM"},
	}, common.TokenList{"group:admins"}, 5*time.Second, common.DefaultMaxAuthorizedUsers, Accounting{ChargeInUse: true}, log)
	ctx := context.Background()
	if err := svc.Bootstrap(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	root := model.RootNodeID
	uni, chair, other := "uni", "chair", "other"
	for _, n := range []model.Node{
		{ID: uni, Kind: model.KindBudget, ParentID: &root, Status: model.StatusApproved, Name: "Uni", Limit: common.ProjectQuota{"cores": 400, "gpu-hours": 200, "ram": 100}},
		{ID: chair, Kind: model.KindBudget, ParentID: &uni, Status: model.StatusApproved, Name: "Chair", Limit: common.ProjectQuota{"cores": 20, "gpu-hours": 50}},
		{ID: other, Kind: model.KindBudget, ParentID: &uni, Status: model.StatusApproved, Name: "Other", Limit: common.ProjectQuota{"cores": 20}},
		{ID: "p1", Kind: model.KindProject, ParentID: &chair, Status: model.StatusApproved, Limit: common.ProjectQuota{"cores": 8, "gpu-hours": 20},
			Bindings: map[string]model.Binding{"openstack": {InUse: common.ProjectQuota{"cores": 3}}, "aiaas": {InUse: common.ProjectQuota{"gpu-hours": 5}}}},
		{ID: "p2", Kind: model.KindProject, ParentID: &chair, Status: model.StatusApproved, Limit: common.ProjectQuota{"cores": 2},
			Bindings: map[string]model.Binding{"openstack": {InUse: common.ProjectQuota{"cores": 0}}}},
		{ID: "p3", Kind: model.KindProject, ParentID: &other, Status: model.StatusApproved, Limit: common.ProjectQuota{"cores": 4},
			Bindings: map[string]model.Binding{"openstack": {InUse: common.ProjectQuota{"cores": 4}}}},
		// Not charged, so neither allocated nor counted as used.
		{ID: "p4", Kind: model.KindProject, ParentID: &other, Status: model.StatusRejected, Limit: common.ProjectQuota{"cores": 4},
			Bindings: map[string]model.Binding{"openstack": {InUse: common.ProjectQuota{"cores": 9}}}},
	} {
		if err := store.UpsertNode(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	nodes, err := svc.attachUsage(ctx, []model.Node{mustLoad(t, store, uni), mustLoad(t, store, chair), mustLoad(t, store, other), mustLoad(t, store, "p1")}, common.TokenList{"group:admins"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]common.ProjectQuota{}
	for _, n := range nodes {
		got[n.ID] = n.SubtreeInUse
	}
	if u := got[chair]; u["cores"] != 3 || u["gpu-hours"] != 5 || len(u) != 2 {
		t.Errorf("chair = %v, want 3 cores and 5 GPU hours, nothing else", u)
	}
	if u := got[other]; u["cores"] != 4 || len(u) != 1 {
		t.Errorf("other = %v, want 4 cores (the rejected project is not counted)", u)
	}
	if u := got[uni]; u["cores"] != 7 || u["gpu-hours"] != 5 {
		t.Errorf("uni = %v, want 7 cores and 5 GPU hours over the whole subtree", u)
	}
	if _, measured := got[uni]["ram"]; measured {
		t.Errorf("uni = %v: RAM is measured by nobody and must stay absent", got[uni])
	}
	if got["p1"] != nil {
		t.Errorf("a project has no subtree_in_use, got %v", got["p1"])
	}
}

// A sum past the int range stays at its end instead of wrapping negative.
func TestSubtreeInUse_Saturates(t *testing.T) {
	b := "b"
	parents := map[string]*string{b: nil}
	leaves := []model.Node{
		{ID: "x", ParentID: &b, Bindings: map[string]model.Binding{"openstack": {InUse: common.ProjectQuota{"cores": math.MaxInt}}}},
		{ID: "y", ParentID: &b, Bindings: map[string]model.Binding{"openstack": {InUse: common.ProjectQuota{"cores": 1}}}},
	}
	if got := rollUpInUse(leaves, parents)[b]["cores"]; got != math.MaxInt {
		t.Fatalf("sum wrapped to %d", got)
	}
}

func mustLoad(t *testing.T, store *nodestore.InMemoryStore, id string) model.Node {
	t.Helper()
	n, err := store.GetNode(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return *n
}
