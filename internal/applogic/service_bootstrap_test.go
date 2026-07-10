package applogic_test

import (
	"context"
	"testing"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/applogic"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/roleprovider"
	"github.com/pfisterer/openstack-management-api/internal/storage"
	"go.uber.org/zap"
)

// TestEnsureRootDelegation verifies real-mode startup bootstraps a single
// unlimited top-level pool owned by the root admins, idempotently.
func TestEnsureRootDelegation(t *testing.T) {
	resources := []string{"cores", "ram", "storage", "gpu"}
	log := zap.NewNop().Sugar()
	store := storage.NewInMemoryProjectStore(log)
	svc := applogic.NewService(store, roleprovider.NewMockRoleProvider(), resources,
		common.TokenList{"group:root-admin"}, 10*time.Second, log)

	ctx := context.Background()
	if err := svc.InitializeState(ctx, false); err != nil { // real mode
		t.Fatalf("InitializeState: %v", err)
	}

	root, err := store.GetDelegationByID(ctx, "root")
	if err != nil || root == nil {
		t.Fatalf("expected a bootstrapped root delegation, got %v (err=%v)", root, err)
	}
	if root.ParentID != nil {
		t.Errorf("root must be top-level, got parent=%q", *root.ParentID)
	}
	if !root.CanDelegate || root.DelegationStrategy != common.DelegationStrategyPool {
		t.Errorf("root must be a delegatable pool, got can_delegate=%v strategy=%q", root.CanDelegate, root.DelegationStrategy)
	}
	for _, id := range resources {
		if root.Quota.Limit[id] != common.UnlimitedQuota {
			t.Errorf("root %s limit = %d, want unlimited (%d)", id, root.Quota.Limit[id], common.UnlimitedQuota)
		}
	}
	if !common.NewTokenSet(root.AdminScope).ContainsAny(common.TokenList{"group:root-admin"}) {
		t.Errorf("root admin_scope missing group:root-admin: %v", root.AdminScope)
	}

	// Idempotent: re-init keeps the same root (no duplicate, stable CreatedAt).
	if err := svc.InitializeState(ctx, false); err != nil {
		t.Fatalf("re-init: %v", err)
	}
	if root2, _ := store.GetDelegationByID(ctx, "root"); root2 == nil || root2.CreatedAt != root.CreatedAt {
		t.Errorf("root delegation should be stable across re-init")
	}
}

// TestNoRootWithoutRootAdmins: no root admins configured → no implicit root.
func TestNoRootWithoutRootAdmins(t *testing.T) {
	log := zap.NewNop().Sugar()
	store := storage.NewInMemoryProjectStore(log)
	svc := applogic.NewService(store, roleprovider.NewMockRoleProvider(),
		[]string{"cores"}, common.TokenList{}, 10*time.Second, log)

	if err := svc.InitializeState(context.Background(), false); err != nil {
		t.Fatalf("InitializeState: %v", err)
	}
	if root, _ := store.GetDelegationByID(context.Background(), "root"); root != nil {
		t.Errorf("expected no root delegation without root admins, got %v", root)
	}
}
