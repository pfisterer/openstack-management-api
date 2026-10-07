package tree

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

// AllocationRequest grants, changes or removes what a project draws from a
// budget above its own (see Node.Allocations). The limit is the whole
// allocation from that budget, not a delta; an empty one removes it.
type AllocationRequest struct {
	BudgetID string              `json:"budget_id" binding:"required"`
	Limit    common.ProjectQuota `json:"limit"`
	// Reason is required when granting or raising: the managers of the budget
	// in between see the allocation and should be able to tell why it exists.
	Reason string `json:"reason"`
}

// SetAllocation applies an AllocationRequest to a project.
//
// Who may do what follows the money. Granting or raising is for the managers
// of the allocating budget or above — they hold what is handed out, and the
// budgets in between are never charged for it. Removing or lowering it may also
// come from the project's owner and admins, who may give back what they hold;
// the managers in between may not, as they would veto something they never had.
//
// The allocating budget has to lie above the project's own budget: from there
// its managers already manage the project, and the project stays where it is
// in the tree, with its own budget's rules — lifetime, auto-approve, a person's
// share — applying as before, to its own limit only.
func (s *Service) SetAllocation(id string, req AllocationRequest, actor Actor, userTokens common.TokenList) (Node, error) {
	limit := maps.Clone(req.Limit)
	maps.DeleteFunc(limit, func(_ string, v int) bool { return v == 0 })
	if err := s.validateLeafLimit(limit); err != nil {
		return Node{}, err
	}

	s.approvalMu.Lock()
	defer s.approvalMu.Unlock()
	ctx, cancel := s.newCtx()
	defer cancel()

	current, err := s.store.GetNode(ctx, id)
	if err != nil {
		return Node{}, fmt.Errorf("load node: %w", err)
	}
	if current == nil {
		return Node{}, fmt.Errorf("node %w", common.ErrNotFound)
	}
	if current.Kind != KindProject {
		return Node{}, fmt.Errorf("only a project draws from budgets above its own; a budget gets more from its parent")
	}
	if current.Status == StatusImported {
		return Node{}, fmt.Errorf("imported nodes are read-only until promoted: %w", common.ErrForbidden)
	}
	if IsTerminalStatus(current.Status) {
		return Node{}, fmt.Errorf("%w: cannot change node in status %q", common.ErrConflict, current.Status)
	}

	chain, err := s.parentChainNodes(ctx, current)
	if err != nil {
		return Node{}, err
	}
	at := slices.IndexFunc(chain, func(n Node) bool { return n.ID == req.BudgetID })
	if at < 0 {
		return Node{}, fmt.Errorf("budget %q is not above this project — a project draws only from budgets above its own", req.BudgetID)
	}
	if at == 0 {
		return Node{}, fmt.Errorf("this is the project's own budget — change its resources with a change request instead")
	}
	source := chain[at]

	var held common.ProjectQuota
	for _, a := range current.Allocations {
		if a.BudgetID == source.ID {
			held = a.Limit
		}
	}
	if len(held) == 0 && len(limit) == 0 {
		return Node{}, fmt.Errorf("the project draws nothing from budget %q", nodeLabel(source))
	}

	raises := false
	for rid, v := range limit {
		if v > held[rid] {
			raises = true
		}
	}
	callerSet := common.NewTokenSet(userTokens)
	managesSource := false
	for _, n := range chain[at:] {
		if callerSet.ContainsAny(n.AdminScope) {
			managesSource = true
			break
		}
	}
	if !managesSource {
		// Giving back is the holder's right, as it is for the project's own
		// limit; anything else belongs to whoever holds what is handed out.
		holder := isOwner(userTokens, current) || callerSet.ContainsAny(current.AdminScope)
		if !holder || raises {
			return Node{}, common.ErrForbidden
		}
	}

	if raises {
		if strings.TrimSpace(req.Reason) == "" {
			return Node{}, fmt.Errorf("a reason is required: the managers of the budgets in between see this allocation and should know why it exists")
		}
		if source.Status != StatusApproved {
			return Node{}, fmt.Errorf("%w: budget %q is in status %q", common.ErrConflict, nodeLabel(source), source.Status)
		}
		if err := s.validateAllocationAvailabilities(*current, *bindingInChain(chain[at:]), limit); err != nil {
			return Node{}, err
		}
		if err := s.checkCapacity(ctx, chain[at:], limit, held); err != nil {
			return Node{}, err
		}
	}

	entry := newHistoryEntry("allocation_set", actor, current.Status)
	if len(limit) == 0 {
		entry.Event = "allocation_removed"
	}
	entry.AllocationFrom = &source.ID
	entry.LimitFrom = &held
	entry.LimitTo = &limit
	if r := strings.TrimSpace(req.Reason); r != "" {
		entry.Reason = &r
	}

	updated := *current
	updated.Allocations = slices.DeleteFunc(slices.Clone(current.Allocations), func(a Allocation) bool { return a.BudgetID == source.ID })
	if len(limit) > 0 {
		updated.Allocations = append(updated.Allocations, Allocation{
			BudgetID:  source.ID,
			Limit:     limit,
			Reason:    strings.TrimSpace(req.Reason),
			GrantedBy: actor.Email,
			GrantedAt: entry.Timestamp,
		})
	}
	if len(updated.Allocations) == 0 {
		updated.Allocations = nil
	}
	updated.History = append(slices.Clone(current.History), entry)
	if err := s.store.UpsertNode(ctx, updated); err != nil {
		return Node{}, fmt.Errorf("persist node: %w", err)
	}
	named, err := s.attachParentNames(ctx, []Node{updated})
	if err != nil {
		return updated, nil
	}
	return named[0], nil
}

// AllocationSources lists the budgets the caller may allocate to a project
// from: the approved budgets above the project's own that the caller manages,
// directly or from further up. Decorated with usage and available resources,
// so a form can say what each still has room for. Empty for anyone else —
// the project's own managers included, who change its own share instead.
func (s *Service) AllocationSources(id string, userTokens common.TokenList) ([]Node, error) {
	ctx, cancel := s.newCtx()
	defer cancel()

	project, err := s.store.GetNode(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("load node: %w", err)
	}
	if project == nil {
		return nil, fmt.Errorf("node %w", common.ErrNotFound)
	}
	if project.Kind != KindProject {
		return nil, fmt.Errorf("only a project draws from budgets above its own")
	}
	chain, err := s.parentChainNodes(ctx, project)
	if err != nil {
		return nil, err
	}
	callerSet := common.NewTokenSet(userTokens)
	var out []Node
	for i := len(chain) - 1; i >= 1; i-- {
		// Walking down from the root: once the caller manages a budget, they
		// manage every one below it as well.
		if callerSet.ContainsAny(chain[i].AdminScope) || len(out) > 0 {
			if chain[i].Status == StatusApproved {
				out = append(out, chain[i])
			}
		}
	}
	if len(out) == 0 {
		if !isOwner(userTokens, project) && !callerSet.ContainsAny(project.AdminScope) {
			if manages, err := s.managesNode(ctx, userTokens, project); err != nil {
				return nil, err
			} else if !manages {
				return nil, common.ErrForbidden
			}
		}
		return []Node{}, nil
	}
	// Nearest first: the budget closest to the project is the usual source.
	slices.Reverse(out)
	return s.attachUsage(ctx, out)
}

// validateAllocationAvailabilities checks the availabilities of an allocation:
// the allocating budget holds each one, and the project does not already get
// it elsewhere — an availability comes from one place, so that withdrawing it
// has one answer to "who still depends on this".
func (s *Service) validateAllocationAvailabilities(project, source Node, limit common.ProjectQuota) error {
	for rid, v := range limit {
		if !s.isBool(rid) || v != 1 {
			continue
		}
		if held := source.Limit[rid]; source.ID != RootNodeID && held != 1 && held != common.UnlimitedQuota {
			return fmt.Errorf("%q is not available in budget %q", rid, nodeLabel(source))
		}
		if from := availabilitySource(project, rid, source.ID); from != "" {
			return fmt.Errorf("the project already has %q from %s", rid, from)
		}
	}
	return nil
}

// availabilitySource names where the project already gets an availability
// from, apart from the allocation of budget except: its own limit (or a
// waiting proposal for it), or another allocation. Empty when nowhere.
func availabilitySource(project Node, rid, except string) string {
	if project.Limit[rid] == 1 || (project.Pending != nil && project.Pending.Limit != nil && (*project.Pending.Limit)[rid] == 1) {
		return "its own budget"
	}
	for _, a := range project.Allocations {
		if a.BudgetID != except && a.Limit[rid] == 1 {
			return fmt.Sprintf("budget %q", cmp.Or(a.BudgetName, a.BudgetID))
		}
	}
	return ""
}

// checkOwnLimitAgainstAllocations refuses a project limit that grants an
// availability one of its allocations already grants.
func (s *Service) checkOwnLimitAgainstAllocations(project Node, limit common.ProjectQuota) error {
	for rid, v := range limit {
		if !s.isBool(rid) || v != 1 {
			continue
		}
		for _, a := range project.Allocations {
			if a.Limit[rid] == 1 {
				return fmt.Errorf("the project already has %q from budget %q — an availability comes from one place", rid, cmp.Or(a.BudgetName, a.BudgetID))
			}
		}
	}
	return nil
}

// checkAllocationsAfterMove refuses a move that would leave an allocation
// coming from a budget no longer above the project: the project would hold
// something no budget above it is charged for. Checked for the moved node and,
// when it is a budget, for every project below it. Nothing is dropped
// silently — the allocation has to be removed first.
func (s *Service) checkAllocationsAfterMove(ctx context.Context, moved Node, newParentChain []Node) error {
	above := map[string]bool{}
	for _, n := range newParentChain {
		above[n.ID] = true
	}
	projects := []Node{moved}
	inside := map[string]bool{}
	if moved.Kind == KindBudget {
		parentMap, err := s.buildSubtreeParentMap(ctx, []Node{moved})
		if err != nil {
			return fmt.Errorf("walk subtree: %w", err)
		}
		budgetIDs := make([]string, 0, len(parentMap))
		for bid := range parentMap {
			budgetIDs = append(budgetIDs, bid)
			inside[bid] = true
		}
		if projects, err = s.store.ListNodes(ctx, NodeQuery{ParentIDs: budgetIDs, Kinds: []string{KindProject}}, 0, 0); err != nil {
			return fmt.Errorf("load projects below: %w", err)
		}
	}
	for _, p := range projects {
		if IsTerminalStatus(p.Status) {
			continue
		}
		for _, a := range p.Allocations {
			// Above the new parent, or inside the moved subtree (it moves along).
			if above[a.BudgetID] || inside[a.BudgetID] {
				if moved.Kind == KindProject && len(newParentChain) > 0 && newParentChain[0].ID == a.BudgetID {
					return fmt.Errorf("%s draws an allocation from budget %q, its new own budget — remove the allocation first", nodeLabel(p), nodeLabel(newParentChain[0]))
				}
				continue
			}
			return fmt.Errorf("%s draws an allocation from budget %q, which would no longer be above it — remove the allocation first", nodeLabel(p), a.BudgetID)
		}
	}
	return nil
}
