package tree

import (
	"context"
	"testing"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/nodestore"
	"go.uber.org/zap"
)

// A write by the service that happened signals; one that did not — a skipped update, a delete whose predicate failed, a missing node — must not, or every no-op would start a reconciler run. Nor must the reconciler's own reports, or every run would start the next.
func TestChanges(t *testing.T) {
	ctx := context.Background()
	log := zap.NewNop().Sugar()
	svc := NewService(nodestore.NewInMemoryStore(log), nil, []common.Resource{{ID: "cores", Name: "Cores"}}, nil, time.Second, 0, Accounting{}, log)
	sync := svc.ProviderSync("openstack")
	expect := func(step string, want bool) {
		t.Helper()
		select {
		case <-sync.Changes():
			if !want {
				t.Fatalf("%s: signalled a change", step)
			}
		default:
			if want {
				t.Fatalf("%s: no change signalled", step)
			}
		}
	}

	if err := svc.store.UpsertNode(ctx, model.Node{ID: "p1", Kind: model.KindProject, Status: model.StatusPending}); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.UpsertNode(ctx, model.Node{ID: "p2", Kind: model.KindProject, Status: model.StatusPending}); err != nil {
		t.Fatal(err)
	}
	expect("two upserts fold into one", true)
	expect("nothing since", false)

	if _, err := svc.store.UpdateNode(ctx, "p1", func(n *model.Node) error { return nodestore.ErrSkipUpdate }); err != nil {
		t.Fatal(err)
	}
	expect("skipped update", false)

	if _, err := svc.store.UpdateNode(ctx, "p1", func(n *model.Node) error { n.Status = model.StatusApproved; return nil }); err != nil {
		t.Fatal(err)
	}
	expect("update", true)

	if _, err := svc.store.UpdateNode(ctx, "missing", func(n *model.Node) error { return nil }); err != nil {
		t.Fatal(err)
	}
	expect("update of a missing node", false)

	if _, err := svc.store.DeleteNodeIf(ctx, "p1", func(n model.Node) bool { return false }); err != nil {
		t.Fatal(err)
	}
	expect("delete refused by its predicate", false)

	if err := sync.RecordExternalID(ctx, "p1", "os-p1"); err != nil {
		t.Fatal(err)
	}
	expect("the provider's own report", false)

	if _, err := svc.store.DeleteNodeIf(ctx, "p1", func(n model.Node) bool { return true }); err != nil {
		t.Fatal(err)
	}
	expect("delete", true)

	if err := svc.store.DeleteNodes(ctx, nil); err != nil {
		t.Fatal(err)
	}
	expect("delete of nothing", false)
}

// Every provider hears of every change: each view has its own channel.
func TestChanges_EveryProviderHears(t *testing.T) {
	ctx := context.Background()
	log := zap.NewNop().Sugar()
	svc := NewService(nodestore.NewInMemoryStore(log), nil, []common.Resource{{ID: "cores", Name: "Cores"}}, nil, time.Second, 0, Accounting{}, log)
	os, ai := svc.ProviderSync("openstack"), svc.ProviderSync("aiaas")
	if err := svc.store.UpsertNode(ctx, model.Node{ID: "p1", Kind: model.KindProject, Status: model.StatusPending}); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]*ProviderSync{"openstack": os, "aiaas": ai} {
		select {
		case <-p.Changes():
		default:
			t.Errorf("%s did not hear of the change", name)
		}
	}
}

// A deletion request is accepted only where every provider that carries the project out acts on it.
func TestDeletable_PerProvider(t *testing.T) {
	log := zap.NewNop().Sugar()
	svc := NewService(nodestore.NewInMemoryStore(log), nil, []common.Resource{
		{ID: "cores", Provider: "openstack"}, {ID: "llm-mtokens", Provider: "aiaas", Kind: common.KindWindow, Window: "30d"},
	}, nil, time.Second, 0, Accounting{}, log)
	svc.SetDeletingProviders("aiaas")
	ai := model.Node{Limit: common.ProjectQuota{"llm-mtokens": 10}}
	both := model.Node{Limit: common.ProjectQuota{"cores": 2, "llm-mtokens": 10}}
	boundOS := model.Node{Bindings: map[string]model.Binding{"openstack": {ExternalID: "os-1"}}}
	if !svc.deletable(ai) || svc.deletable(both) || svc.deletable(boundOS) {
		t.Errorf("only aiaas deletes: ai %v, both %v, bound to openstack %v", svc.deletable(ai), svc.deletable(both), svc.deletable(boundOS))
	}
	svc.SetDeletingProviders("aiaas", "openstack")
	if !svc.deletable(both) || !svc.deletable(model.Node{}) {
		t.Error("with both deleting, both kinds and an empty project may be deleted")
	}
	svc.SetDeletingProviders()
	if svc.deletable(model.Node{}) {
		t.Error("nobody deletes: nothing may be marked")
	}
}
