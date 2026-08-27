package tree

import (
	"context"
	"testing"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"go.uber.org/zap"
)

// noRoles satisfies common.RoleProvider without answering anything. The usage
// rollup never asks it; the service just refuses to be built without one. Kept
// local because an internal test cannot import roleprovider — its mock reaches
// back into this package through mockdata, which is an import cycle.
type noRoles struct{}

func (noRoles) GetUserTokens(context.Context, *common.UserClaims) (common.TokenList, error) {
	return nil, nil
}
func (noRoles) SearchGroups(context.Context, string, int) ([]common.GroupSummary, error) {
	return nil, nil
}
func (noRoles) SearchUsers(context.Context, string, int) ([]string, error) { return nil, nil }
func (noRoles) GetGroupUsers(context.Context, string) ([]string, error)    { return nil, nil }

// accountingSvc builds a service over an in-memory store holding one budget
// with a single leaf in the given status.
func accountingSvc(t *testing.T, acc Accounting, leafStatus string) (*Service, string) {
	t.Helper()
	log := zap.NewNop().Sugar()
	store := NewInMemoryStore(log)
	svc := NewService(store, noRoles{}, []common.ManagedProject{{ID: "cores", Name: "Cores"}},
		common.TokenList{"group:admins"}, 5*time.Second, common.DefaultMaxAuthorizedUsers, acc, log)

	ctx := context.Background()
	if err := svc.Bootstrap(ctx, nil, nil); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	budgetID := "b_accounting"
	rootID := RootNodeID
	if err := store.UpsertNode(ctx, Node{
		ID: budgetID, Kind: KindBudget, ParentID: &rootID,
		Status: StatusApproved, Name: "Accounting", Limit: common.ProjectQuota{"cores": 100},
		AdminScope: common.TokenList{"group:admins"},
	}); err != nil {
		t.Fatalf("write budget: %v", err)
	}
	if err := store.UpsertNode(ctx, Node{
		ID: "p_accounting", Kind: KindProject, ParentID: &budgetID,
		Status: leafStatus, Name: "Leaf", Limit: common.ProjectQuota{"cores": 7},
		Owner: "user:someone@example.edu",
	}); err != nil {
		t.Fatalf("write leaf: %v", err)
	}
	return svc, budgetID
}

func budgetCores(t *testing.T, svc *Service, budgetID string) int {
	t.Helper()
	budget, err := svc.store.GetNode(context.Background(), budgetID)
	if err != nil || budget == nil {
		t.Fatalf("load budget: %v", err)
	}
	usage, err := svc.loadSubtreeUsage(context.Background(), []Node{*budget})
	if err != nil {
		t.Fatalf("usage rollup: %v", err)
	}
	return usage[budgetID].Total([]string{"cores"})["cores"]
}

// The point of the switch: releasing does not delete the OpenStack project, so
// its cores stay occupied and stay booked. Without this a budget could be
// re-spent on capacity that is still running.
func TestReleasedLeafAccounting_ChargedWhenOn(t *testing.T) {
	svc, budgetID := accountingSvc(t, Accounting{ChargeOSInUse: true, ChargeReleased: true}, StatusReleased)
	if got := budgetCores(t, svc, budgetID); got != 7 {
		t.Errorf("released leaf must still cost its 7 cores, got %d", got)
	}
}

// Off is the pre-2026-08 behaviour: the budget frees up the moment the project
// is released, and over-booking is accepted for as long as the deletion takes.
func TestReleasedLeafAccounting_FreeWhenOff(t *testing.T) {
	svc, budgetID := accountingSvc(t, Accounting{ChargeOSInUse: true}, StatusReleased)
	if got := budgetCores(t, svc, budgetID); got != 0 {
		t.Errorf("with the switch off a released leaf costs nothing, got %d", got)
	}
}

// The switch must not reach any status but released — an approved leaf is
// charged either way, and a rejected one never is.
func TestReleasedLeafAccounting_LeavesOtherStatusesAlone(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   int
	}{
		{StatusApproved, 7},
		{StatusChangePending, 7},
		{StatusPending, 0},
		{StatusRejected, 0},
	} {
		t.Run(tc.status, func(t *testing.T) {
			for _, charge := range []bool{false, true} {
				svc, budgetID := accountingSvc(t,
					Accounting{ChargeOSInUse: true, ChargeReleased: charge}, tc.status)
				if got := budgetCores(t, svc, budgetID); got != tc.want {
					t.Errorf("ChargeReleased=%v: %s cost %d, want %d", charge, tc.status, got, tc.want)
				}
			}
		})
	}
}
