package tree

import (
	"context"
	"slices"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/nodestore"
	"go.uber.org/zap"
)

// A pass that could not read the quota detail must leave the last known usage
// alone. Writing an empty map there is not a cosmetic bug: the accounting bills
// max(limit, in-use), so an erased measurement drops the charge back to the
// declared limit — which is exactly the shrink-after-filling loophole that
// billing the maximum exists to close, reopened by a transient API error.
func TestApplyObservation_UnmeasuredPassKeepsTheLastKnownUsage(t *testing.T) {
	leaf := model.Binding{
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
	leaf := model.Binding{InUse: common.ProjectQuota{"cores": 8}}

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
	leaf := model.Binding{
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
	leaf := model.Binding{ExternalID: "os-1", InUse: common.ProjectQuota{"cores": 2}, Facts: map[string]int{model.FactServers: 1, model.FactServerLimit: 2}}

	if ApplyObservation(&leaf, "os-1", &Observation{InUse: common.ProjectQuota{"cores": 2}, Facts: map[string]int{model.FactServers: 1, model.FactServerLimit: 2}}) {
		t.Error("nothing changed, but the node was marked for persisting")
	}
}

// The server count is part of the measurement: written with it, and a change
// in it alone is worth persisting.
func TestApplyObservation_ServerCount(t *testing.T) {
	leaf := model.Binding{ExternalID: "os-1", InUse: common.ProjectQuota{"cores": 2}}

	if !ApplyObservation(&leaf, "os-1", &Observation{InUse: common.ProjectQuota{"cores": 2}, Facts: map[string]int{model.FactServers: 3}}) {
		t.Fatal("a first server count has to be persisted")
	}
	if leaf.Facts[model.FactServers] != 3 {
		t.Fatalf("facts = %v, want 3 servers", leaf.Facts)
	}
	if ApplyObservation(&leaf, "os-1", nil) || leaf.Facts[model.FactServers] != 3 {
		t.Error("an unmeasured pass must keep the last count")
	}
}

// An unread use keeps the stored one; a read one replaces it and counts as a change.
func TestApplyObservation_GrantUse(t *testing.T) {
	leaf := model.Binding{ExternalID: "os-1", GrantUse: common.ProjectQuota{"dhbw-ipv4": 1}}
	if ApplyObservation(&leaf, "os-1", &Observation{}) && leaf.GrantUse["dhbw-ipv4"] != 1 {
		t.Errorf("unread use overwrote the stored one: %v", leaf.GrantUse)
	}
	if !ApplyObservation(&leaf, "os-1", &Observation{GrantUse: common.ProjectQuota{"dhbw-ipv4": 0}}) || leaf.GrantUse["dhbw-ipv4"] != 0 {
		t.Errorf("fresh use not applied: %v", leaf.GrantUse)
	}
}

func providerSyncWith(t *testing.T, nodes ...model.Node) (*ProviderSync, nodestore.Store) {
	t.Helper()
	store := nodestore.NewInMemoryStore(zap.NewNop().Sugar())
	for _, n := range nodes {
		if err := store.UpsertNode(context.Background(), n); err != nil {
			t.Fatal(err)
		}
	}
	return NewProviderSync(store, "openstack"), store
}

func nodeIn(t *testing.T, store nodestore.Store, id string) model.Node {
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
	imported := model.Node{ID: "p_1", Kind: model.KindProject, Status: model.StatusImported, Owner: "user:owner@x",
		Flags:           []string{model.FlagPromoteOnReconcile},
		AuthorizedUsers: []common.AuthorizedUser{{Token: "user:a@x", OpenstackRole: "reader"}}}
	withdrawn := model.Node{ID: "p_2", Kind: model.KindProject, Status: model.StatusImported}
	p, store := providerSyncWith(t, imported, withdrawn)

	current := []common.AuthorizedUser{{Token: "user:a@x", OpenstackRole: "member"}, {Token: "user:new@x", OpenstackRole: "member"}, {Token: "user:owner@x", OpenstackRole: "member"}}
	if ok, err := p.Promote(ctx, "p_1", current); err != nil || !ok {
		t.Fatalf("promote: %v %v", ok, err)
	}
	n := nodeIn(t, store, "p_1")
	if n.Status != model.StatusPending || slices.Contains(n.Flags, model.FlagPromoteOnReconcile) || len(n.AuthorizedUsers) != 2 || n.AuthorizedUsers[0].OpenstackRole != "reader" {
		t.Errorf("promoted node: %+v", n)
	}
	if ok, _ := p.Promote(ctx, "p_2", current); ok || nodeIn(t, store, "p_2").Status != model.StatusImported {
		t.Error("a node without the promotion flag was promoted")
	}
}

// Refreshing an import leaves a node alone that is being adopted or already was.
func TestRefreshImportLeavesAPromotionAlone(t *testing.T) {
	ctx := context.Background()
	p, store := providerSyncWith(t,
		model.Node{ID: "p_1", Kind: model.KindProject, Status: model.StatusImported, Name: "old", Flags: []string{model.FlagPromoteOnReconcile}},
		model.Node{ID: "p_2", Kind: model.KindProject, Status: model.StatusPending, Name: "old"},
		model.Node{ID: "p_3", Kind: model.KindProject, Status: model.StatusImported, Name: "old"})
	for _, id := range []string{"p_1", "p_2", "p_3"} {
		if err := p.RefreshImport(ctx, model.Node{ID: id, Name: "new", Bindings: map[string]model.Binding{"openstack": {ExternalID: "os-" + id}}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if nodeIn(t, store, "p_1").Name != "old" || nodeIn(t, store, "p_2").Name != "old" {
		t.Error("a node being adopted or adopted was refreshed as an import")
	}
	if n := nodeIn(t, store, "p_3"); n.Name != "new" || n.Binding("openstack").ExternalID != "os-p_3" {
		t.Errorf("import not refreshed: %+v", n)
	}
}

// Only a retired leaf is removed when its project is gone, only an import as an import.
func TestRemoveGuards(t *testing.T) {
	ctx := context.Background()
	p, _ := providerSyncWith(t,
		model.Node{ID: "p_rel", Kind: model.KindProject, Status: model.StatusReleased},
		model.Node{ID: "p_app", Kind: model.KindProject, Status: model.StatusApproved},
		model.Node{ID: "p_imp", Kind: model.KindProject, Status: model.StatusImported})
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

// A provider writes only its own side of a project: what another provider reported stays as it was.
func TestProviderSync_WritesOnlyItsOwnBinding(t *testing.T) {
	ctx := context.Background()
	theirs := model.Binding{ExternalID: "ws-1", InUse: common.ProjectQuota{"gpu_hours": 3}}
	p, store := providerSyncWith(t, model.Node{ID: "p_1", Kind: model.KindProject, Status: model.StatusApproved,
		Bindings: map[string]model.Binding{"workspaces": theirs}})

	if err := p.RecordExternalID(ctx, "p_1", "os-1"); err != nil {
		t.Fatal(err)
	}
	if err := p.RecordObservation(ctx, "p_1", "os-1", &Observation{InUse: common.ProjectQuota{"cores": 2}, Facts: map[string]int{model.FactServers: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := p.RecordPurgeProgress(ctx, "p_1", PurgeReport{Step: "servers", Steps: 3}); err != nil {
		t.Fatal(err)
	}
	n := nodeIn(t, store, "p_1")
	if os := n.Binding("openstack"); os.ExternalID != "os-1" || os.InUse["cores"] != 2 || os.Purge == nil {
		t.Errorf("openstack binding = %+v", os)
	}
	if ws := n.Binding("workspaces"); ws.ExternalID != "ws-1" || !quotaEqual(ws.InUse, theirs.InUse) || ws.Purge != nil {
		t.Errorf("the other provider's binding changed: %+v", ws)
	}
	if got := n.InUse(); got["cores"] != 2 || got["gpu_hours"] != 3 {
		t.Errorf("InUse across providers = %v, want both", got)
	}
}

// A provider sees the leaves it carries out and those holding one of its resources — not a project that only has another provider's.
func TestProviderSync_LeavesOfAProvider(t *testing.T) {
	ctx := context.Background()
	catalog := []common.Resource{
		{ID: "cores", Provider: "openstack"},
		{ID: "gpu_hours", Provider: "workspaces", Kind: common.KindWindow, Window: "30d"},
		{ID: "legacy"}, // no provider: everybody's
	}
	leaf := func(id string, limit common.ProjectQuota, bound ...string) model.Node {
		n := model.Node{ID: id, Kind: model.KindProject, Status: model.StatusApproved, Limit: limit}
		for _, p := range bound {
			n.EditBinding(p, func(b *model.Binding) { b.ExternalID = p + "-" + id })
		}
		return n
	}
	_, store := providerSyncWith(t,
		leaf("vm", common.ProjectQuota{"cores": 4}),
		leaf("ws", common.ProjectQuota{"gpu_hours": 100, "cores": 0}),
		leaf("both", common.ProjectQuota{"cores": 2, "gpu_hours": 10}),
		leaf("emptied", common.ProjectQuota{"cores": 0}, "openstack"),
		leaf("unlimited", common.ProjectQuota{"cores": common.UnlimitedQuota}),
		leaf("old", common.ProjectQuota{"legacy": 1}),
		leaf("nothing", common.ProjectQuota{}),
	)
	ids := func(provider string) []string {
		p := &ProviderSync{store: store, provider: provider, resources: func() []common.Resource { return catalog }}
		leaves, err := p.Leaves(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, n := range leaves {
			out = append(out, n.ID)
		}
		slices.Sort(out)
		return out
	}
	if got, want := ids("openstack"), []string{"both", "emptied", "old", "unlimited", "vm"}; !slices.Equal(got, want) {
		t.Errorf("openstack sees %v, want %v", got, want)
	}
	if got, want := ids("workspaces"), []string{"both", "old", "ws"}; !slices.Equal(got, want) {
		t.Errorf("workspaces sees %v, want %v", got, want)
	}
}
