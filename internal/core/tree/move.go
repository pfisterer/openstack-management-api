package tree

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

// ReparentNode moves a node under a new parent budget. Requires management rights
// on the node's CURRENT parent chain (you give it away) AND on the new parent (you
// receive it). Active nodes are capacity-checked against the new ancestor chain.
func (s *Service) ReparentNode(id string, req ReparentNodeRequest, actor Actor, userTokens common.TokenList) (Node, error) {
	s.approvalMu.Lock()
	defer s.approvalMu.Unlock()

	ctx, cancel := s.newCtx()
	defer cancel()

	current, err := s.loadLive(ctx, id)
	if err != nil {
		return Node{}, err
	}
	if current.ID == RootNodeID || current.ID == UnassignedNodeID {
		return Node{}, fmt.Errorf("structural nodes cannot be moved: %w", common.ErrForbidden)
	}
	if current.ParentID != nil && *current.ParentID == req.NewParentID {
		return Node{}, fmt.Errorf("%w: node is already under this parent", common.ErrConflict)
	}

	newParent, newParentChain, err := s.loadTarget(ctx, userTokens, current, req.NewParentID)
	if err != nil {
		return Node{}, err
	}
	// Cycle guard: the new parent must not lie inside the node's own subtree.
	for _, ancestor := range newParentChain {
		if ancestor.ID == current.ID {
			return Node{}, fmt.Errorf("cannot move a node into its own subtree")
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
			if current.Status == StatusArchived {
				need, _ = s.leafCost(*current)
			}
			// What it costs is charged; a retired project's limit is no
			// longer granted, so only an active one is checked against it.
			if err := s.checkCapacity(ctx, newParentChain, need, nil); err != nil {
				return Node{}, err
			}
			if current.Status == StatusApproved || current.Status == StatusChangePending {
				if err := s.checkLeafLimit(ctx, *current, newParentChain, current.Limit, nil, false); err != nil {
					return Node{}, err
				}
			}
		} else {
			// One that inherits has no limit to fit; what is used below it
			// is checked next.
			if !current.InheritsLimit {
				if err := s.checkBudgetLimit(ctx, *current, newParentChain, current.Limit, false); err != nil {
					return Node{}, err
				}
			}
			subtreeUsage, err := s.loadSubtreeUsage(ctx, []Node{*current})
			if err != nil {
				return Node{}, fmt.Errorf("compute subtree usage for move: %w", err)
			}
			moved := subtreeUsage[current.ID].Total(s.countIDs)
			if err := s.checkCapacity(ctx, newParentChain, moved, nil); err != nil {
				return Node{}, err
			}
		}
	}

	// An allocation has to keep coming from above.
	if err := s.checkAllocationsAfterMove(ctx, *current, newParentChain); err != nil {
		return Node{}, err
	}

	historyEntry := newHistoryEntry("reparented", actor, current.Status)
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
		if err := shortenEnd(&updated, *bound, actor, reason); err != nil && !errors.Is(err, ErrSkipUpdate) {
			return Node{}, err
		}
	}
	if maxTerm != nil && !updated.IsLeaf() {
		if err := shortenTerm(&updated, *maxTerm, actor, termReason(*newParent, *maxTerm)); err != nil && !errors.Is(err, ErrSkipUpdate) {
			return Node{}, err
		}
	}

	if err := s.store.UpsertNode(ctx, updated); err != nil {
		return Node{}, fmt.Errorf("persist node: %w", err)
	}
	if bound != nil && !updated.IsLeaf() {
		if _, err := s.shortenSubtreeEnds(ctx, []string{updated.ID}, *bound, actor, reason); err != nil {
			return Node{}, err
		}
	}
	if updated.MaxProjectTermDays != nil && !updated.IsLeaf() {
		days := *updated.MaxProjectTermDays
		if _, err := s.shortenSubtreeTerms(ctx, []string{updated.ID}, days, now, actor, termReason(updated, days)); err != nil {
			return Node{}, err
		}
	}
	return updated, nil
}

// TransferOwner hands a leaf to a new responsible person. Only managers of the
// parent chain may transfer (e.g. when the current owner leaves the organization).
func (s *Service) TransferOwner(id string, req TransferOwnerRequest, actor Actor, userTokens common.TokenList) (Node, error) {
	ctx, cancel := s.newCtx()
	defer cancel()

	current, err := s.loadLive(ctx, id)
	if err != nil {
		return Node{}, err
	}
	if !current.IsLeaf() {
		return Node{}, fmt.Errorf("only project leaves have an owner")
	}

	if manages, err := s.managesParentChain(ctx, userTokens, current); err != nil {
		return Node{}, err
	} else if !manages {
		return Node{}, common.ErrForbidden
	}

	newOwner, err := normalizeOwnerToken(req.NewOwner)
	if err != nil {
		return Node{}, err
	}
	if newOwner == current.Owner {
		return Node{}, fmt.Errorf("%w: node is already owned by %s", common.ErrConflict, newOwner)
	}

	historyEntry := newHistoryEntry("owner_transferred", actor, current.Status)
	historyEntry.OwnerFrom = &current.Owner
	historyEntry.OwnerTo = &newOwner

	updated := *current
	updated.Owner = newOwner
	updated.History = append(slices.Clone(current.History), historyEntry)

	if err := s.store.UpsertNode(ctx, updated); err != nil {
		return Node{}, fmt.Errorf("persist node: %w", err)
	}
	return updated, nil
}

// PromoteNode converts an imported leaf into a managed request: it is reparented
// under req.NewParentID, gets an owner and the promote flag. The reconciler then
// tags the existing OpenStack project and transitions the leaf to pending, after
// which the normal approval cycle applies. Requires management rights on the
// unassigned chain (root admins) and on the target budget.
func (s *Service) PromoteNode(id string, req PromoteNodeRequest, actor Actor, userTokens common.TokenList) (Node, error) {
	s.approvalMu.Lock()
	defer s.approvalMu.Unlock()

	ctx, cancel := s.newCtx()
	defer cancel()

	current, err := s.loadNode(ctx, id)
	if err != nil {
		return Node{}, err
	}
	if current.Status != StatusImported {
		return Node{}, fmt.Errorf("only imported nodes can be promoted: %w", common.ErrForbidden)
	}

	newParent, newParentChain, err := s.loadTarget(ctx, userTokens, current, req.NewParentID)
	if err != nil {
		return Node{}, err
	}

	owner, err := normalizeOwnerToken(req.Owner)
	if err != nil {
		return Node{}, err
	}

	effectiveLimit := current.Limit
	if len(req.Limit) > 0 {
		effectiveLimit = req.Limit
	}

	// Checked early, as if granted, so a promotion that cannot be approved
	// fails here; the authoritative check runs again at approval time.
	if err := s.checkLeafLimit(ctx, *current, newParentChain, effectiveLimit, nil, true); err != nil {
		return Node{}, err
	}
	terminationDate, err := fitProjectEnd(req.TerminationDate, newParentChain, time.Now())
	if err != nil {
		return Node{}, err
	}

	normalizedAuthorizedUsers, err := s.normalizeAuthorizedUsers(ctx, req.AuthorizedUsers)
	if err != nil {
		return Node{}, err
	}

	historyEntry := newHistoryEntry("promote_requested", actor, StatusImported)
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
	if !slices.Contains(updated.Flags, FlagPromoteOnReconcile) {
		updated.Flags = append(slices.Clone(current.Flags), FlagPromoteOnReconcile)
	}
	updated.History = append(slices.Clone(current.History), historyEntry)

	if err := s.store.UpsertNode(ctx, updated); err != nil {
		return Node{}, fmt.Errorf("persist node: %w", err)
	}
	return updated, nil
}
