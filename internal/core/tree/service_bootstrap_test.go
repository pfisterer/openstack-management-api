package tree

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/nodestore"
)

// staleRootStore hands out the root as it was when the test froze it, the way an earlier read would, and counts the writes that reach the root.
type staleRootStore struct {
	nodestore.Store
	stale       *model.Node
	rootUpdates int
	rootUpserts int
}

func (s *staleRootStore) GetNode(ctx context.Context, id string) (*model.Node, error) {
	if id == model.RootNodeID && s.stale != nil {
		copied := *s.stale
		return &copied, nil
	}
	return s.Store.GetNode(ctx, id)
}

func (s *staleRootStore) UpsertNode(ctx context.Context, n model.Node) error {
	if n.ID == model.RootNodeID {
		s.rootUpserts++
	}
	return s.Store.UpsertNode(ctx, n)
}

func (s *staleRootStore) UpdateNode(ctx context.Context, id string, fn func(n *model.Node) error) (bool, error) {
	wrote, err := s.Store.UpdateNode(ctx, id, fn)
	if id == model.RootNodeID && wrote {
		s.rootUpdates++
	}
	return wrote, err
}

var bootstrapCatalogue = []common.Resource{
	{ID: "cores", Name: "Cores", Provider: "openstack"},
	{ID: "dhbw-ipv4", Name: "DHBW IPv4", Provider: "openstack", Kind: common.KindBool},
	{ID: "ws-storage-gb", Name: "Workspace storage", Provider: "aiaas", Area: common.AreaWorkspaces},
	{ID: "ws-gpu-hours-l4", Name: "GPU hours", Provider: "aiaas", Kind: common.KindWindow, Window: "30d", Area: common.AreaWorkspaces},
}

func bootstrapService(store nodestore.Store, resources []common.Resource) *Service {
	return NewService(store, noRoles{}, resources, common.TokenList{"group:root-admin"}, 5*time.Second, common.DefaultMaxAuthorizedUsers, Accounting{}, zap.NewNop().Sugar())
}

// os21: a quantity of any provider that joins the catalogue of a running deployment is entered at the root as unlimited on the next start. Before, the key was simply missing — the root's dialog read that as "no cap", while below the root nobody was offered the resource until the root had been saved once.
func TestBootstrap_RootGetsNewQuantitiesOfEveryProviderUnlimited(t *testing.T) {
	ctx := context.Background()
	store := &staleRootStore{Store: nodestore.NewInMemoryStore(zap.NewNop().Sugar())}
	if err := bootstrapService(store, bootstrapCatalogue[:1]).Bootstrap(ctx, nil, nil); err != nil {
		t.Fatalf("initial bootstrap: %v", err)
	}
	// An operator capped cores at the root on purpose.
	if _, err := store.Store.UpdateNode(ctx, model.RootNodeID, func(n *model.Node) error { n.Limit = common.ProjectQuota{"cores": 100}; return nil }); err != nil {
		t.Fatal(err)
	}

	if err := bootstrapService(store, bootstrapCatalogue).Bootstrap(ctx, nil, nil); err != nil {
		t.Fatalf("bootstrap after the catalogue grew: %v", err)
	}
	root, _ := store.Store.GetNode(ctx, model.RootNodeID)
	want := common.ProjectQuota{"cores": 100, "dhbw-ipv4": 1, "ws-storage-gb": common.UnlimitedQuota, "ws-gpu-hours-l4": common.UnlimitedQuota}
	for id, v := range want {
		if got, ok := root.Limit[id]; !ok || got != v {
			t.Errorf("root limit %s = %d (present %v), want %d", id, got, ok, v)
		}
	}

	// Below the root the resource is now offered without anyone saving the root first.
	svc := bootstrapService(store, bootstrapCatalogue)
	if err := svc.validateChildBudgetLimit(root, common.ProjectQuota{"ws-storage-gb": 500}); err != nil {
		t.Errorf("the root holds ws-storage-gb but cannot pass it on: %v", err)
	}
}

// Running it again writes nothing: the adoption is a no-op once the root holds every resource.
func TestBootstrap_RootAdoptionIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := &staleRootStore{Store: nodestore.NewInMemoryStore(zap.NewNop().Sugar())}
	if err := bootstrapService(store, bootstrapCatalogue[:1]).Bootstrap(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := bootstrapService(store, bootstrapCatalogue).Bootstrap(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	if store.rootUpdates != 1 {
		t.Fatalf("expected one write adopting the new resources, got %d", store.rootUpdates)
	}
	if err := bootstrapService(store, bootstrapCatalogue).Bootstrap(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	if store.rootUpdates != 1 {
		t.Errorf("a start with nothing new wrote the root again (%d writes)", store.rootUpdates)
	}
}

// The root is changed as it is stored, not as some earlier read saw it: during a rolling update the old pod still serves, and what it wrote to the root in between must survive the new pod's start.
func TestBootstrap_RootSyncDoesNotWriteBackAStaleCopy(t *testing.T) {
	ctx := context.Background()
	store := &staleRootStore{Store: nodestore.NewInMemoryStore(zap.NewNop().Sugar())}
	if err := bootstrapService(store, bootstrapCatalogue[:1]).Bootstrap(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	old, _ := store.Store.GetNode(ctx, model.RootNodeID)
	store.stale = old
	// Changed after the stale read: a description and a cap.
	if _, err := store.Store.UpdateNode(ctx, model.RootNodeID, func(n *model.Node) error {
		n.Reason = "edited meanwhile"
		n.Limit = common.ProjectQuota{"cores": 64}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := bootstrapService(store, bootstrapCatalogue).Bootstrap(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	root, _ := store.Store.GetNode(ctx, model.RootNodeID)
	if root.Reason != "edited meanwhile" || root.Limit["cores"] != 64 {
		t.Errorf("the start wrote back an earlier copy of the root: reason %q, cores %d", root.Reason, root.Limit["cores"])
	}
	if root.Limit["ws-storage-gb"] != common.UnlimitedQuota {
		t.Errorf("ws-storage-gb not adopted: %v", root.Limit)
	}
	if store.rootUpserts != 1 {
		t.Errorf("the root was overwritten whole %d times after its creation", store.rootUpserts-1)
	}
}

// Adopting never edits the map it was handed: it may be shared with whoever read the node before.
func TestAdoptNewCatalogueResources_CopiesTheLimit(t *testing.T) {
	svc := bootstrapService(nodestore.NewInMemoryStore(zap.NewNop().Sugar()), bootstrapCatalogue)
	shared := common.ProjectQuota{"cores": 10}
	root := model.Node{ID: model.RootNodeID, Limit: shared}
	if added := svc.adoptNewCatalogueResources(&root); len(added) != 3 {
		t.Fatalf("added %v", added)
	}
	if len(shared) != 1 {
		t.Errorf("the original map was edited: %v", shared)
	}
}
