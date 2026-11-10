package tree_test

import (
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/tree"
)

// An empty edit is refused before anything else, for anybody: it used to pass without a rights check and leave an empty history entry (10.11.2026).
func TestUpdateNode_EmptyIsRefused(t *testing.T) {
	f := newChangeFixture(t, nil, nil)
	before := len(f.budget.History)
	if _, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{}, model.UIActor("stranger@x"), nil); err == nil {
		t.Fatal("an empty edit must be refused")
	}
	n, err := f.svc.GetNode(f.budget.ID, rootTokens)
	if err != nil {
		t.Fatal(err)
	}
	if len(n.History) != before {
		t.Fatalf("an empty edit must not touch the history, %d → %d entries", before, len(n.History))
	}
}
