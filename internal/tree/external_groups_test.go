package tree_test

import (
	"context"
	"errors"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/tree"
)

// A group given access in OpenStack can be taken off a project by whoever looks
// after it, and only by them.
func TestRemoveExternalGroup(t *testing.T) {
	f := newAllocationFixture(t)
	// The fixture's project, with two groups from OpenStack.
	n := mustGet(t, f.svc, f.project.ID)
	n.ExternalGroupAssignments = []common.ExternalGroupAssignment{
		{GroupID: "g1", GroupName: "fakultaet-wi", Role: "member"},
		{GroupID: "g2", GroupName: "team", Role: "reader"},
	}
	if err := f.store.UpsertNode(context.Background(), n); err != nil {
		t.Fatal(err)
	}

	if _, err := f.svc.RemoveExternalGroup(f.project.ID, "g1", tree.UIActor("x@x"), common.TokenList{"user:other@x"}); !errors.Is(err, common.ErrForbidden) {
		t.Errorf("a stranger: want forbidden, got %v", err)
	}
	got, err := f.svc.RemoveExternalGroup(f.project.ID, "g1", tree.UIActor("stud@x"), ownerToken)
	if err != nil {
		t.Fatalf("owner: %v", err)
	}
	if len(got.ExternalGroupAssignments) != 1 || got.ExternalGroupAssignments[0].GroupID != "g2" {
		t.Errorf("left %v, want only g2", got.ExternalGroupAssignments)
	}
	if last := got.History[len(got.History)-1]; last.Event != "external_group_removed" || last.Reason == nil || *last.Reason != "fakultaet-wi" {
		t.Errorf("history %+v", last)
	}
	if _, err := f.svc.RemoveExternalGroup(f.project.ID, "g1", tree.UIActor("mgr@x"), studMgr); !errors.Is(err, common.ErrNotFound) {
		t.Errorf("removing it again: want not found, got %v", err)
	}
	if _, err := f.svc.RemoveExternalGroup(f.project.ID, "g2", tree.UIActor("mgr@x"), studMgr); err != nil {
		t.Errorf("budget manager: %v", err)
	}
}
