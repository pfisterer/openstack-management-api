package tree

import (
	"context"
	"fmt"
	"slices"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

// ReleaseNode marks an approved leaf as released, returning its capacity and
// driving OpenStack deprovisioning on the next reconcile. The owner or a manager
// of the parent chain may release. With req.Delete the leaf is marked for
// deletion in the same write — see RequestDeletion.
func (s *Service) ReleaseNode(id string, req ReleaseNodeRequest, actor Actor, userTokens common.TokenList) (Node, error) {
	ctx, cancel := s.newCtx()
	defer cancel()

	current, err := s.loadNode(ctx, id)
	if err != nil {
		return Node{}, err
	}
	if !current.IsLeaf() {
		return Node{}, fmt.Errorf("only project leaves can be released; delete budgets instead")
	}
	if current.Status != StatusApproved {
		return Node{}, fmt.Errorf("%w: cannot release node in status %q", common.ErrConflict, current.Status)
	}
	if req.Delete && !s.deletionAllowed {
		return Node{}, fmt.Errorf("%w: projects cannot be deleted here", common.ErrConflict)
	}
	if err := s.authorizeRetirement(ctx, current, userTokens); err != nil {
		return Node{}, err
	}

	historyEntry := newHistoryEntry("released", actor, StatusReleased)
	historyEntry.StatusFrom = new(StatusApproved)
	historyEntry.LimitFrom = &current.Limit

	updated := *current
	updated.Status = StatusReleased
	updated.Pending = nil
	updated.History = append(slices.Clone(current.History), historyEntry)
	if req.Delete {
		updated.Flags = append(slices.Clone(current.Flags), FlagDeleteRequested)
		updated.History = append(updated.History, newHistoryEntry("deletion_requested", actor, StatusReleased))
	}

	if err := s.store.UpsertNode(ctx, updated); err != nil {
		return Node{}, fmt.Errorf("persist node: %w", err)
	}
	return updated, nil
}

// RequestDeletion marks a released or archived leaf for deletion: the
// reconciler empties its OpenStack project on the next passes — servers,
// volumes, snapshots, images, networks, everything — deletes it and then the
// leaf. Not reversible once the reconciler has started. Allowed only where a
// reconciler is configured to act on it (SetDeletionAllowed), and to whoever may
// release: the owner, the project's admins and the managers above it.
func (s *Service) RequestDeletion(id string, actor Actor, userTokens common.TokenList) (Node, error) {
	if !s.deletionAllowed {
		return Node{}, fmt.Errorf("%w: projects cannot be deleted here", common.ErrConflict)
	}
	ctx, cancel := s.newCtx()
	defer cancel()

	current, err := s.loadNode(ctx, id)
	if err != nil {
		return Node{}, err
	}
	if !current.IsLeaf() || !slices.Contains(RetiredStatuses, current.Status) {
		return Node{}, fmt.Errorf("%w: only released or archived projects can be deleted", common.ErrConflict)
	}
	if err := s.authorizeRetirement(ctx, current, userTokens); err != nil {
		return Node{}, err
	}
	if slices.Contains(current.Flags, FlagDeleteRequested) {
		return *current, nil
	}

	var updated Node
	if _, err := s.store.UpdateNode(ctx, id, func(n *Node) error {
		if !slices.Contains(RetiredStatuses, n.Status) {
			return fmt.Errorf("%w: only released or archived projects can be deleted", common.ErrConflict)
		}
		if !slices.Contains(n.Flags, FlagDeleteRequested) {
			n.Flags = append(slices.Clone(n.Flags), FlagDeleteRequested)
			n.History = append(slices.Clone(n.History), newHistoryEntry("deletion_requested", actor, n.Status))
		}
		updated = *n
		return nil
	}); err != nil {
		return Node{}, err
	}
	return updated, nil
}

// authorizeRetirement: the owner, the project's own admins and the managers
// above it. Inclusive (managesNode) on purpose: releasing and deleting need no
// approval, so the admins the owner chose may end the project as the owner may.
func (s *Service) authorizeRetirement(ctx context.Context, current *Node, userTokens common.TokenList) error {
	if isOwner(userTokens, current) {
		return nil
	}
	manages, err := s.managesNode(ctx, userTokens, current)
	if err != nil {
		return err
	}
	if !manages {
		return common.ErrForbidden
	}
	return nil
}

// SetDeletionAllowed says whether released and archived projects may be marked
// for deletion. Set at startup from the reconciler's configuration: a mark
// nobody acts on would leave a project "being deleted" for ever.
func (s *Service) SetDeletionAllowed(allowed bool) { s.deletionAllowed = allowed }

// DeleteNode removes a budget subtree. Refused while any node in the subtree is
// still pending, awaiting a change decision, active, or an unpromoted import —
// those must be decided/released first so nothing is silently discarded.
func (s *Service) DeleteNode(id string, actor Actor, userTokens common.TokenList) error {
	_ = actor
	ctx, cancel := s.newCtx()
	defer cancel()

	current, err := s.loadNode(ctx, id)
	if err != nil {
		return err
	}
	if current.Kind != KindBudget {
		return fmt.Errorf("project leaves are not deleted; reject or release them instead")
	}
	if current.ID == RootNodeID || current.ID == UnassignedNodeID {
		return fmt.Errorf("structural nodes cannot be deleted: %w", common.ErrForbidden)
	}

	if manages, err := s.managesNode(ctx, userTokens, current); err != nil {
		return err
	} else if !manages {
		return common.ErrForbidden
	}

	// Collect the budget subtree, then every node inside it.
	parentMap, err := s.buildSubtreeParentMap(ctx, []Node{*current})
	if err != nil {
		return fmt.Errorf("collect budget subtree: %w", err)
	}
	budgetIDs := make([]string, 0, len(parentMap))
	for budgetID := range parentMap {
		budgetIDs = append(budgetIDs, budgetID)
	}
	descendants, err := s.store.ListNodes(ctx, NodeQuery{ParentIDs: budgetIDs}, 0, 0)
	if err != nil {
		return fmt.Errorf("load subtree nodes: %w", err)
	}

	deleteIDs := append([]string{}, budgetIDs...)
	for _, n := range descendants {
		blocked := n.Status == StatusPending || n.Status == StatusChangePending ||
			n.Status == StatusImported ||
			(n.IsLeaf() && n.Status == StatusApproved)
		if blocked {
			return fmt.Errorf("cannot delete: node %q (%s) is in status %q — decide or release it first", n.ID, n.Name, n.Status)
		}
		if n.Kind != KindBudget { // budgets are already in deleteIDs via parentMap
			deleteIDs = append(deleteIDs, n.ID)
		}
	}

	if err := s.store.DeleteNodes(ctx, deleteIDs); err != nil {
		return fmt.Errorf("delete subtree: %w", err)
	}
	return nil
}
