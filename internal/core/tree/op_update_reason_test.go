package tree_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/tree"
)

// A budget's purpose can be edited by its managers; the edit is trimmed and recorded in the history with what it was before.
func TestUpdateNode_BudgetPurpose(t *testing.T) {
	f := newChangeFixture(t, nil, nil)

	updated, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{Reason: ptr("  Teaching in winter term  ")}, rootActor(), rootTokens)
	if err != nil {
		t.Fatalf("edit purpose: %v", err)
	}
	if updated.Reason != "Teaching in winter term" {
		t.Fatalf("purpose = %q, want it trimmed", updated.Reason)
	}
	last := updated.History[len(updated.History)-1]
	if last.Event != "updated" || last.ReasonFrom == nil || *last.ReasonFrom != "t" || last.ReasonTo == nil || *last.ReasonTo != "Teaching in winter term" {
		t.Fatalf("history should record the purpose from/to, got %+v", last)
	}

	// The same purpose again changes nothing worth recording.
	again, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{Reason: ptr("Teaching in winter term")}, rootActor(), rootTokens)
	if err != nil {
		t.Fatalf("edit purpose again: %v", err)
	}
	if last := again.History[len(again.History)-1]; last.ReasonFrom != nil || last.ReasonTo != nil {
		t.Fatalf("an unchanged purpose should not be recorded as a change, got %+v", last)
	}
}

// Only a manager of the budget or above may change its purpose, like every other policy field.
func TestUpdateNode_BudgetPurposeNeedsManager(t *testing.T) {
	f := newChangeFixture(t, nil, nil)
	if _, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{Reason: ptr("mine now")}, model.UIActor("stud@x"), studTokens); !errors.Is(err, common.ErrForbidden) {
		t.Fatalf("an eligible requester should not edit the purpose, got %v", err)
	}
}

// The purpose obeys the rules of creation: required, and at most common.MaxReasonLength characters.
func TestUpdateNode_BudgetPurposeValidated(t *testing.T) {
	f := newChangeFixture(t, nil, nil)
	if _, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{Reason: ptr("   ")}, rootActor(), rootTokens); err == nil || !strings.Contains(err.Error(), "purpose is required") {
		t.Fatalf("an empty purpose should be refused, got %v", err)
	}
	long := strings.Repeat("x", common.MaxReasonLength+1)
	if _, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{Reason: &long}, rootActor(), rootTokens); err == nil {
		t.Fatal("a purpose longer than the limit should be refused")
	}
	if _, err := f.svc.UpdateNode(f.budget.ID, tree.UpdateNodeRequest{Reason: new(strings.Repeat("x", common.MaxReasonLength))}, rootActor(), rootTokens); err != nil {
		t.Fatalf("a purpose of exactly the limit should pass: %v", err)
	}
}

// A project's purpose is not a direct edit: its owner cannot change it through UpdateNode.
func TestUpdateNode_ProjectPurposeRefused(t *testing.T) {
	f := newChangeFixture(t, &model.AutoApprove{}, nil)
	p := f.approved(t, 1, nil)
	if _, err := f.svc.UpdateNode(p.ID, tree.UpdateNodeRequest{Reason: ptr("other")}, model.UIActor("stud@x"), studTokens); err == nil || !strings.Contains(err.Error(), "only the name and the admins") {
		t.Fatalf("a project's purpose should not be a direct edit, got %v", err)
	}
}
