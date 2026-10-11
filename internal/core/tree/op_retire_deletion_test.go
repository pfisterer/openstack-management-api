package tree_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/tree"
)

// Deleting for good: the owner, a project admin or a manager may ask, only for
// a released or archived project, and only where deletion is switched on.
func TestRequestDeletion(t *testing.T) {
	svc, _ := newSvc(t, common.TokenList{"group:root"})
	rootTokens := common.TokenList{"user:root@x", "group:root"}
	ownerTokens := common.TokenList{"user:owner@x"}
	strangerTokens := common.TokenList{"user:stranger@x"}

	budget, err := svc.CreateNode(tree.CreateNodeRequest{
		ParentID: model.RootNodeID, Kind: model.KindBudget, Name: "Chair", Reason: "test",
		Limit: cores(10), AdminScope: common.TokenList{"group:root"},
		EligibleRequesters: common.TokenList{"user:owner@x"},
	}, model.UIActor("root@x"), "root@x", rootTokens)
	if err != nil {
		t.Fatal(err)
	}
	newProject := func(name string) model.Node {
		p, err := svc.CreateNode(tree.CreateNodeRequest{
			ParentID: budget.ID, Kind: model.KindProject, Name: name, Reason: "lab", Limit: cores(1),
		}, model.UIActor("owner@x"), "owner@x", ownerTokens)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.ApproveNode(p.ID, tree.ApproveNodeRequest{}, model.UIActor("root@x"), rootTokens); err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := newProject("Lab")

	// Switched off: neither the request nor release-and-delete.
	if _, err := svc.ReleaseNode(p.ID, tree.ReleaseNodeRequest{Delete: true}, model.UIActor("owner@x"), ownerTokens); !errors.Is(err, common.ErrConflict) {
		t.Fatalf("release with delete while switched off: %v, want a conflict", err)
	}
	svc.SetDeletionAllowed(true)

	// Not released yet.
	if _, err := svc.RequestDeletion(p.ID, model.UIActor("owner@x"), ownerTokens); !errors.Is(err, common.ErrConflict) {
		t.Fatalf("deletion of an approved project: %v, want a conflict", err)
	}
	if _, err := svc.ReleaseNode(p.ID, tree.ReleaseNodeRequest{}, model.UIActor("owner@x"), ownerTokens); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RequestDeletion(p.ID, model.UIActor("stranger@x"), strangerTokens); !errors.Is(err, common.ErrForbidden) {
		t.Fatalf("a stranger asked for deletion: %v", err)
	}
	marked, err := svc.RequestDeletion(p.ID, model.UIActor("owner@x"), ownerTokens)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(marked.Flags, model.FlagDeleteRequested) || marked.History[len(marked.History)-1].Event != "deletion_requested" {
		t.Errorf("not marked: flags %v", marked.Flags)
	}
	// Asking twice changes nothing.
	again, err := svc.RequestDeletion(p.ID, model.UIActor("root@x"), rootTokens)
	if err != nil || len(again.History) != len(marked.History) {
		t.Errorf("second request: %v, history %d → %d", err, len(marked.History), len(again.History))
	}

	// Release and delete in one go.
	q := newProject("Lab 2")
	both, err := svc.ReleaseNode(q.ID, tree.ReleaseNodeRequest{Delete: true}, model.UIActor("owner@x"), ownerTokens)
	if err != nil {
		t.Fatal(err)
	}
	if both.Status != model.StatusReleased || !slices.Contains(both.Flags, model.FlagDeleteRequested) {
		t.Errorf("release with delete: status %s, flags %v", both.Status, both.Flags)
	}
}
