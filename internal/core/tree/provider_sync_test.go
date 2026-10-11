package tree

import (
	"context"
	"slices"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"go.uber.org/zap"
)

// A pass that could not read the quota detail must leave the last known usage
// alone. Writing an empty map there is not a cosmetic bug: the accounting bills
// max(limit, in-use), so an erased measurement drops the charge back to the
// declared limit — which is exactly the shrink-after-filling loophole that
// billing the maximum exists to close, reopened by a transient API error.
func TestApplyObservation_UnmeasuredPassKeepsTheLastKnownUsage(t *testing.T) {
	leaf := Node{
		ExternalID:    "os-1",
		Overcommitted: true,
		InUse:         common.ProjectQuota{"cores": 8, "ram": 5},
	}

	changed := ApplyObservation(&leaf, "os-1", nil)

	if changed {
		t.Error("nothing was measured and the project id is unchanged; there is nothing to persist")
	}
	if !quotaEqual(leaf.InUse, common.ProjectQuota{"cores": 8, "ram": 5}) {
		t.Errorf("InUse = %v, want the stored measurement untouched", leaf.InUse)
	}
	if !leaf.Overcommitted {
		t.Error("Overcommitted was cleared by a pass that did not measure anything")
	}
}

// The project id is tracked even when the quota detail was unreadable — it does
// not come from the measurement, so there is no reason to lose it.
func TestApplyObservation_UnmeasuredPassStillAdoptsTheProjectID(t *testing.T) {
	leaf := Node{InUse: common.ProjectQuota{"cores": 8}}

	if !ApplyObservation(&leaf, "os-new", nil) {
		t.Fatal("a new OS project id has to be persisted")
	}
	if leaf.ExternalID != "os-new" {
		t.Errorf("OSProjectID = %q, want %q", leaf.ExternalID, "os-new")
	}
	if !quotaEqual(leaf.InUse, common.ProjectQuota{"cores": 8}) {
		t.Errorf("InUse = %v, want it untouched", leaf.InUse)
	}
}

// A real measurement overwrites, including back down to zero: a project that
// genuinely released its servers must stop being billed for them.
func TestApplyObservation_MeasuredPassOverwrites(t *testing.T) {
	leaf := Node{
		ExternalID:    "os-1",
		Overcommitted: true,
		InUse:         common.ProjectQuota{"cores": 8, "ram": 5},
	}

	if !ApplyObservation(&leaf, "os-1", &Observation{InUse: common.ProjectQuota{"cores": 0, "ram": 0}}) {
		t.Fatal("the measurement changed, so the node needs persisting")
	}
	if !quotaEqual(leaf.InUse, common.ProjectQuota{"cores": 0, "ram": 0}) {
		t.Errorf("InUse = %v, want the fresh measurement", leaf.InUse)
	}
	if leaf.Overcommitted {
		t.Error("Overcommitted should follow the fresh measurement")
	}
}

// An unchanged measurement must not report a change, or the reconciler rewrites
// every leaf on every tick.
func TestApplyObservation_IdenticalMeasurementIsNotAChange(t *testing.T) {
	servers, serverLimit := 1, 2
	leaf := Node{ExternalID: "os-1", InUse: common.ProjectQuota{"cores": 2}, Servers: &servers, ServerLimit: &serverLimit}

	if ApplyObservation(&leaf, "os-1", &Observation{InUse: common.ProjectQuota{"cores": 2}, Servers: 1, ServerLimit: 2}) {
		t.Error("nothing changed, but the node was marked for persisting")
	}
}

// The server count is part of the measurement: written with it, and a change
// in it alone is worth persisting.
func TestApplyObservation_ServerCount(t *testing.T) {
	leaf := Node{ExternalID: "os-1", InUse: common.ProjectQuota{"cores": 2}}

	if !ApplyObservation(&leaf, "os-1", &Observation{InUse: common.ProjectQuota{"cores": 2}, Servers: 3}) {
		t.Fatal("a first server count has to be persisted")
	}
	if leaf.Servers == nil || *leaf.Servers != 3 {
		t.Fatalf("Servers = %v, want 3", leaf.Servers)
	}
	if ApplyObservation(&leaf, "os-1", nil) || *leaf.Servers != 3 {
		t.Error("an unmeasured pass must keep the last count")
	}
}

// An unread use keeps the stored one; a read one replaces it and counts as a change.
func TestApplyObservation_GrantUse(t *testing.T) {
	leaf := Node{ExternalID: "os-1", GrantUse: common.ProjectQuota{"dhbw-ipv4": 1}}
	if ApplyObservation(&leaf, "os-1", &Observation{}) && leaf.GrantUse["dhbw-ipv4"] != 1 {
		t.Errorf("unread use overwrote the stored one: %v", leaf.GrantUse)
	}
	if !ApplyObservation(&leaf, "os-1", &Observation{GrantUse: common.ProjectQuota{"dhbw-ipv4": 0}}) || leaf.GrantUse["dhbw-ipv4"] != 0 {
		t.Errorf("fresh use not applied: %v", leaf.GrantUse)
	}
}

func providerSyncWith(t *testing.T, nodes ...Node) (*ProviderSync, Store) {
	t.Helper()
	store := NewInMemoryStore(zap.NewNop().Sugar())
	for _, n := range nodes {
		if err := store.UpsertNode(context.Background(), n); err != nil {
			t.Fatal(err)
		}
	}
	return NewProviderSync(store), store
}

func nodeIn(t *testing.T, store Store, id string) Node {
	t.Helper()
	n, err := store.GetNode(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return *n
}

// Adopting turns the import into a pending project and adds who joined in
// OpenStack meanwhile; a promotion withdrawn during the pass is left alone.
func TestPromote(t *testing.T) {
	ctx := context.Background()
	imported := Node{ID: "p_1", Kind: KindProject, Status: StatusImported, Owner: "user:owner@x",
		Flags:           []string{FlagPromoteOnReconcile},
		AuthorizedUsers: []common.AuthorizedUser{{Token: "user:a@x", OpenstackRole: "reader"}}}
	withdrawn := Node{ID: "p_2", Kind: KindProject, Status: StatusImported}
	p, store := providerSyncWith(t, imported, withdrawn)

	current := []common.AuthorizedUser{{Token: "user:a@x", OpenstackRole: "member"}, {Token: "user:new@x", OpenstackRole: "member"}, {Token: "user:owner@x", OpenstackRole: "member"}}
	if ok, err := p.Promote(ctx, "p_1", current); err != nil || !ok {
		t.Fatalf("promote: %v %v", ok, err)
	}
	n := nodeIn(t, store, "p_1")
	if n.Status != StatusPending || slices.Contains(n.Flags, FlagPromoteOnReconcile) || len(n.AuthorizedUsers) != 2 || n.AuthorizedUsers[0].OpenstackRole != "reader" {
		t.Errorf("promoted node: %+v", n)
	}
	if ok, _ := p.Promote(ctx, "p_2", current); ok || nodeIn(t, store, "p_2").Status != StatusImported {
		t.Error("a node without the promotion flag was promoted")
	}
}

// Refreshing an import leaves a node alone that is being adopted or already was.
func TestRefreshImportLeavesAPromotionAlone(t *testing.T) {
	ctx := context.Background()
	p, store := providerSyncWith(t,
		Node{ID: "p_1", Kind: KindProject, Status: StatusImported, Name: "old", Flags: []string{FlagPromoteOnReconcile}},
		Node{ID: "p_2", Kind: KindProject, Status: StatusPending, Name: "old"},
		Node{ID: "p_3", Kind: KindProject, Status: StatusImported, Name: "old"})
	for _, id := range []string{"p_1", "p_2", "p_3"} {
		if err := p.RefreshImport(ctx, Node{ID: id, Name: "new", ExternalID: "os-" + id}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if nodeIn(t, store, "p_1").Name != "old" || nodeIn(t, store, "p_2").Name != "old" {
		t.Error("a node being adopted or adopted was refreshed as an import")
	}
	if n := nodeIn(t, store, "p_3"); n.Name != "new" || n.ExternalID != "os-p_3" {
		t.Errorf("import not refreshed: %+v", n)
	}
}

// Only a retired leaf is removed when its project is gone, only an import as an import.
func TestRemoveGuards(t *testing.T) {
	ctx := context.Background()
	p, _ := providerSyncWith(t,
		Node{ID: "p_rel", Kind: KindProject, Status: StatusReleased},
		Node{ID: "p_app", Kind: KindProject, Status: StatusApproved},
		Node{ID: "p_imp", Kind: KindProject, Status: StatusImported})
	if ok, _ := p.RemoveRetired(ctx, "p_app"); ok {
		t.Error("an approved project was removed as retired")
	}
	if ok, _ := p.RemoveRetired(ctx, "p_rel"); !ok {
		t.Error("a released project was kept")
	}
	if ok, _ := p.RemoveImport(ctx, "p_app"); ok {
		t.Error("a managed project was removed as an import")
	}
	if ok, _ := p.RemoveImport(ctx, "p_imp"); !ok {
		t.Error("an import was kept")
	}
}
