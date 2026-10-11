package reconciler

import (
	"context"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/nodestore"
	"github.com/pfisterer/openstack-management-api/internal/core/tree"
	osclient "github.com/pfisterer/openstack-management-api/internal/provider/openstack/client"
	"go.uber.org/zap"
)

// A pass loads its leaves once and then spends seconds per leaf in OpenStack.
// These tests change a node in the store AFTER the pass's copy was taken — as a
// user would through the API — and check that the pass's write keeps that change.

func staleWriteReconciler(t *testing.T) (*Reconciler, *nodestore.InMemoryStore) {
	t.Helper()
	log := zap.NewNop().Sugar()
	store := nodestore.NewInMemoryStore(log)
	return &Reconciler{nodes: tree.NewProviderSync(store, osclient.Provider), log: log}, store
}

func TestOSSyncStateDoesNotRevertAChangeMadeDuringThePass(t *testing.T) {
	r, store := staleWriteReconciler(t)
	ctx := context.Background()
	parent := "b_1"

	// What the pass loaded: an approved leaf.
	loaded := model.Node{ID: "p_1", Kind: model.KindProject, ParentID: &parent, Status: model.StatusApproved,
		Limit: common.ProjectQuota{"cores": 2}}
	// What the user did meanwhile: filed a change request.
	now := loaded
	now.Status = model.StatusChangePending
	now.Pending = &model.PendingChanges{Limit: &common.ProjectQuota{"cores": 8}}
	if err := store.UpsertNode(ctx, now); err != nil {
		t.Fatal(err)
	}

	r.persistOSSyncState(ctx, loaded.ID, "os-1", &tree.Observation{Overcommitted: true, InUse: common.ProjectQuota{"cores": 3}})

	got, _ := store.GetNode(ctx, "p_1")
	if got.Status != model.StatusChangePending || got.Pending == nil || got.Pending.Limit == nil || (*got.Pending.Limit)["cores"] != 8 {
		t.Errorf("the change request was lost: status %q, pending %+v", got.Status, got.Pending)
	}
	if b := got.Binding(osclient.Provider); b.ExternalID != "os-1" || !b.Overcommitted || b.InUse["cores"] != 3 {
		t.Errorf("the measured OpenStack state was not written: %+v", got)
	}
}

func TestOSProjectIDIsWrittenOntoTheCurrentNode(t *testing.T) {
	r, store := staleWriteReconciler(t)
	ctx := context.Background()
	parent := "b_1"

	// Released while the pass was creating its project: the release stays, and
	// the ID is recorded so the project can still be cleaned up.
	if err := store.UpsertNode(ctx, model.Node{ID: "p_1", Kind: model.KindProject, ParentID: &parent, Status: model.StatusReleased}); err != nil {
		t.Fatal(err)
	}

	r.persistOSProjectID(ctx, "p_1", "os-new")

	got, _ := store.GetNode(ctx, "p_1")
	if got.Status != model.StatusReleased || osID(*got) != "os-new" {
		t.Errorf("got status %q / os id %q, want released / os-new", got.Status, osID(*got))
	}
}

func TestStaleImportIsNotDeletedOnceItWasPromoted(t *testing.T) {
	r, store := staleWriteReconciler(t)
	ctx := context.Background()
	parent := "b_1"

	// The pass loaded an import whose project left the scope; in the meantime an
	// admin promoted it into the managed tree.
	loaded := model.Node{ID: "p_imp", Kind: model.KindProject, ParentID: &parent, Status: model.StatusImported, Bindings: boundTo("os-gone")}
	now := loaded
	now.Status = model.StatusPending
	if err := store.UpsertNode(ctx, now); err != nil {
		t.Fatal(err)
	}

	var res reconcileResult
	imported := map[string]model.Node{"os-gone": loaded}
	r.removeStaleImports(ctx, imported, map[string]osclient.ProjectInfo{}, &res)

	if got, _ := store.GetNode(ctx, "p_imp"); got == nil {
		t.Fatal("a promoted leaf was deleted as a stale import")
	}
	if res.importedRemoved != 0 {
		t.Errorf("counted %d removals, want 0", res.importedRemoved)
	}
}
