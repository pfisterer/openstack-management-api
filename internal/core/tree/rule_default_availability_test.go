package tree

import (
	"testing"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/nodestore"
	"go.uber.org/zap"
)

// A new project gets the availabilities with default 1 its budget holds, only in the areas it asks something of, and never against an explicit 0 — whatever way the request came (form, start page, auto-approve, API).
func TestWithDefaultAvailabilities(t *testing.T) {
	log := zap.NewNop().Sugar()
	catalogue := []common.Resource{
		{ID: "cores", Name: "Cores"},
		{ID: "ws-vcpu-hours", Name: "Workspace CPU", Kind: common.KindWindow, Window: "30d", Area: common.AreaWorkspaces},
		{ID: "ws-profile-cpu", Name: "Server type: CPU", Kind: common.KindBool, Default: 1, Area: common.AreaWorkspaces},
		{ID: "ws-profile-gpu", Name: "Server type: GPU", Kind: common.KindBool, Default: 1, Area: common.AreaWorkspaces},
		{ID: "dhbw-ipv4", Name: "DHBW IPv4", Kind: common.KindBool},
	}
	svc := NewService(nodestore.NewInMemoryStore(log), noRoles{}, catalogue, common.TokenList{"group:root-admin"}, 5*time.Second, common.DefaultMaxAuthorizedUsers, Accounting{}, log)
	budget := model.Node{ID: "b", Kind: model.KindBudget, Limit: common.ProjectQuota{"ws-vcpu-hours": 100, "ws-profile-cpu": 1}}
	inheriting := model.Node{ID: "i", Kind: model.KindBudget, InheritsLimit: true}
	root := model.Node{ID: model.RootNodeID, Kind: model.KindBudget}

	got := svc.withDefaultAvailabilities([]model.Node{budget, root}, common.ProjectQuota{"ws-vcpu-hours": 10})
	if got["ws-profile-cpu"] != 1 {
		t.Errorf("held default not set: %v", got)
	}
	if _, ok := got["ws-profile-gpu"]; ok {
		t.Errorf("default the budget does not hold was set: %v", got)
	}
	if _, ok := got["dhbw-ipv4"]; ok {
		t.Errorf("availability without default 1 was set: %v", got)
	}
	if got := svc.withDefaultAvailabilities([]model.Node{budget, root}, common.ProjectQuota{"ws-vcpu-hours": 10, "ws-profile-cpu": 0}); got["ws-profile-cpu"] != 0 {
		t.Errorf("explicit 0 overridden: %v", got)
	}
	if got := svc.withDefaultAvailabilities([]model.Node{budget, root}, common.ProjectQuota{"cores": 4}); len(got) != 1 {
		t.Errorf("cloud project got workspace availabilities: %v", got)
	}
	// Under a budget that inherits, the root decides: it holds the whole catalogue.
	if got := svc.withDefaultAvailabilities([]model.Node{inheriting, root}, common.ProjectQuota{"ws-vcpu-hours": 10}); got["ws-profile-cpu"] != 1 || got["ws-profile-gpu"] != 1 {
		t.Errorf("inheriting budget: %v", got)
	}
}
