package reconciler

import (
	"context"
	"testing"

	osclient "github.com/pfisterer/openstack-management-api/internal/openstack/client"
	"github.com/pfisterer/openstack-management-api/internal/tree"
	"go.uber.org/zap"
)

func releasedTestReconciler(t *testing.T, store ReconcilerStore, cfg Config) *Reconciler {
	t.Helper()
	return &Reconciler{store: store, cfg: cfg, log: zap.NewNop().Sugar()}
}

// The case the change exists for: the project is no longer in the reconciler's
// scope — deleted in OpenStack, or moved out of the scope parent, which counts
// the same. The released record has served its purpose and goes.
func TestReleasedLeafIsRemovedOnceItsProjectIsGone(t *testing.T) {
	store := &deletedStore{}
	r := releasedTestReconciler(t, store, Config{})

	var res reconcileResult
	r.removeReleasedLeavesWithoutProject(context.Background(),
		map[string]tree.Node{
			"p_1": {ID: "p_1", OSProjectID: "os-1"},
			"p_2": {ID: "p_2", OSProjectID: "os-2"},
		}, map[string]osclient.ProjectInfo{}, &res)

	if len(store.deleted) != 2 {
		t.Errorf("deleted %v, want both", store.deleted)
	}
	if res.releasedLeavesRemoved != 2 {
		t.Errorf("counted %d removals, want 2", res.releasedLeavesRemoved)
	}
}

// Phase 5 removes from this map every released leaf whose project it saw, so an
// empty map is the normal case: everything released still has its project.
func TestReleasedLeafCleanupDoesNothingWhenAllProjectsAreInScope(t *testing.T) {
	store := &deletedStore{}
	r := releasedTestReconciler(t, store, Config{})

	var res reconcileResult
	r.removeReleasedLeavesWithoutProject(context.Background(),
		map[string]tree.Node{}, map[string]osclient.ProjectInfo{}, &res)

	if len(store.deleted) != 0 || res.releasedLeavesRemoved != 0 {
		t.Errorf("deleted %v / counted %d, want nothing", store.deleted, res.releasedLeavesRemoved)
	}
}

// NoDelete means "destroy nothing in OpenStack". The project here is already
// gone from it; keeping our record would leave the tree disagreeing with the
// cloud, which is what this reconciler exists to prevent. DryRun is the switch
// that does hold it back — it reports what a run would do and writes nothing.
func TestReleasedLeafRemovalRespectsTheSafetySwitches(t *testing.T) {
	cases := []struct {
		name        string
		cfg         Config
		wantDeleted int
		wantCounted int
	}{
		{"NoDelete does not apply to our own records", Config{NoDelete: true}, 1, 1},
		{"DryRun", Config{DryRun: true}, 0, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &deletedStore{}
			r := releasedTestReconciler(t, store, tc.cfg)

			var res reconcileResult
			r.removeReleasedLeavesWithoutProject(context.Background(),
				map[string]tree.Node{"p_1": {ID: "p_1", OSProjectID: "os-1"}},
				map[string]osclient.ProjectInfo{}, &res)

			if len(store.deleted) != tc.wantDeleted {
				t.Errorf("deleted %v, want %d", store.deleted, tc.wantDeleted)
			}
			if res.releasedLeavesRemoved != tc.wantCounted {
				t.Errorf("counted %d, want %d", res.releasedLeavesRemoved, tc.wantCounted)
			}
		})
	}
}

// A project that is in scope but lost its resource-id tag is not a deletion —
// the tag is editable in Horizon by anyone who owns the project, and the stored
// project ID is the second witness that survives it.
func TestReleasedLeafWithAnUntaggedProjectInScopeIsKept(t *testing.T) {
	store := &deletedStore{}
	r := releasedTestReconciler(t, store, Config{})

	var res reconcileResult
	r.removeReleasedLeavesWithoutProject(context.Background(),
		map[string]tree.Node{"p_1": {ID: "p_1", OSProjectID: "os-1"}},
		map[string]osclient.ProjectInfo{"os-1": {ID: "os-1"}}, &res)

	if len(store.deleted) != 0 || res.releasedLeavesRemoved != 0 {
		t.Errorf("deleted %v / counted %d, want nothing", store.deleted, res.releasedLeavesRemoved)
	}
}
