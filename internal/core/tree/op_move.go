package tree

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/nodestore"
)

// ReparentNode moves a node under a new parent budget. Requires management rights
// on the node's CURRENT parent chain (you give it away) AND on the new parent (you
// receive it). Active nodes are capacity-checked against the new ancestor chain.
func (s *Service) ReparentNode(id string, req ReparentNodeRequest, actor model.Actor, userTokens common.TokenList) (model.Node, error) {
	s.approvalMu.Lock()
	defer s.approvalMu.Unlock()

	ctx, cancel := s.newCtx()
	defer cancel()

	current, err := s.loadLive(ctx, id)
	if err != nil {
		return model.Node{}, err
	}
	if current.ID == model.RootNodeID || current.ID == model.UnassignedNodeID {
		return model.Node{}, fmt.Errorf("structural nodes cannot be moved: %w", common.ErrForbidden)
	}
	if current.ParentID != nil && *current.ParentID == req.NewParentID {
		return model.Node{}, fmt.Errorf("%w: node is already under this parent", common.ErrConflict)
	}

	newParent, newParentChain, err := s.loadTarget(ctx, userTokens, current, req.NewParentID)
	if err != nil {
		return model.Node{}, err
	}
	// Cycle guard: the new parent must not lie inside the node's own subtree.
	for _, ancestor := range newParentChain {
		if ancestor.ID == current.ID {
			return model.Node{}, fmt.Errorf("cannot move a node into its own subtree")
		}
	}

	// Capacity / limit invariants against the new chain for nodes that carry
	// consumption. Pending nodes move freely — they are checked at approval.
	// The set is the accounting's, not a fixed list: a released leaf that is
	// billed must also be checked when it moves, or moving it is a way to put
	// charged usage into a budget that has no room for it.
	if slices.Contains(s.chargedStatuses(), current.Status) {
		if current.IsLeaf() {
			need := current.Limit
			if current.Status == model.StatusArchived {
				need, _ = s.leafCost(*current)
			}
			// What it costs is charged; a retired project's limit is no
			// longer granted, so only an active one is checked against it.
			if err := s.checkCapacity(ctx, newParentChain, need, nil); err != nil {
				return model.Node{}, err
			}
			if current.Status == model.StatusApproved || current.Status == model.StatusChangePending {
				if err := s.checkLeafLimit(ctx, *current, newParentChain, current.Limit, nil, false); err != nil {
					return model.Node{}, err
				}
			}
		} else {
			// One that inherits has no limit to fit; what is used below it
			// is checked next.
			if !current.InheritsLimit {
				if err := s.checkBudgetLimit(ctx, *current, newParentChain, current.Limit, false); err != nil {
					return model.Node{}, err
				}
			} else if err := s.checkInheritingMove(ctx, *current, newParent.ID, newParentChain); err != nil {
				return model.Node{}, err
			}
			subtreeUsage, err := s.loadSubtreeUsage(ctx, []model.Node{*current})
			if err != nil {
				return model.Node{}, fmt.Errorf("compute subtree usage for move: %w", err)
			}
			moved := subtreeUsage[current.ID].Total(s.countIDs())
			if err := s.checkCapacity(ctx, newParentChain, moved, nil); err != nil {
				return model.Node{}, err
			}
		}
	}

	// An allocation has to keep coming from above.
	if err := s.checkAllocationsAfterMove(ctx, *current, newParentChain); err != nil {
		return model.Node{}, err
	}

	historyEntry := model.NewHistoryEntry("reparented", actor, current.Status)
	historyEntry.ParentFrom = current.ParentID
	historyEntry.ParentTo = &newParent.ID

	updated := *current
	updated.ParentID = &newParent.ID
	updated.History = append(slices.Clone(current.History), historyEntry)
	// Under a budget that ends sooner, the moved node and everything below it
	// end with it; under a budget with a shorter maximum project term, the
	// projects among them are shortened to it.
	now := time.Now()
	bound, reason := chainEnd(newParentChain), endsWithReason(*newParent)
	maxTerm := chainMaxTerm(newParentChain)
	if updated.IsLeaf() {
		if b, term := projectBound(newParentChain, now); term != nil {
			bound, reason = b, termReason(*newParent, *term)
		}
	}
	if bound != nil {
		if err := shortenEnd(&updated, *bound, actor, reason); err != nil && !errors.Is(err, nodestore.ErrSkipUpdate) {
			return model.Node{}, err
		}
	}
	if maxTerm != nil && !updated.IsLeaf() {
		if err := shortenTerm(&updated, *maxTerm, actor, termReason(*newParent, *maxTerm)); err != nil && !errors.Is(err, nodestore.ErrSkipUpdate) {
			return model.Node{}, err
		}
	}

	if err := s.store.UpsertNode(ctx, updated); err != nil {
		return model.Node{}, fmt.Errorf("persist node: %w", err)
	}
	if bound != nil && !updated.IsLeaf() {
		if _, err := s.shortenSubtreeEnds(ctx, []string{updated.ID}, *bound, actor, reason); err != nil {
			return model.Node{}, err
		}
	}
	if updated.MaxProjectTermDays != nil && !updated.IsLeaf() {
		days := *updated.MaxProjectTermDays
		if _, err := s.shortenSubtreeTerms(ctx, []string{updated.ID}, days, now, actor, termReason(updated, days)); err != nil {
			return model.Node{}, err
		}
	}
	return updated, nil
}

// checkInheritingMove checks a move of a budget that inherits its limit: the budgets below it that have a limit of their own were held to the limit that applied there, and after the move a different one applies — the new parent's, or the one it inherits. Each of them must fit it, as if it were created there. Budgets below that inherit as well pass the check on to their own children.
func (s *Service) checkInheritingMove(ctx context.Context, moved model.Node, newParentID string, newParentChain []model.Node) error {
	moved.ParentID = &newParentID
	type level struct {
		node  model.Node
		chain []model.Node // nearest first, ending in the new parent's chain
	}
	queue := []level{{moved, append([]model.Node{moved}, newParentChain...)}}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		children, err := s.store.ListNodes(ctx, nodestore.NodeQuery{ParentIDs: []string{next.node.ID}, Kinds: []string{model.KindBudget}}, 0, 0)
		if err != nil {
			return fmt.Errorf("load budgets below %q: %w", nodeLabel(next.node), err)
		}
		for _, child := range children {
			// A request is held to the limit that applies when it is decided.
			if !slices.Contains(model.ActiveStatuses, child.Status) {
				continue
			}
			if child.InheritsLimit {
				queue = append(queue, level{child, append([]model.Node{child}, next.chain...)})
				continue
			}
			if err := s.checkBudgetLimit(ctx, child, next.chain, child.Limit, false); err != nil {
				return fmt.Errorf("budget %q below does not fit there: %w", nodeLabel(child), err)
			}
		}
	}
	return nil
}

// TransferOwner hands a leaf to a new responsible person. Only managers of the
// parent chain may transfer (e.g. when the current owner leaves the organization).
func (s *Service) TransferOwner(id string, req TransferOwnerRequest, actor model.Actor, userTokens common.TokenList) (model.Node, error) {
	// Serialized with the approvals: who owns a project decides whose share it counts against.
	s.approvalMu.Lock()
	defer s.approvalMu.Unlock()

	ctx, cancel := s.newCtx()
	defer cancel()

	current, err := s.loadLive(ctx, id)
	if err != nil {
		return model.Node{}, err
	}
	if !current.IsLeaf() {
		return model.Node{}, fmt.Errorf("only project leaves have an owner")
	}

	if manages, err := s.managesParentChain(ctx, userTokens, current); err != nil {
		return model.Node{}, err
	} else if !manages {
		return model.Node{}, common.ErrForbidden
	}

	newOwner, err := normalizeOwnerToken(req.NewOwner)
	if err != nil {
		return model.Node{}, err
	}

	// Written on the node as it is now, not on the copy the checks read: a reconciler pass in between must not be undone. What was checked is checked again on that state.
	var updated model.Node
	if _, err := s.store.UpdateNode(ctx, id, func(n *model.Node) error {
		if n.Status == model.StatusImported || model.IsTerminalStatus(n.Status) {
			return fmt.Errorf("%w: a node in status %q cannot be changed", common.ErrConflict, n.Status)
		}
		if derefParent(n.ParentID) != derefParent(current.ParentID) {
			return fmt.Errorf("%w: the project was moved meanwhile; reload", common.ErrConflict)
		}
		if newOwner == n.Owner {
			return fmt.Errorf("%w: node is already owned by %s", common.ErrConflict, newOwner)
		}
		historyEntry := model.NewHistoryEntry("owner_transferred", actor, n.Status)
		historyEntry.OwnerFrom = new(n.Owner)
		historyEntry.OwnerTo = &newOwner
		n.Owner = newOwner
		n.History = append(slices.Clone(n.History), historyEntry)
		updated = *n
		return nil
	}); err != nil {
		return model.Node{}, err
	}
	if updated.ID == "" {
		return model.Node{}, fmt.Errorf("node %w", common.ErrNotFound)
	}
	return updated, nil
}

// derefParent is a parent id for comparison, "" for none.
func derefParent(id *string) string {
	if id == nil {
		return ""
	}
	return *id
}

// PromoteNode converts an imported leaf into a managed request: it is reparented
// under req.NewParentID, gets an owner and the promote flag. The reconciler then
// tags the existing OpenStack project and transitions the leaf to pending, after
// which the normal approval cycle applies. Requires management rights on the
// unassigned chain (root admins) and on the target budget.
func (s *Service) PromoteNode(id string, req PromoteNodeRequest, actor model.Actor, userTokens common.TokenList) (model.Node, error) {
	s.approvalMu.Lock()
	defer s.approvalMu.Unlock()

	ctx, cancel := s.newCtx()
	defer cancel()

	current, err := s.loadNode(ctx, id)
	if err != nil {
		return model.Node{}, err
	}
	if current.Status != model.StatusImported {
		return model.Node{}, fmt.Errorf("only imported nodes can be promoted: %w", common.ErrForbidden)
	}

	newParent, newParentChain, err := s.loadTarget(ctx, userTokens, current, req.NewParentID)
	if err != nil {
		return model.Node{}, err
	}

	owner, err := normalizeOwnerToken(req.Owner)
	if err != nil {
		return model.Node{}, err
	}

	effectiveLimit := current.Limit
	if len(req.Limit) > 0 {
		effectiveLimit = req.Limit
	}

	// Checked early, as if granted, so a promotion that cannot be approved
	// fails here; the authoritative check runs again at approval time.
	if err := s.checkLeafLimit(ctx, *current, newParentChain, effectiveLimit, nil, true); err != nil {
		return model.Node{}, err
	}
	terminationDate, err := fitProjectEnd(req.TerminationDate, newParentChain, time.Now())
	if err != nil {
		return model.Node{}, err
	}

	if err := validateReason(req.Reason); err != nil {
		return model.Node{}, err
	}
	// loadTarget made sure the caller manages both the chain the node leaves and the budget it goes to.
	normalizedAuthorizedUsers, err := s.normalizeAuthorizedUsers(ctx, req.AuthorizedUsers, userTokens, true)
	if err != nil {
		return model.Node{}, err
	}

	historyEntry := model.NewHistoryEntry("promote_requested", actor, model.StatusImported)
	historyEntry.ParentFrom = current.ParentID
	historyEntry.ParentTo = &newParent.ID
	historyEntry.OwnerTo = &owner
	historyEntry.Reason = new(req.Reason)

	updated := *current
	updated.ParentID = &newParent.ID
	updated.Owner = owner
	updated.Reason = req.Reason
	updated.TerminationDate = terminationDate
	updated.Limit = effectiveLimit
	updated.AuthorizedUsers = normalizedAuthorizedUsers
	if !slices.Contains(updated.Flags, model.FlagPromoteOnReconcile) {
		updated.Flags = append(slices.Clone(current.Flags), model.FlagPromoteOnReconcile)
	}
	updated.History = append(slices.Clone(current.History), historyEntry)

	if err := s.store.UpsertNode(ctx, updated); err != nil {
		return model.Node{}, fmt.Errorf("persist node: %w", err)
	}
	return updated, nil
}
