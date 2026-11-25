package tree

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

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
	// A project is not in the tree; moving it means moving its allocation, where it has a single one.
	var viaProject *model.Node
	if current.IsProject() {
		viaProject = current
		if current, err = s.soleLive(ctx, current, liveAllocationStatuses); err != nil {
			return model.Node{}, err
		}
	}
	var project *model.Node
	if current.IsLeaf() {
		if project, err = s.projectOf(ctx, current); err != nil {
			return model.Node{}, err
		}
		if model.IsTerminalStatus(project.Status) {
			return model.Node{}, refuse(common.ErrConflict, "Im Status %q lässt sich am Projekt nichts mehr ändern.", "Nothing can be changed on the project in status %q.", project.Status)
		}
	}
	if current.ID == model.RootNodeID || current.ID == model.UnassignedNodeID {
		return model.Node{}, refuse(common.ErrForbidden, "Die Struktur-Knoten (Wurzel und Nicht zugeordnet) lassen sich nicht verschieben.", "The structural nodes (root and Unassigned) cannot be moved.")
	}
	if current.ParentID != nil && *current.ParentID == req.NewParentID {
		return model.Node{}, refuse(common.ErrConflict, "Liegt bereits unter diesem Budget.", "Already under this budget.")
	}

	newParent, newParentChain, err := s.loadTarget(ctx, userTokens, current, req.NewParentID)
	if err != nil {
		return model.Node{}, err
	}
	// Cycle guard: the new parent must not lie inside the node's own subtree.
	for _, ancestor := range newParentChain {
		if ancestor.ID == current.ID {
			return model.Node{}, invalid("Ein Budget lässt sich nicht unter sich selbst verschieben.", "A budget cannot be moved under itself.")
		}
	}

	// Capacity / limit invariants against the new chain for nodes that carry
	// consumption. Pending nodes move freely — they are checked at approval.
	// The set is the accounting's, not a fixed list: a released leaf that is
	// billed must also be checked when it moves, or moving it is a way to put
	// charged usage into a budget that has no room for it.
	if slices.Contains(s.chargedStatuses(), current.Status) {
		if current.IsLeaf() {
			need := s.leafCost(*current, project)
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

	// One live allocation per budget and project.
	if project != nil {
		allocs, err := s.allocationsOf(ctx, project.ID)
		if err != nil {
			return model.Node{}, err
		}
		for _, a := range allocs[project.ID] {
			if a.ID != current.ID && derefParent(a.ParentID) == newParent.ID && slices.Contains(liveAllocationStatuses, a.Status) {
				return model.Node{}, refuse(common.ErrConflict, "Das Projekt hat bereits eine Zuteilung aus dem Budget %q.", "The project already has an allocation from the budget %q.", nodeLabel(*newParent))
			}
		}
	}

	historyEntry := model.NewHistoryEntry("reparented", actor, current.Status)
	historyEntry.ParentFrom = current.ParentID
	historyEntry.ParentTo = &newParent.ID

	updated := *current
	updated.ParentID = &newParent.ID
	updated.History = append(slices.Clone(current.History), historyEntry)
	// Moved to a budget that exists, it no longer stands for one that was deleted.
	if updated.Expired != nil && updated.Expired.Cause == model.ExpiryBudgetDeleted {
		updated.Expired = nil
	}
	// Under a budget that ends sooner, the moved node and everything below it
	// end with it; under a budget with a shorter maximum project term, the
	// projects among them are shortened to it.
	bound, term := s.endBound(updated.Kind, newParentChain)
	reason := endsWithReason(*newParent)
	if term != nil {
		reason = termReason(*newParent, *term)
	}
	maxTerm := model.ResolveMaxProjectTermDays(newParentChain)
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
		if _, err := s.shortenSubtreeTerms(ctx, []string{updated.ID}, days, s.now(), actor, termReason(updated, days)); err != nil {
			return model.Node{}, err
		}
	}
	if project != nil {
		stored, err := s.refreshProject(ctx, project.ID, actor)
		if err != nil {
			return model.Node{}, err
		}
		if viaProject != nil {
			return *stored, nil
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
				return invalid("Das Budget %q darunter passt dort nicht hinein: %w", "The budget %q below does not fit there: %w", nodeLabel(child), err)
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
	if !current.IsProject() {
		return model.Node{}, invalid("Nur Projekte haben eine verantwortliche Person.", "Only projects have a responsible person.")
	}

	// Whoever manages every budget paying for it — the managers of a single one, or a root admin.
	allocs, err := s.allocationsOf(ctx, current.ID)
	if err != nil {
		return model.Node{}, err
	}
	if manages, err := s.managesWholeProject(ctx, userTokens, allocs[current.ID]); err != nil {
		return model.Node{}, err
	} else if !manages {
		return model.Node{}, errNoPermission
	}

	newOwner, err := normalizeOwnerToken(req.NewOwner)
	if err != nil {
		return model.Node{}, err
	}

	// Written on the node as it is now, not on the copy the checks read: a reconciler pass in between must not be undone. What was checked is checked again on that state.
	var updated model.Node
	if _, err := s.store.UpdateNode(ctx, id, func(n *model.Node) error {
		if n.Status == model.StatusImported || model.IsTerminalStatus(n.Status) {
			return refuse(common.ErrConflict, "Im Status %q lässt sich nichts mehr ändern.", "Nothing can be changed in status %q.", n.Status)
		}
		if newOwner == n.Owner {
			return refuse(common.ErrConflict, "%s ist bereits verantwortlich.", "%s is already responsible.", newOwner)
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
		return model.Node{}, errNodeNotFound
	}
	// A person's share is counted by the owner on the allocations.
	if err := s.syncAllocations(ctx, updated); err != nil {
		return model.Node{}, err
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

// PromoteNode turns an imported project into a managed request: it gets an owner, members, a purpose and a waiting allocation under req.NewParentID, and the promote flag. The reconciler then tags the existing OpenStack project and moves the project to pending, after which its allocation is decided like any request. Requires managing the unassigned node (root admins) and the target budget.
func (s *Service) PromoteNode(id string, req PromoteNodeRequest, actor model.Actor, userTokens common.TokenList) (model.Node, error) {
	var err error
	if req.TerminationDate, err = normalizeEnd(req.TerminationDate); err != nil {
		return model.Node{}, err
	}
	s.approvalMu.Lock()
	defer s.approvalMu.Unlock()

	ctx, cancel := s.newCtx()
	defer cancel()

	current, err := s.loadNode(ctx, id)
	if err != nil {
		return model.Node{}, err
	}
	if current.Status != model.StatusImported || !current.IsProject() {
		return model.Node{}, refuse(common.ErrForbidden, "Nur importierte Projekte lassen sich übernehmen.", "Only imported projects can be adopted.")
	}
	caller := common.NewTokenSet(userTokens)
	if manages, err := s.managesChainFrom(ctx, caller, new(model.UnassignedNodeID)); err != nil {
		return model.Node{}, err
	} else if !manages {
		return model.Node{}, errNoPermission
	}
	newParent, newParentChain, err := s.allocationTarget(ctx, req.NewParentID)
	if err != nil {
		return model.Node{}, err
	}
	if !managesInChain(caller, newParentChain) {
		return model.Node{}, errNoPermission
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
	if err := s.checkLeafLimit(ctx, model.Node{}, newParentChain, effectiveLimit, nil, true); err != nil {
		return model.Node{}, err
	}
	terminationDate, err := s.fitNodeEnd(model.KindAllocation, req.TerminationDate, newParentChain)
	if err != nil {
		return model.Node{}, err
	}

	if err := validateReason(req.Reason); err != nil {
		return model.Node{}, err
	}
	// An import carries the binding of the provider it came from, and with it that provider's ceiling.
	normalizedAuthorizedUsers, err := s.normalizeAuthorizedUsers(ctx, req.AuthorizedUsers, s.memberCeilingOf(*current, req.Limit), userTokens, true)
	if err != nil {
		return model.Node{}, err
	}

	historyEntry := model.NewHistoryEntry("promote_requested", actor, model.StatusImported)
	historyEntry.ParentTo = &newParent.ID
	historyEntry.OwnerTo = &owner
	historyEntry.Reason = new(req.Reason)

	updated := *current
	updated.ParentID = nil
	updated.Owner = owner
	updated.Reason = req.Reason
	updated.TerminationDate = terminationDate
	updated.AuthorizedUsers = normalizedAuthorizedUsers
	updated.StartWhen = model.StartNow
	if !slices.Contains(updated.Flags, model.FlagPromoteOnReconcile) {
		updated.Flags = append(slices.Clone(current.Flags), model.FlagPromoteOnReconcile)
	}
	updated.History = append(slices.Clone(current.History), historyEntry)

	if err := s.store.UpsertNode(ctx, updated); err != nil {
		return model.Node{}, fmt.Errorf("persist node: %w", err)
	}
	alloc, err := s.placeAllocation(ctx, updated, *newParent, newParentChain, effectiveLimit, terminationDate, req.Reason, false, false, actor, strings.TrimPrefix(owner, "user:"))
	if err != nil {
		_ = s.store.UpsertNode(ctx, *current)
		return model.Node{}, err
	}
	// Waiting until the reconciler has adopted the OpenStack project; decided then like any request.
	if alloc.Status != model.StatusPending {
		if err := s.setAllocationStatus(ctx, alloc.ID, model.StatusPending, "waiting_for_adoption", actor, ""); err != nil {
			return model.Node{}, err
		}
	}
	updated.HomeBudgetID, updated.HomeAllocationID = &newParent.ID, alloc.ID
	if _, err := s.store.UpdateNode(ctx, updated.ID, func(n *model.Node) error {
		n.HomeBudgetID, n.HomeAllocationID = &newParent.ID, alloc.ID
		return nil
	}); err != nil {
		return model.Node{}, err
	}
	return updated, nil
}
