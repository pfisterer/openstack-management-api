package tree

import (
	"context"
	"testing"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"go.uber.org/zap"
)

// archivedSvc: root → Uni → Chair, and an archived project under Chair holding
// 4 cores and 50 GB of its own, 2 cores allocated from Uni, 30 GB measured.
func archivedSvc(t *testing.T, acc Accounting) (*Service, Node, Node) {
	t.Helper()
	log := zap.NewNop().Sugar()
	store := NewInMemoryStore(log)
	svc := NewService(store, noRoles{}, []common.ManagedProject{
		{ID: "cores", Name: "Cores", OSQuotaField: "cores"},
		{ID: "storage", Name: "Storage", OSQuotaField: "gigabytes"},
	}, common.TokenList{"group:admins"}, 5*time.Second, common.DefaultMaxAuthorizedUsers, acc, log)
	ctx := context.Background()
	if err := svc.Bootstrap(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	root := RootNodeID
	uniID, chairID := "uni", "chair"
	for _, n := range []Node{
		{ID: uniID, Kind: KindBudget, ParentID: &root, Status: StatusApproved, Name: "Uni",
			Limit: common.ProjectQuota{"cores": 100, "storage": 1000}},
		{ID: chairID, Kind: KindBudget, ParentID: &uniID, Status: StatusApproved, Name: "Chair",
			Limit: common.ProjectQuota{"cores": 10, "storage": 100}},
		{ID: "p", Kind: KindProject, ParentID: &chairID, Status: StatusArchived, Name: "Old",
			Limit:       common.ProjectQuota{"cores": 4, "storage": 50},
			Allocations: []Allocation{{BudgetID: uniID, Limit: common.ProjectQuota{"cores": 2}}},
			OSInUse:     common.ProjectQuota{"cores": 0, "storage": 30}},
	} {
		if err := store.UpsertNode(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	uni, _ := store.GetNode(ctx, uniID)
	chair, _ := store.GetNode(ctx, chairID)
	return svc, *uni, *chair
}

func rolledUp(t *testing.T, svc *Service, budget Node) common.ProjectQuota {
	t.Helper()
	usage, err := svc.loadSubtreeUsage(context.Background(), []Node{budget})
	if err != nil {
		t.Fatal(err)
	}
	return usage[budget.ID].Total([]string{"cores", "storage"})
}

// Charged, an archived project costs only the storage it measurably holds, in
// its own budget; its cores and its allocation from further up are free again.
func TestArchivedAccounting_StorageOnly(t *testing.T) {
	svc, uni, chair := archivedSvc(t, Accounting{ChargeOSInUse: true, ChargeReleased: true, ChargeArchived: true})
	if got := rolledUp(t, svc, chair); got["cores"] != 0 || got["storage"] != 30 {
		t.Errorf("chair is charged %v, want 0 cores and the 30 GB measured", got)
	}
	if got := rolledUp(t, svc, uni); got["cores"] != 0 || got["storage"] != 30 {
		t.Errorf("uni is charged %v, want no allocated cores, the storage rolled up", got)
	}
}

// Not charged, archiving frees the budget entirely.
func TestArchivedAccounting_Free(t *testing.T) {
	svc, uni, chair := archivedSvc(t, Accounting{ChargeOSInUse: true, ChargeReleased: true})
	for _, b := range []Node{chair, uni} {
		if got := rolledUp(t, svc, b); got["cores"] != 0 || got["storage"] != 0 {
			t.Errorf("%s is charged %v for an archived project", b.Name, got)
		}
	}
}

func TestMarkArchived(t *testing.T) {
	n := Node{Status: StatusReleased}
	if !MarkArchived(&n) || n.Status != StatusArchived || len(n.History) != 1 || n.History[0].Via != ChannelReconciler {
		t.Errorf("released leaf not archived properly: %+v", n)
	}
	if MarkArchived(&n) {
		t.Error("an archived leaf was archived again")
	}
	approved := Node{Status: StatusApproved}
	if MarkArchived(&approved) {
		t.Error("an approved leaf must not be archived")
	}
}
