package reconciler

import (
	"context"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	osclient "github.com/pfisterer/openstack-management-api/internal/openstack/client"
	"github.com/pfisterer/openstack-management-api/internal/tree"
	"go.uber.org/zap"
)

// A pass loads its leaves once and then spends seconds per leaf in OpenStack.
// These tests change a node in the store AFTER the pass's copy was taken — as a
// user would through the API — and check that the pass's write keeps that change.

func staleWriteReconciler(t *testing.T) (*Reconciler, *tree.InMemoryStore) {
	t.Helper()
	log := zap.NewNop().Sugar()
	store := tree.NewInMemoryStore(log)
	return &Reconciler{store: store, log: log}, store
}

func TestOSSyncStateDoesNotRevertAChangeMadeDuringThePass(t *testing.T) {
	r, store := staleWriteReconciler(t)
	ctx := context.Background()
	parent := "b_1"

	// What the pass loaded: an approved leaf.
	loaded := tree.Node{ID: "p_1", Kind: tree.KindProject, ParentID: &parent, Status: tree.StatusApproved,
		Limit: common.ProjectQuota{"cores": 2}}
	// What the user did meanwhile: filed a change request.
	now := loaded
	now.Status = tree.StatusChangePending
	now.Pending = &tree.PendingChanges{Limit: &common.ProjectQuota{"cores": 8}}
	if err := store.UpsertNode(ctx, now); err != nil {
		t.Fatal(err)
	}

	r.persistOSSyncState(ctx, loaded.ID, "os-1", true, common.ProjectQuota{"cores": 3}, true)

	got, _ := store.GetNode(ctx, "p_1")
	if got.Status != tree.StatusChangePending || got.Pending == nil || got.Pending.Limit == nil || (*got.Pending.Limit)["cores"] != 8 {
		t.Errorf("the change request was lost: status %q, pending %+v", got.Status, got.Pending)
	}
	if got.OSProjectID != "os-1" || !got.OSOvercommitted || got.OSInUse["cores"] != 3 {
		t.Errorf("the measured OpenStack state was not written: %+v", got)
	}
}

func TestOSProjectIDIsWrittenOntoTheCurrentNode(t *testing.T) {
	r, store := staleWriteReconciler(t)
	ctx := context.Background()
	parent := "b_1"

	// Released while the pass was creating its project: the release stays, and
	// the ID is recorded so the project can still be cleaned up.
	if err := store.UpsertNode(ctx, tree.Node{ID: "p_1", Kind: tree.KindProject, ParentID: &parent, Status: tree.StatusReleased}); err != nil {
		t.Fatal(err)
	}

	r.persistOSProjectID(ctx, "p_1", "os-new")

	got, _ := store.GetNode(ctx, "p_1")
	if got.Status != tree.StatusReleased || got.OSProjectID != "os-new" {
		t.Errorf("got status %q / os id %q, want released / os-new", got.Status, got.OSProjectID)
	}
}

func TestStaleImportIsNotDeletedOnceItWasPromoted(t *testing.T) {
	r, store := staleWriteReconciler(t)
	ctx := context.Background()
	parent := "b_1"

	// The pass loaded an import whose project left the scope; in the meantime an
	// admin promoted it into the managed tree.
	loaded := tree.Node{ID: "p_imp", Kind: tree.KindProject, ParentID: &parent, Status: tree.StatusImported, OSProjectID: "os-gone"}
	now := loaded
	now.Status = tree.StatusPending
	if err := store.UpsertNode(ctx, now); err != nil {
		t.Fatal(err)
	}

	var res reconcileResult
	imported := map[string]tree.Node{"os-gone": loaded}
	r.removeStaleImports(ctx, imported, map[string]osclient.ProjectInfo{}, &res)

	if got, _ := store.GetNode(ctx, "p_imp"); got == nil {
		t.Fatal("a promoted leaf was deleted as a stale import")
	}
	if res.importedRemoved != 0 {
		t.Errorf("counted %d removals, want 0", res.importedRemoved)
	}
}
