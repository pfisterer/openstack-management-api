package tree

import (
	"context"
	"fmt"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/nodestore"
)

// checkAvailabilityWithdrawal refuses to take an availability away from a budget
// while something below it still holds it.
//
// The parent-child rule is enforced when a CHILD is written, so it cannot catch
// this direction: lowering a parent from 1 to 0 leaves every existing descendant
// holding a resource its parent no longer has, and nothing would ever notice.
// Quantities have the same problem and the same answer — "new limit is below
// current active usage" — so this is that check for the other kind.
//
// Refusing rather than cascading is deliberate. A cascade would silently revoke
// a network or a GPU flavour across a whole subtree, in projects whose owners
// never asked for the change; making the withdrawal fail puts the decision back
// with the person who has to take it away one node at a time.
func (s *Service) checkAvailabilityWithdrawal(ctx context.Context, node model.Node, newLimit common.ProjectQuota) error {
	withdrawn := make([]string, 0, len(s.resources()))
	for _, r := range s.resources() {
		if !r.IsBool() {
			continue
		}
		if node.Limit[r.ID] == 1 && newLimit[r.ID] != 1 {
			withdrawn = append(withdrawn, r.ID)
		}
	}
	if len(withdrawn) == 0 {
		return nil
	}

	holders, err := s.descendantsHolding(ctx, node, withdrawn)
	if err != nil {
		return err
	}
	for _, id := range withdrawn {
		if names := holders[id]; len(names) > 0 {
			return fmt.Errorf("%q cannot be withdrawn while %d node(s) below still hold it (e.g. %s)",
				id, len(names), names[0])
		}
	}
	return nil
}

// descendantsHolding reports, per resource id, the names of nodes in the subtree
// that carry it. The node itself is excluded — it is the one being changed.
func (s *Service) descendantsHolding(ctx context.Context, root model.Node, resourceIDs []string) (map[string][]string, error) {
	parentMap, err := s.buildSubtreeParentMap(ctx, []model.Node{root})
	if err != nil {
		return nil, fmt.Errorf("walk subtree: %w", err)
	}

	budgetIDs := make([]string, 0, len(parentMap))
	for id := range parentMap {
		budgetIDs = append(budgetIDs, id)
	}
	// Leaves are not in the parent map — it walks budgets only — so they are
	// fetched in one go under every budget of the subtree.
	leaves, err := s.store.ListNodes(ctx, nodestore.NodeQuery{ParentIDs: budgetIDs, Kinds: []string{model.KindProject}}, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("load subtree leaves: %w", err)
	}
	budgets, err := s.store.ListNodes(ctx, nodestore.NodeQuery{IDs: budgetIDs}, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("load subtree budgets: %w", err)
	}

	// A project holds it from this subtree through its own limit — it lives
	// here — or through an allocation from a budget of the subtree. One that
	// has it from a budget further up does not depend on this one at all.
	holds := func(n model.Node, id string) bool {
		if n.Limit[id] == 1 {
			return true
		}
		for _, a := range n.Allocations {
			if _, inSubtree := parentMap[a.BudgetID]; inSubtree && a.Limit[id] == 1 {
				return true
			}
		}
		return false
	}
	out := make(map[string][]string, len(resourceIDs))
	for _, n := range append(budgets, leaves...) {
		if n.ID == root.ID {
			continue
		}
		for _, id := range resourceIDs {
			if holds(n, id) {
				out[id] = append(out[id], nodeLabel(n))
			}
		}
	}
	return out, nil
}

// nodeLabel names a node for an error message: its name where it has one,
// otherwise its id. Leaves often carry no name.
func nodeLabel(n model.Node) string {
	if n.Name != "" {
		return n.Name
	}
	return n.ID
}

// checkCapacity verifies that adding addLimit to the current committed usage of
// each budget in ancestors stays within its limit. subtractLimit is removed from
// the committed total first — used when a change_pending node's current resources
// are already in the active set and must not be double-counted.
func (s *Service) checkCapacity(ctx context.Context, ancestors []model.Node, addLimit, subtractLimit common.ProjectQuota) error {
	if len(ancestors) == 0 {
		return nil
	}
	subtreeUsage, err := s.loadSubtreeUsage(ctx, ancestors)
	if err != nil {
		return fmt.Errorf("compute subtree usage for capacity check: %w", err)
	}
	for _, ancestor := range ancestors {
		// One that inherits has no limit of its own; the budget above it
		// whose limit applies is in the chain too.
		if ancestor.InheritsLimit {
			continue
		}
		committed := subtreeUsage[ancestor.ID].Total(s.countIDs)
		for _, resourceID := range s.countIDs {
			committed[resourceID] -= subtractLimit[resourceID]
		}
		needed := quotaAdd(committed, addLimit, s.countIDs)
		if id, ok := shortfall(needed, ancestor.Limit, s.countIDs); ok {
			// Named, with the numbers: "capacity exceeded" alone leaves a
			// request with ten resources to guess which one did not fit.
			free := ancestor.Limit[id] - (needed[id] - addLimit[id])
			return fmt.Errorf("budget %q (%s) capacity exceeded: %s asks %d, %d free", ancestor.ID, ancestor.Name, id, addLimit[id], max(free, 0))
		}
	}
	return nil
}

// validateLeafLimit rejects a leaf limit that is negative, set on an unmanaged
// resource, or empty. A leaf limit is a concrete allocation with no "unlimited"
// sentinel: a negative value would lower the tracked usage and map to Nova's
// unlimited, letting a request grab unbounded resources.
func (s *Service) validateLeafLimit(q common.ProjectQuota) error {
	if err := s.validateKnownResources(q); err != nil {
		return err
	}
	if err := s.validateAvailabilities(q); err != nil {
		return err
	}
	for key, val := range q {
		if s.isBool(key) {
			continue
		}
		if val < 0 {
			return fmt.Errorf("limit for %q must not be negative (got %d)", key, val)
		}
	}
	return nil
}

// validateBudgetLimit rejects unknown resources and negative values other than the
// UnlimitedQuota sentinel. Missing keys count as 0 — an intentionally hard cap.
func (s *Service) validateBudgetLimit(q common.ProjectQuota) error {
	if err := s.validateKnownResources(q); err != nil {
		return err
	}
	if err := s.validateAvailabilities(q); err != nil {
		return err
	}
	for key, val := range q {
		if s.isBool(key) {
			continue
		}
		if val < 0 && val != common.UnlimitedQuota {
			return fmt.Errorf("limit for %q must be >= 0 or %d for unlimited (got %d)", key, common.UnlimitedQuota, val)
		}
	}
	return nil
}

// validateAvailabilities holds availabilities to 0 or 1.
//
// The value that matters here is UnlimitedQuota. "Unlimited access to a network"
// is not a thing, and letting it through would be worse than meaningless: the
// parent-child rule reads UnlimitedQuota as "no cap", so a -1 on an availability
// would act as a wildcard that hands the resource to every descendant.
//
// Anything above 1 is refused for the same reason a boolean has two values —
// a 2 would sort above a 1 in the edge check and mean nothing at all.
func (s *Service) validateAvailabilities(q common.ProjectQuota) error {
	for key, val := range q {
		if !s.isBool(key) {
			continue
		}
		if val != 0 && val != 1 {
			return fmt.Errorf("%q is an availability: it is granted (1) or not (0), got %d", key, val)
		}
	}
	return nil
}

// isBool reports whether a resource id names an availability. Unknown ids answer
// false; validateKnownResources is what rejects those, and it runs first.
func (s *Service) isBool(id string) bool {
	for _, r := range s.resources() {
		if r.ID == id {
			return r.IsBool()
		}
	}
	return false
}

// validateKnownResources rejects ids the catalogue does not know, and any value
// but zero for an availability that is being withdrawn: it stays known so the
// zeros the withdrawal wrote validate, but it cannot be granted again.
func (s *Service) validateKnownResources(q common.ProjectQuota) error {
	known := make(map[string]common.Resource, len(s.resources()))
	for _, r := range s.resources() {
		known[r.ID] = r
	}
	for key, v := range q {
		r, ok := known[key]
		if !ok {
			return fmt.Errorf("unknown resource %q", key)
		}
		if r.Withdrawn && v != 0 {
			return fmt.Errorf("%q is being withdrawn and can no longer be granted", key)
		}
	}
	return nil
}

// bindingBudget returns the budget whose limit applies at n: n itself, or,
// where n inherits its limit (Node.InheritsLimit), the nearest budget above it
// with one of its own. A budget that inherits stores no limit, so reading its
// own would read zero everywhere. The checks have the chain in hand and use
// bindingInChain; this is for showing a single node.
func (s *Service) bindingBudget(ctx context.Context, n *model.Node) (*model.Node, error) {
	if !n.InheritsLimit {
		return n, nil
	}
	chain, err := s.nodeChain(ctx, n.ID)
	if err != nil {
		return nil, err
	}
	if b := bindingInChain(chain); b != nil {
		return b, nil
	}
	return nil, fmt.Errorf("budget %q inherits its limit, but no budget above it has one", nodeLabel(*n))
}

// bindingInChain is bindingBudget on a chain already loaded, nearest first:
// its first node with a limit of its own, nil for an empty chain.
func bindingInChain(chain []model.Node) *model.Node {
	for i := range chain {
		if !chain[i].InheritsLimit {
			return &chain[i]
		}
	}
	return nil
}

// validateChildBudgetLimit enforces the static invariant that a child budget's
// limit cannot exceed its direct parent's limit for any resource (and cannot be
// unlimited under a limited parent). Enforced on every edge — create, approve,
// direct limit edit and reparent — so it holds inductively across the tree.
func (s *Service) validateChildBudgetLimit(parent *model.Node, childLimit common.ProjectQuota) error {
	for _, r := range s.resources() {
		id := r.ID
		parentCap := parent.Limit[id]
		childCap := childLimit[id]
		if parentCap == common.UnlimitedQuota {
			continue
		}
		if childCap == common.UnlimitedQuota {
			return fmt.Errorf("child limit for %q cannot be unlimited when parent limit is %d", id, parentCap)
		}
		if childCap > parentCap {
			return fmt.Errorf("child limit for %q (%d) exceeds parent limit (%d)", id, childCap, parentCap)
		}
	}
	return nil
}

// validateLeafAvailabilities refuses a project limit that grants an availability
// the project's budget does not hold.
//
// Budgets get this rule on every edge from validateChildBudgetLimit; projects
// need it separately, because nothing else would catch it — the capacity and
// per-requester checks count quantities only, and availabilities never sum.
// Without it anyone who may request under a budget could grant themselves any
// availability by naming it, and an auto-approve budget would even approve it
// on the spot. The UI hides what a budget does not carry, but that is only
// presentation.
//
// The root holds the whole catalogue (see availableResourcesFor).
func (s *Service) validateLeafAvailabilities(parent *model.Node, limit common.ProjectQuota) error {
	for _, r := range s.resources() {
		if !s.isBool(r.ID) || limit[r.ID] != 1 {
			continue
		}
		if parent == nil {
			return fmt.Errorf("%q cannot be granted without a budget", r.ID)
		}
		if parent.ID == model.RootNodeID {
			continue
		}
		if held := parent.Limit[r.ID]; held == 1 || held == common.UnlimitedQuota {
			continue
		}
		return fmt.Errorf("%q is not available in budget %q — it has to be delegated to that budget first", r.ID, nodeLabel(*parent))
	}
	return nil
}

// checkBudgetLimit checks limit as the new limit of budget n against the
// chain above it, nearest first: a valid limit, and no more than the budget
// whose limit applies there. inForce says the limit replaces one that budgets
// and projects below already rely on — then it may neither withdraw an
// availability somebody below still holds nor fall below what is in use.
// Every path that sets a budget's limit comes through here.
func (s *Service) checkBudgetLimit(ctx context.Context, n model.Node, chain []model.Node, limit common.ProjectQuota, inForce bool) error {
	if err := s.validateBudgetLimit(limit); err != nil {
		return err
	}
	if b := bindingInChain(chain); b != nil {
		if err := s.validateChildBudgetLimit(b, limit); err != nil {
			return err
		}
	}
	if !inForce {
		return nil
	}
	if err := s.checkAvailabilityWithdrawal(ctx, n, limit); err != nil {
		return err
	}
	probe := n
	probe.Limit = limit
	subtreeUsage, err := s.loadSubtreeUsage(ctx, []model.Node{probe})
	if err != nil {
		return fmt.Errorf("compute subtree usage for limit check: %w", err)
	}
	inUse := subtreeUsage[n.ID].Total(s.countIDs)
	for _, id := range s.countIDs {
		if limit[id] != common.UnlimitedQuota && inUse[id] > limit[id] {
			return fmt.Errorf("new limit for %q (%d) is below current active usage (%d)", id, limit[id], inUse[id])
		}
	}
	return nil
}

// checkLeafLimit checks limit as the limit of a project placed under chain,
// nearest first: a valid limit, every availability in it held by the budget
// whose limit applies there, and nothing in it that the project also draws
// from a budget further up. commit says the limit is being granted: then
// every budget in the chain must have room for it, subtract being what the
// project holds there already. A request that only waits for a decision
// commits nothing, so capacity is checked when it is decided. Every path that
// sets a project's limit comes through here.
func (s *Service) checkLeafLimit(ctx context.Context, project model.Node, chain []model.Node, limit, subtract common.ProjectQuota, commit bool) error {
	if err := s.validateLeafLimit(limit); err != nil {
		return err
	}
	if b := bindingInChain(chain); b != nil {
		if err := s.validateLeafAvailabilities(b, limit); err != nil {
			return err
		}
	}
	if err := s.checkOwnLimitAgainstAllocations(project, limit); err != nil {
		return err
	}
	if commit {
		return s.checkCapacity(ctx, chain, limit, subtract)
	}
	return nil
}

// loadTarget loads the budget a node is to be moved under — by a move or the
// promotion of an import — and checks that the caller may take the node away
// from where it is and put it there. Returns the target and its chain,
// nearest first.
func (s *Service) loadTarget(ctx context.Context, userTokens common.TokenList, current *model.Node, targetID string) (*model.Node, []model.Node, error) {
	target, err := s.store.GetNode(ctx, targetID)
	if err != nil {
		return nil, nil, fmt.Errorf("load new parent: %w", err)
	}
	if target == nil {
		return nil, nil, fmt.Errorf("new parent %w", common.ErrNotFound)
	}
	if target.Kind != model.KindBudget || target.Status != model.StatusApproved {
		return nil, nil, fmt.Errorf("the new parent must be an approved budget")
	}
	// A wrong target is a wrong request, not a missing right.
	if target.ID == model.UnassignedNodeID {
		return nil, nil, fmt.Errorf("nodes cannot be moved into the unassigned collection")
	}
	chain, err := s.nodeChain(ctx, target.ID)
	if err != nil {
		return nil, nil, err
	}
	if manages, err := s.managesParentChain(ctx, userTokens, current); err != nil {
		return nil, nil, err
	} else if !manages {
		return nil, nil, common.ErrForbidden
	}
	if manages, err := s.managesNode(ctx, userTokens, target); err != nil {
		return nil, nil, err
	} else if !manages {
		return nil, nil, common.ErrForbidden
	}
	return target, chain, nil
}

// limitShrinks reports whether next asks for nothing that current does not
// already hold: no count grows and no availability is newly switched on.
// Giving something back never needs anybody's permission.
func (s *Service) limitShrinks(current, next common.ProjectQuota) bool {
	for id, v := range next {
		if s.isBool(id) {
			if v > 0 && current[id] <= 0 {
				return false
			}
			continue
		}
		if v > current[id] {
			return false
		}
	}
	return true
}
