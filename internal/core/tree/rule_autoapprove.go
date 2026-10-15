package tree

import (
	"context"
	"fmt"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
)

// validateAutoApprove checks the per-requester limit of an auto-approve policy.
// An empty limit is valid: it is the pool flavour, bounded by the budget alone.
func (s *Service) validateAutoApprove(a *model.AutoApprove) error {
	if a == nil || a.IsPool() {
		return nil
	}
	return s.validateRequesterLimit(a.PerRequesterLimit)
}

// autoApprovable reports whether budget's auto-approve policy grants a leaf of
// owner going from subtractLimit (what it holds now; nil for a new leaf) to
// addLimit: within the per-requester limit, if the policy has one, and within
// the capacity of every budget up the chain.
func (s *Service) autoApprovable(ctx context.Context, budget *model.Node, owner string, addLimit, subtractLimit common.ProjectQuota) (bool, error) {
	if budget == nil || budget.AutoApprove == nil || budget.Status != model.StatusApproved {
		return false, nil
	}
	if !budget.AutoApprove.IsPool() {
		usage, err := s.ownerActiveUsage(ctx, budget.ID, owner)
		if err != nil {
			return false, fmt.Errorf("compute per-requester usage: %w", err)
		}
		for _, id := range s.countIDs() {
			usage[id] -= subtractLimit[id]
		}
		need, err := quotaAddChecked(usage, addLimit, s.countIDs())
		if err != nil || !quotaFits(need, budget.AutoApprove.PerRequesterLimit, s.countIDs()) {
			return false, nil
		}
	}
	ancestors, err := s.nodeChain(ctx, budget.ID)
	if err != nil {
		return false, err
	}
	return s.checkCapacity(ctx, ancestors, addLimit, subtractLimit) == nil, nil
}

// autoApproveReason is the history note of a grant made by budget's policy.
func autoApproveReason(budget *model.Node) string {
	if budget.AutoApprove.IsPool() {
		return "Auto-approved (the budget has room)"
	}
	return "Auto-approved (within per-requester limit)"
}

// autoApprovedEntry records an approval nobody had to give.
func autoApprovedEntry(actor model.Actor, statusFrom, reason string) model.HistoryEntry {
	entry := model.NewHistoryEntry("approved", model.Actor{Email: "system:auto-approval", Via: actor.Channel()}, model.StatusApproved)
	entry.StatusFrom = new(statusFrom)
	entry.Reason = new(reason)
	return entry
}
