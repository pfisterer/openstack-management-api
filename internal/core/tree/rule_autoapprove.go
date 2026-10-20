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
	if ok, err := s.withinRequesterShare(ctx, budget, owner, addLimit, subtractLimit); err != nil || !ok {
		return false, err
	}
	ancestors, err := s.nodeChain(ctx, budget.ID)
	if err != nil {
		return false, err
	}
	return s.checkCapacity(ctx, ancestors, addLimit, subtractLimit) == nil, nil
}

// subBudgetAutoApprovable reports whether budget's auto-approve policy grants a sub-budget requested by requester (an owner token). Budget limits are caps, not reservations: a sub-budget consumes nothing of the budgets above, and the projects in it are held to every ancestor's capacity when they are granted (checkCapacity). So a pool policy grants any sub-budget the edge rule allows, while a per-requester policy counts its full limit against the requester's share (see ownerActiveUsage). Who manages it is free — handing it to others is the point of a sub-budget —, it still counts against the one who asked. Not granted: a request for nothing at all, as with projects, one the edge rule refuses (checkBudgetLimit), and under a per-requester policy a limit with no finite size, which no share can measure.
func (s *Service) subBudgetAutoApprovable(ctx context.Context, budget *model.Node, chain []model.Node, node model.Node, requester string) (bool, error) {
	if budget == nil || budget.AutoApprove == nil || budget.Status != model.StatusApproved {
		return false, nil
	}
	if node.InheritsLimit || allZero(node.Limit) {
		return false, nil
	}
	if err := s.checkBudgetLimit(ctx, node, chain, node.Limit, false); err != nil {
		return false, nil
	}
	if !budget.AutoApprove.IsPool() {
		for _, id := range s.countIDs() {
			if node.Limit[id] == common.UnlimitedQuota {
				return false, nil
			}
		}
	}
	return s.withinRequesterShare(ctx, budget, requester, node.Limit, nil)
}

// withinRequesterShare reports whether owner, going from subtractLimit to addLimit, stays within budget's per-requester limit; a pool policy has none.
func (s *Service) withinRequesterShare(ctx context.Context, budget *model.Node, owner string, addLimit, subtractLimit common.ProjectQuota) (bool, error) {
	if budget.AutoApprove.IsPool() {
		return true, nil
	}
	usage, bounded, err := s.ownerActiveUsage(ctx, budget.ID, owner)
	if err != nil {
		return false, fmt.Errorf("compute per-requester usage: %w", err)
	}
	if !bounded {
		return false, nil
	}
	for _, id := range s.countIDs() {
		usage[id] -= subtractLimit[id]
	}
	need, err := quotaAddChecked(usage, addLimit, s.countIDs())
	if err != nil || !quotaFits(need, budget.AutoApprove.PerRequesterLimit, s.countIDs()) {
		return false, nil
	}
	return true, nil
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
