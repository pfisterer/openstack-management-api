package tree

import (
	"cmp"
	"maps"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/nodestore"
)

// The automatic split of a project request over budgets (decided 25.11.2026, (1)): first what needs nobody's decision, from the most specific budget first, then the rest as a request. The split is shown before the request is sent and can be changed there; the request then names its allocations (CreateNodeRequest.Allocations).

// SplitRequest asks how a project of Limit would be paid for.
type SplitRequest struct {
	Limit           common.ProjectQuota `json:"limit" binding:"required"`
	TerminationDate *string             `json:"termination_date"`
	// BudgetIDs restricts the budgets considered; empty considers every budget the caller may request from.
	BudgetIDs []string `json:"budget_ids"`
}

// SplitPart is one allocation of a proposed split.
type SplitPart struct {
	BudgetID   string              `json:"budget_id"`
	BudgetName string              `json:"budget_name"`
	BudgetPath []model.PathEntry   `json:"budget_path,omitempty"`
	Limit      common.ProjectQuota `json:"limit"`
	// AutoApproved is the part the budget grants on the spot; the rest of Limit is asked for and waits for its managers.
	AutoApproved common.ProjectQuota `json:"auto_approved"`
	// TerminationDate is the allocation's end: the one asked for, or the latest the budget allows where that is earlier.
	TerminationDate *string `json:"termination_date,omitempty"`
}

// SplitPlan is a proposed split. Unplaced is what no budget would even take as a request; empty when everything is placed.
type SplitPlan struct {
	Parts    []SplitPart         `json:"parts"`
	Unplaced common.ProjectQuota `json:"unplaced,omitempty"`
}

// candidate is a budget considered for a split, with what decides about it.
type candidate struct {
	budget  model.Node
	chain   []model.Node // from the budget upward
	manager bool         // the caller manages it: whatever is placed there is granted at once
}

// PlanSplit proposes how a project of req.Limit would be paid for by the caller's budgets (planSplit).
func (s *Service) PlanSplit(req SplitRequest, userEmail string, userTokens common.TokenList) (SplitPlan, error) {
	if strings.TrimSpace(userEmail) == "" || len(userTokens) == 0 {
		return SplitPlan{}, errNoPermission
	}
	end, err := normalizeEnd(req.TerminationDate)
	if err != nil {
		return SplitPlan{}, err
	}
	if err := s.validateLeafLimit(req.Limit); err != nil {
		return SplitPlan{}, err
	}
	owner, err := normalizeOwnerToken(userEmail)
	if err != nil {
		return SplitPlan{}, err
	}
	ctx, cancel := s.newCtx()
	defer cancel()
	plan, err := s.planSplit(ctx, req.Limit, end, req.BudgetIDs, owner, userTokens)
	if err != nil {
		return SplitPlan{}, err
	}
	return s.nameSplit(ctx, plan)
}

// candidates are the approved budgets the caller may request from (or manages), restricted to ids when given, most specific first: deepest in the tree, then by name.
func (s *Service) candidates(ctx context.Context, ids []string, userTokens common.TokenList) ([]candidate, error) {
	var budgets []model.Node
	var err error
	if len(ids) > 0 {
		budgets, err = s.store.ListNodes(ctx, nodestore.NodeQuery{IDs: ids, Kinds: []string{model.KindBudget}}, 0, 0)
	} else {
		budgets, err = s.store.ListNodes(ctx, nodestore.NodeQuery{Kinds: []string{model.KindBudget}, Statuses: []string{model.StatusApproved}, EligibleAny: userTokens}, 0, 0)
	}
	if err != nil {
		return nil, fmt.Errorf("load budgets: %w", err)
	}
	caller := common.NewTokenSet(userTokens)
	var out []candidate
	for _, b := range budgets {
		if b.Status != model.StatusApproved || b.ID == model.UnassignedNodeID {
			continue
		}
		chain, err := s.nodeChain(ctx, b.ID)
		if err != nil {
			return nil, err
		}
		if !isEligibleRequester(userTokens, &b) && !managesInChain(caller, chain) {
			if len(ids) > 0 {
				return nil, refuse(common.ErrForbidden, "Aus dem Budget %q lässt sich nichts beantragen.", "Nothing can be requested from the budget %q.", nodeLabel(b))
			}
			continue
		}
		out = append(out, candidate{budget: b, chain: chain, manager: managesInChain(caller, chain)})
	}
	slices.SortStableFunc(out, func(a, b candidate) int {
		return cmp.Or(cmp.Compare(len(b.chain), len(a.chain)), cmp.Compare(a.budget.Name, b.budget.Name), cmp.Compare(a.budget.ID, b.budget.ID))
	})
	return out, nil
}

// planSplit is the split itself. Each budget, most specific first, gives what its auto-approve policy grants of what is still open (autoGrantable); what is left goes as a request to the most specific budget that takes requests beyond its policy and holds every availability of it — onto the part that budget grants already, where it has one. An availability is placed once, with the first budget that grants it.
func (s *Service) planSplit(ctx context.Context, want common.ProjectQuota, end *string, ids []string, owner string, userTokens common.TokenList) (SplitPlan, error) {
	cands, err := s.candidates(ctx, ids, userTokens)
	if err != nil {
		return SplitPlan{}, err
	}
	open := common.ProjectQuota{}
	for k, v := range want {
		if v != 0 {
			open[k] = v
		}
	}
	var plan SplitPlan
	parts := map[string]*SplitPart{}
	var order []string
	add := func(c candidate, granted, asked common.ProjectQuota) {
		p, ok := parts[c.budget.ID]
		if !ok {
			fitted, _ := s.fitSplitEnd(end, c.chain)
			p = &SplitPart{BudgetID: c.budget.ID, BudgetName: nodeLabel(c.budget), Limit: common.ProjectQuota{}, AutoApproved: common.ProjectQuota{}, TerminationDate: fitted}
			parts[c.budget.ID] = p
			order = append(order, c.budget.ID)
		}
		for k, v := range granted {
			p.Limit[k] += v
			p.AutoApproved[k] += v
		}
		for k, v := range asked {
			p.Limit[k] += v
		}
	}
	for _, c := range cands {
		if allZero(open) {
			break
		}
		if c.budget.AutoApprove == nil {
			continue
		}
		if s.maxProjectsReached(ctx, &c.budget, owner) {
			continue
		}
		g, err := s.autoGrantable(ctx, &c.budget, c.chain, owner, open)
		if err != nil {
			return SplitPlan{}, err
		}
		if allZero(g) {
			continue
		}
		add(c, g, nil)
		for k, v := range g {
			open[k] -= v
		}
	}
	if !allZero(open) {
		for _, c := range cands {
			if !c.budget.RequestsBeyondAutoApproveAllowed() {
				continue
			}
			if s.maxProjectsReached(ctx, &c.budget, owner) && parts[c.budget.ID] == nil {
				continue
			}
			if b := bindingInChain(c.chain); b == nil || s.validateLeafAvailabilities(b, open) != nil {
				continue
			}
			add(c, nil, open)
			open = common.ProjectQuota{}
			break
		}
	}
	managed := map[string]bool{}
	for _, c := range cands {
		managed[c.budget.ID] = c.manager
	}
	for _, id := range order {
		p := parts[id]
		// A manager's own budget grants all of it on creation (placeAllocation).
		if managed[id] {
			p.AutoApproved = maps.Clone(p.Limit)
		}
		quotas := []common.ProjectQuota{p.Limit, p.AutoApproved}
		for _, m := range quotas {
			for k, v := range m {
				if v == 0 {
					delete(m, k)
				}
			}
		}
		plan.Parts = append(plan.Parts, *p)
	}
	for k, v := range open {
		if v != 0 {
			if plan.Unplaced == nil {
				plan.Unplaced = common.ProjectQuota{}
			}
			plan.Unplaced[k] = v
		}
	}
	if plan.Parts == nil {
		plan.Parts = []SplitPart{}
	}
	return plan, nil
}

// nameSplit gives each part the path of its budget.
func (s *Service) nameSplit(ctx context.Context, plan SplitPlan) (SplitPlan, error) {
	var start []string
	for _, p := range plan.Parts {
		start = append(start, p.BudgetID)
	}
	tree, err := s.walkAncestors(ctx, start)
	if err != nil {
		return plan, err
	}
	for i := range plan.Parts {
		plan.Parts[i].BudgetPath = tree.path(plan.Parts[i].BudgetID)
	}
	return plan, nil
}

// fitSplitEnd is the end an allocation under chain gets for a requested end: the requested one where the budgets allow it, else the latest they allow.
func (s *Service) fitSplitEnd(end *string, chain []model.Node) (*string, error) {
	if fitted, err := s.fitNodeEnd(model.KindAllocation, end, chain); err == nil {
		return fitted, nil
	}
	bound, _ := s.endBound(model.KindAllocation, chain)
	return bound, nil
}

// maxProjectsReached reports whether owner already has as many live projects in budget as allowed (checkProjectsPerOwner).
func (s *Service) maxProjectsReached(ctx context.Context, budget *model.Node, owner string) bool {
	return s.checkProjectsPerOwner(ctx, budget, strings.TrimPrefix(owner, "user:")) != nil
}

// autoGrantable is the largest part of want that budget's auto-approve policy grants owner now: per count, the least of what is wanted, what is left of the person's share (if the policy has one) and the room in every budget up the chain; every availability wanted that the budget holds. Empty where the budget grants nothing on its own.
func (s *Service) autoGrantable(ctx context.Context, budget *model.Node, chain []model.Node, owner string, want common.ProjectQuota) (common.ProjectQuota, error) {
	if budget.AutoApprove == nil || budget.Status != model.StatusApproved {
		return common.ProjectQuota{}, nil
	}
	room, err := s.capacityRoom(ctx, chain)
	if err != nil {
		return nil, err
	}
	if !budget.AutoApprove.IsPool() {
		used, err := s.ownerActiveUsage(ctx, budget.ID, owner)
		if err != nil {
			return nil, err
		}
		for _, id := range s.countIDs() {
			cap := budget.AutoApprove.PerRequesterLimit[id]
			if cap == common.UnlimitedQuota {
				continue
			}
			left := max(cap-used[id], 0)
			if r, limited := room[id]; !limited || left < r {
				room[id] = left
			}
		}
	}
	g := common.ProjectQuota{}
	for _, id := range s.countIDs() {
		w := want[id]
		if w <= 0 {
			continue
		}
		if r, limited := room[id]; limited {
			w = min(w, r)
		}
		if w > 0 {
			g[id] = w
		}
	}
	if b := bindingInChain(chain); b != nil {
		for id, v := range want {
			if s.isBool(id) && v == 1 && grantsAvailability(b, id) {
				g[id] = 1
			}
		}
	}
	if allZero(g) || !slices.ContainsFunc(s.countIDs(), func(id string) bool { return g[id] > 0 }) {
		return common.ProjectQuota{}, nil
	}
	ok, err := s.autoApprovable(ctx, budget, owner, g, nil)
	if err != nil || !ok {
		return common.ProjectQuota{}, err
	}
	return g, nil
}

// capacityRoom is, per count resource, what is still free in every budget of chain (the least of them); a resource no budget of the chain limits is absent.
func (s *Service) capacityRoom(ctx context.Context, chain []model.Node) (common.ProjectQuota, error) {
	room := common.ProjectQuota{}
	if len(chain) == 0 {
		return room, nil
	}
	usage, err := s.loadSubtreeUsage(ctx, chain)
	if err != nil {
		return nil, fmt.Errorf("compute subtree usage: %w", err)
	}
	for _, b := range chain {
		if b.InheritsLimit {
			continue
		}
		used := usage[b.ID].Total(s.countIDs())
		for _, id := range s.countIDs() {
			limit := b.Limit[id]
			if limit == common.UnlimitedQuota {
				continue
			}
			free := max(limit-used[id], 0)
			if r, limited := room[id]; !limited || free < r {
				room[id] = free
			}
		}
	}
	return room, nil
}
