package nodestore

import (
	"context"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"go.uber.org/zap"
)

// A write that happened notifies; one that did not — a skipped update, a
// delete whose predicate failed, a missing node — must not, or every no-op
// would start a reconciler run.
func TestNotifyOnWrite(t *testing.T) {
	ctx := context.Background()
	calls := 0
	store := NotifyOnWrite(NewInMemoryStore(zap.NewNop().Sugar()), func() { calls++ })
	expect := func(step string, want int) {
		t.Helper()
		if calls != want {
			t.Fatalf("%s: %d notifications, want %d", step, calls, want)
		}
	}

	if err := store.UpsertNode(ctx, model.Node{ID: "p1", Kind: model.KindProject, Status: model.StatusPending}); err != nil {
		t.Fatal(err)
	}
	expect("upsert", 1)

	if _, err := store.UpdateNode(ctx, "p1", func(n *model.Node) error { return ErrSkipUpdate }); err != nil {
		t.Fatal(err)
	}
	expect("skipped update", 1)

	if _, err := store.UpdateNode(ctx, "p1", func(n *model.Node) error { n.Status = model.StatusApproved; return nil }); err != nil {
		t.Fatal(err)
	}
	expect("update", 2)

	if _, err := store.UpdateNode(ctx, "missing", func(n *model.Node) error { return nil }); err != nil {
		t.Fatal(err)
	}
	expect("update of a missing node", 2)

	if _, err := store.DeleteNodeIf(ctx, "p1", func(n model.Node) bool { return false }); err != nil {
		t.Fatal(err)
	}
	expect("delete refused by its predicate", 2)

	if _, err := store.DeleteNodeIf(ctx, "p1", func(n model.Node) bool { return true }); err != nil {
		t.Fatal(err)
	}
	expect("delete", 3)

	if err := store.DeleteNodes(ctx, nil); err != nil {
		t.Fatal(err)
	}
	expect("delete of nothing", 3)
}
