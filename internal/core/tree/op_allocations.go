package tree

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/pfisterer/cloud-self-service-golib/msg"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/nodestore"
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
func (s *Service) SetAllocation(id string, req AllocationRequest, actor model.Actor, userTokens common.TokenList) (model.Node, error) {
	limit := maps.Clone(req.Limit)
	maps.DeleteFunc(limit, func(_ string, v int) bool { return v == 0 })
	if err := s.validateLeafLimit(limit); err != nil {
		return model.Node{}, err
	}
	if err := validateReason(req.Reason); err != nil {
		return model.Node{}, err
	}

	s.approvalMu.Lock()
	defer s.approvalMu.Unlock()
	ctx, cancel := s.newCtx()
	defer cancel()

	current, err := s.loadLive(ctx, id)
	if err != nil {
		return model.Node{}, err
	}
	if current.Kind != model.KindProject {
		return model.Node{}, invalid("Nur ein Projekt bekommt Sonderzuteilungen aus Budgets über seinem eigenen; ein Budget bekommt mehr von dem Budget darüber.", "Only a project gets extra allocations from budgets above its own; a budget gets more from the budget above it.")
	}

	chain, err := s.parentChainNodes(ctx, current)
	if err != nil {
		return model.Node{}, err
	}
	at := slices.IndexFunc(chain, func(n model.Node) bool { return n.ID == req.BudgetID })
	if at < 0 {
		return model.Node{}, invalid("Das Budget %q liegt nicht über diesem Projekt. Sonderzuteilungen kommen nur aus Budgets über dem eigenen.", "The budget %q is not above this project. Extra allocations come only from budgets above its own.", req.BudgetID)
	}
	if at == 0 {
		return model.Node{}, invalid("Das ist das eigene Budget des Projekts. Seine Ressourcen ändern sich über eine Änderungsanfrage.", "This is the project's own budget. Its resources change through a change request.")
	}
	source := chain[at]

	var held common.ProjectQuota
	for _, a := range current.Allocations {
		if a.BudgetID == source.ID {
			held = a.Limit
		}
	}
	if len(held) == 0 && len(limit) == 0 {
		return model.Node{}, invalid("Das Projekt hat keine Sonderzuteilung aus dem Budget %q.", "The project has no extra allocation from the budget %q.", nodeLabel(source))
	}

	raises := !s.limitShrinks(held, limit)
	callerSet := common.NewTokenSet(userTokens)
	// The source and the budgets above it, the way managesNode would walk from the source.
	if !managesInChain(callerSet, chain[at:]) {
		// Giving back is the holder's right, as it is for the project's own
		// limit; anything else belongs to whoever holds what is handed out.
		holder := isOwner(userTokens, current) || callerSet.ContainsAny(current.AdminScope)
		if !holder || raises {
			return model.Node{}, errNoPermission
		}
	}

	if raises {
		if strings.TrimSpace(req.Reason) == "" {
			return model.Node{}, invalid("Eine Begründung ist erforderlich: Die Verwaltenden der Budgets dazwischen sehen diese Sonderzuteilung und sollen wissen, warum es sie gibt.", "A reason is required: the managers of the budgets in between see this extra allocation and should know why it exists.")
		}
		if source.Status != model.StatusApproved {
			return model.Node{}, refuse(common.ErrConflict, "Das Budget %q ist im Status %q.", "The budget %q is in status %q.", nodeLabel(source), source.Status)
		}
		if err := s.validateAllocationAvailabilities(*current, *bindingInChain(chain[at:]), limit); err != nil {
			return model.Node{}, err
		}
		if err := s.checkCapacity(ctx, chain[at:], limit, held); err != nil {
			return model.Node{}, err
		}
	}

	entry := model.NewHistoryEntry("allocation_set", actor, current.Status)
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
	updated.Allocations = slices.DeleteFunc(slices.Clone(current.Allocations), func(a model.Allocation) bool { return a.BudgetID == source.ID })
	if len(limit) > 0 {
		updated.Allocations = append(updated.Allocations, model.Allocation{
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
	// Resources drawn from above may make the project a provider's business, with that provider's ceiling on its members.
	if raises {
		if err := checkMemberCeilingFalls(len(current.AuthorizedUsers), s.memberCeilingOf(*current), s.memberCeilingOf(updated)); err != nil {
			return model.Node{}, err
		}
	}
	updated.History = append(slices.Clone(current.History), entry)
	if err := s.store.UpsertNode(ctx, updated); err != nil {
		return model.Node{}, fmt.Errorf("persist node: %w", err)
	}
	named, err := s.attachParentNames(ctx, []model.Node{updated}, userTokens)
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
func (s *Service) AllocationSources(id string, userTokens common.TokenList) ([]model.Node, error) {
	ctx, cancel := s.newCtx()
	defer cancel()

	project, err := s.loadNode(ctx, id)
	if err != nil {
		return nil, err
	}
	if project.Kind != model.KindProject {
		return nil, invalid("Nur ein Projekt bekommt Sonderzuteilungen aus Budgets über seinem eigenen.", "Only a project gets extra allocations from budgets above its own.")
	}
	chain, err := s.parentChainNodes(ctx, project)
	if err != nil {
		return nil, err
	}
	callerSet := common.NewTokenSet(userTokens)
	var out []model.Node
	for i := len(chain) - 1; i >= 1; i-- {
		// Walking down from the root: once the caller manages a budget, they
		// manage every one below it as well.
		if callerSet.ContainsAny(chain[i].AdminScope) || len(out) > 0 {
			if chain[i].Status == model.StatusApproved {
				out = append(out, chain[i])
			}
		}
	}
	if len(out) == 0 {
		if !isOwner(userTokens, project) && !managesNodeIn(callerSet, project, chain) {
			return nil, errNoPermission
		}
		return []model.Node{}, nil
	}
	// Nearest first: the budget closest to the project is the usual source.
	slices.Reverse(out)
	return s.attachUsage(ctx, out, userTokens)
}

// validateAllocationAvailabilities checks the availabilities of an allocation:
// the allocating budget holds each one, and the project does not already get
// it elsewhere — an availability comes from one place, so that withdrawing it
// has one answer to "who still depends on this".
func (s *Service) validateAllocationAvailabilities(project, source model.Node, limit common.ProjectQuota) error {
	for rid, v := range limit {
		if !s.isBool(rid) || v != 1 {
			continue
		}
		if !grantsAvailability(&source, rid) {
			return invalid("%q ist im Budget %q nicht verfügbar.", "%q is not available in the budget %q.", rid, nodeLabel(source))
		}
		if from := availabilitySource(project, rid, source.ID); !from.IsZero() {
			return invalid("Das Projekt hat %q bereits aus %s.", "The project already has %q from %s.", rid, from)
		}
	}
	return nil
}

// availabilitySource names where the project already gets an availability
// from, apart from the allocation of budget except: its own limit (or a
// waiting proposal for it), or another allocation. Zero when nowhere.
func availabilitySource(project model.Node, rid, except string) msg.Text {
	if project.Limit.Grants(rid) || (project.Pending != nil && project.Pending.Limit != nil && project.Pending.Limit.Grants(rid)) {
		return msg.T("seinem eigenen Budget", "its own budget")
	}
	for _, a := range project.Allocations {
		if a.BudgetID != except && a.Limit.Grants(rid) {
			return msg.F("dem Budget %q", "the budget %q", cmp.Or(a.BudgetName, a.BudgetID))
		}
	}
	return msg.Text{}
}

// checkOwnLimitAgainstAllocations refuses a project limit that grants an
// availability one of its allocations already grants.
func (s *Service) checkOwnLimitAgainstAllocations(project model.Node, limit common.ProjectQuota) error {
	for rid, v := range limit {
		if !s.isBool(rid) || v != 1 {
			continue
		}
		for _, a := range project.Allocations {
			if a.Limit.Grants(rid) {
				return invalid("Das Projekt hat %q bereits aus dem Budget %q. Eine Verfügbarkeit kommt nur aus einer Stelle.", "The project already has %q from the budget %q. An availability comes from one place only.", rid, cmp.Or(a.BudgetName, a.BudgetID))
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
func (s *Service) checkAllocationsAfterMove(ctx context.Context, moved model.Node, newParentChain []model.Node) error {
	above := map[string]bool{}
	for _, n := range newParentChain {
		above[n.ID] = true
	}
	projects := []model.Node{moved}
	inside := map[string]bool{}
	if moved.Kind == model.KindBudget {
		parentMap, err := s.buildSubtreeParentMap(ctx, []model.Node{moved})
		if err != nil {
			return fmt.Errorf("walk subtree: %w", err)
		}
		budgetIDs := make([]string, 0, len(parentMap))
		for bid := range parentMap {
			budgetIDs = append(budgetIDs, bid)
			inside[bid] = true
		}
		if projects, err = s.store.ListNodes(ctx, nodestore.NodeQuery{ParentIDs: budgetIDs, Kinds: []string{model.KindProject}}, 0, 0); err != nil {
			return fmt.Errorf("load projects below: %w", err)
		}
	}
	for _, p := range projects {
		if model.IsTerminalStatus(p.Status) {
			continue
		}
		for _, a := range p.Allocations {
			// Above the new parent, or inside the moved subtree (it moves along).
			if above[a.BudgetID] || inside[a.BudgetID] {
				if moved.Kind == model.KindProject && len(newParentChain) > 0 && newParentChain[0].ID == a.BudgetID {
					return invalid("%s hat eine Sonderzuteilung aus dem Budget %q, das sein neues eigenes Budget wäre. Erst die Sonderzuteilung entfernen.", "%s has an extra allocation from the budget %q, which would be its new own budget. Remove the extra allocation first.", nodeLabel(p), nodeLabel(newParentChain[0]))
				}
				continue
			}
			return invalid("%s hat eine Sonderzuteilung aus dem Budget %q, das dann nicht mehr darüber läge. Erst die Sonderzuteilung entfernen.", "%s has an extra allocation from the budget %q, which would then no longer be above it. Remove the extra allocation first.", nodeLabel(p), a.BudgetID)
		}
	}
	return nil
}
