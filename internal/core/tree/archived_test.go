package tree

import (
	"context"
	"testing"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"go.uber.org/zap"
)

// archivedSvc: root → Uni → Chair, and an archived project under Chair holding
// 4 cores and 50 GB of its own, 2 cores allocated from Uni, 30 GB measured.
func archivedSvc(t *testing.T, acc Accounting) (*Service, model.Node, model.Node) {
	t.Helper()
	log := zap.NewNop().Sugar()
	store := NewInMemoryStore(log)
	svc := NewService(store, noRoles{}, []common.Resource{
		{ID: "cores", Name: "Cores", Accounts: common.AccountsCores},
		{ID: "storage", Name: "Storage", Accounts: common.AccountsStorage},
	}, common.TokenList{"group:admins"}, 5*time.Second, common.DefaultMaxAuthorizedUsers, acc, log)
	ctx := context.Background()
	if err := svc.Bootstrap(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	root := model.RootNodeID
	uniID, chairID := "uni", "chair"
	for _, n := range []model.Node{
		{ID: uniID, Kind: model.KindBudget, ParentID: &root, Status: model.StatusApproved, Name: "Uni",
			Limit: common.ProjectQuota{"cores": 100, "storage": 1000}},
		{ID: chairID, Kind: model.KindBudget, ParentID: &uniID, Status: model.StatusApproved, Name: "Chair",
			Limit: common.ProjectQuota{"cores": 10, "storage": 100}},
		{ID: "p", Kind: model.KindProject, ParentID: &chairID, Status: model.StatusArchived, Name: "Old",
			Limit:       common.ProjectQuota{"cores": 4, "storage": 50},
			Allocations: []model.Allocation{{BudgetID: uniID, Limit: common.ProjectQuota{"cores": 2}}},
			InUse:       common.ProjectQuota{"cores": 0, "storage": 30}},
	} {
		if err := store.UpsertNode(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	uni, _ := store.GetNode(ctx, uniID)
	chair, _ := store.GetNode(ctx, chairID)
	return svc, *uni, *chair
}

func rolledUp(t *testing.T, svc *Service, budget model.Node) common.ProjectQuota {
	t.Helper()
	usage, err := svc.loadSubtreeUsage(context.Background(), []model.Node{budget})
	if err != nil {
		t.Fatal(err)
	}
	return usage[budget.ID].Total([]string{"cores", "storage"})
}

// Charged, an archived project costs only the storage it measurably holds, in
// its own budget; its cores and its allocation from further up are free again.
func TestArchivedAccounting_StorageOnly(t *testing.T) {
	svc, uni, chair := archivedSvc(t, Accounting{ChargeInUse: true, ChargeReleased: true, ChargeArchived: true})
	if got := rolledUp(t, svc, chair); got["cores"] != 0 || got["storage"] != 30 {
		t.Errorf("chair is charged %v, want 0 cores and the 30 GB measured", got)
	}
	if got := rolledUp(t, svc, uni); got["cores"] != 0 || got["storage"] != 30 {
		t.Errorf("uni is charged %v, want no allocated cores, the storage rolled up", got)
	}
}

// Not charged, archiving frees the budget entirely.
func TestArchivedAccounting_Free(t *testing.T) {
	svc, uni, chair := archivedSvc(t, Accounting{ChargeInUse: true, ChargeReleased: true})
	for _, b := range []model.Node{chair, uni} {
		if got := rolledUp(t, svc, b); got["cores"] != 0 || got["storage"] != 0 {
			t.Errorf("%s is charged %v for an archived project", b.Name, got)
		}
	}
}

func TestMarkArchived(t *testing.T) {
	n := model.Node{Status: model.StatusReleased}
	if !model.MarkArchived(&n) || n.Status != model.StatusArchived || len(n.History) != 1 || n.History[0].Via != model.ChannelReconciler {
		t.Errorf("released leaf not archived properly: %+v", n)
	}
	if model.MarkArchived(&n) {
		t.Error("an archived leaf was archived again")
	}
	approved := model.Node{Status: model.StatusApproved}
	if model.MarkArchived(&approved) {
		t.Error("an approved leaf must not be archived")
	}
}
