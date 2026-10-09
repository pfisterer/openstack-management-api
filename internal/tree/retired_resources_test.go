package tree_test

import (
	"context"
	"testing"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/roleprovider"
	"github.com/pfisterer/openstack-management-api/internal/tree"
	"go.uber.org/zap"
)

// A quantity taken out of the catalogue disappears from every node on the next
// start — limits, allocations, auto-approve — so the nodes stay editable.
func TestRetiredResourceIsDroppedOnStart(t *testing.T) {
	log := zap.NewNop().Sugar()
	store := tree.NewInMemoryStore(log)
	start := func(resources []common.ManagedProject) *tree.Service {
		t.Helper()
		svc := tree.NewService(store, roleprovider.NewMockRoleProvider(), resources,
			common.TokenList{"group:root"}, 5*time.Second, common.DefaultMaxAuthorizedUsers, testAccounting, log)
		if err := svc.Bootstrap(context.Background(), nil, nil); err != nil {
			t.Fatalf("bootstrap: %v", err)
		}
		return svc
	}

	svc := start(allocationResources)
	uni, err := svc.CreateNode(tree.CreateNodeRequest{ParentID: tree.RootNodeID, Kind: tree.KindBudget, Name: "Uni", Reason: "t",
		Limit: common.ProjectQuota{"cores": 100, "gpu": 4}, AdminScope: common.TokenList{"group:uni"},
		EligibleRequesters: common.TokenList{"user:stud@x"},
		AutoApprove:        &tree.AutoApprove{PerRequesterLimit: common.ProjectQuota{"cores": 4, "gpu": 1}}}, tree.UIActor("root@x"), "root@x", rootTokens)
	if err != nil {
		t.Fatal(err)
	}
	p, err := svc.CreateNode(tree.CreateNodeRequest{ParentID: uni.ID, Kind: tree.KindProject, Name: "P", Reason: "t",
		Limit: common.ProjectQuota{"cores": 2, "gpu": 1}}, tree.UIActor("stud@x"), "stud@x", ownerToken)
	if err != nil {
		t.Fatal(err)
	}

	withoutGPU := []common.ManagedProject{allocationResources[0], allocationResources[2]}
	svc = start(withoutGPU)

	for _, id := range []string{tree.RootNodeID, tree.UnassignedNodeID, uni.ID, p.ID} {
		n := mustGet(t, svc, id)
		if _, ok := n.Limit["gpu"]; ok {
			t.Errorf("%s still holds gpu: %v", id, n.Limit)
		}
	}
	if u := mustGet(t, svc, uni.ID); u.Limit["cores"] != 100 || u.AutoApprove.PerRequesterLimit["gpu"] != 0 || u.AutoApprove.PerRequesterLimit["cores"] != 4 {
		t.Errorf("uni after the start: limit %v, auto-approve %v", u.Limit, u.AutoApprove.PerRequesterLimit)
	}
	// And it can be edited again with its limit as read.
	u := mustGet(t, svc, uni.ID)
	if _, err := svc.UpdateNode(uni.ID, tree.UpdateNodeRequest{Limit: &u.Limit}, tree.UIActor("root@x"), rootTokens); err != nil {
		t.Errorf("edit after the start: %v", err)
	}
}
