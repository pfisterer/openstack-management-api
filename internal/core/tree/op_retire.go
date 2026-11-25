package tree

import (
	"context"
	"fmt"
	"slices"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/nodestore"
)

// ReleaseNode ends something that pays or is asked for.
//
// On a project: an approved project is released, returning what all its allocations hold and driving its deprovisioning on the next reconcile; its allocations go with it (released, a request still waiting is withdrawn). Its owner, its admins and whoever manages every budget paying for it may. With req.Delete the project is marked for deletion in the same write — see RequestDeletion.
//
// On an allocation: the allocation stops paying for its project (ended), which then shrinks by it; a request still waiting is withdrawn. The project's owner and admins may give it back, the managers of its budget (or above) may withdraw it — nobody else, in particular not the managers of another budget paying for the same project (decided 25.11.2026).
func (s *Service) ReleaseNode(id string, req ReleaseNodeRequest, actor model.Actor, userTokens common.TokenList) (model.Node, error) {
	// Releasing changes what a budget is charged for, which the approvals must not see half done.
	s.approvalMu.Lock()
	defer s.approvalMu.Unlock()

	ctx, cancel := s.newCtx()
	defer cancel()

	current, err := s.loadNode(ctx, id)
	if err != nil {
		return model.Node{}, err
	}
	if current.IsLeaf() {
		return s.endAllocation(ctx, current, actor, userTokens)
	}
	if !current.IsProject() {
		return model.Node{}, invalid("Nur Projekte lassen sich löschen und beenden; ein Budget wird stattdessen als Budget gelöscht.", "Only projects can be deleted and ended; a budget is deleted as a budget instead.")
	}
	if current.Status != model.StatusApproved {
		return model.Node{}, refuse(common.ErrConflict, "Im Status %q lässt sich das Projekt nicht löschen.", "The project cannot be deleted in status %q.", current.Status)
	}
	if req.Delete && !s.deletable(*current) {
		return model.Node{}, refuse(common.ErrConflict, "Projekte lassen sich hier nicht endgültig löschen.", "Projects cannot be deleted for good here.")
	}
	if err := s.authorizeProjectLevel(ctx, userTokens, current); err != nil {
		return model.Node{}, err
	}

	// Written on the node as it is now, with the status checked again there: the reconciler's last report is kept, and a node that left approved meanwhile is not released.
	var updated model.Node
	if _, err := s.store.UpdateNode(ctx, id, func(n *model.Node) error {
		if n.Status != model.StatusApproved {
			return refuse(common.ErrConflict, "Im Status %q lässt sich das Projekt nicht löschen.", "The project cannot be deleted in status %q.", n.Status)
		}
		historyEntry := model.NewHistoryEntry("released", actor, model.StatusReleased)
		historyEntry.StatusFrom = new(model.StatusApproved)
		historyEntry.LimitFrom = new(n.Limit)
		n.Status = model.StatusReleased
		n.Pending = nil
		n.History = append(slices.Clone(n.History), historyEntry)
		if req.Delete && !slices.Contains(n.Flags, model.FlagDeleteRequested) {
			n.Flags = append(slices.Clone(n.Flags), model.FlagDeleteRequested)
			n.History = append(n.History, model.NewHistoryEntry("deletion_requested", actor, model.StatusReleased))
		}
		updated = *n
		return nil
	}); err != nil {
		return model.Node{}, err
	}
	if updated.ID == "" {
		return model.Node{}, errNodeNotFound
	}
	allocs, err := s.allocationsOf(ctx, id)
	if err != nil {
		return model.Node{}, err
	}
	for _, a := range allocs[id] {
		to := model.StatusReleased
		switch {
		case a.Status == model.StatusPending:
			to = model.StatusEnded
		case !isPaying(a):
			continue
		}
		if err := s.setAllocationStatus(ctx, a.ID, to, "released", actor, "Mit dem Projekt beendet"); err != nil {
			return model.Node{}, err
		}
	}
	if p, err := s.refreshProject(ctx, id, actor); err == nil {
		updated = *p
	}
	return updated, nil
}

// endAllocation is ReleaseNode on an allocation. Callers hold approvalMu.
func (s *Service) endAllocation(ctx context.Context, alloc *model.Node, actor model.Actor, userTokens common.TokenList) (model.Node, error) {
	if !slices.Contains(liveAllocationStatuses, alloc.Status) {
		return model.Node{}, refuse(common.ErrConflict, "Im Status %q lässt sich die Zuteilung nicht beenden.", "The allocation cannot be ended in status %q.", alloc.Status)
	}
	project, err := s.projectOf(ctx, alloc)
	if err != nil {
		return model.Node{}, err
	}
	holder := holdsProject(userTokens, project)
	manager, err := s.managesParentChain(ctx, userTokens, alloc)
	if err != nil {
		return model.Node{}, err
	}
	if !holder && !manager {
		return model.Node{}, errNoPermission
	}
	reason := "Von den Verantwortlichen des Projekts zurückgegeben"
	if !holder {
		reason = "Von den Verwaltenden des Budgets entzogen"
	}
	if alloc.Status == model.StatusPending {
		reason = "Anfrage zurückgezogen"
	}
	if err := s.setAllocationStatus(ctx, alloc.ID, model.StatusEnded, "allocation_ended", actor, reason); err != nil {
		return model.Node{}, err
	}
	if _, err := s.refreshProject(ctx, project.ID, actor); err != nil {
		return model.Node{}, err
	}
	ended, err := s.loadNode(ctx, alloc.ID)
	if err != nil {
		return model.Node{}, err
	}
	if !holder && isPaying(*alloc) {
		s.notifyShortened(ctx, *ended, *project, actor, reason)
	}
	out, err := s.attachUsage(ctx, []model.Node{*ended}, userTokens)
	if err != nil {
		return *ended, nil
	}
	return out[0], nil
}

// setAllocationStatus moves an allocation to status with a history entry, dropping a waiting change and resolving its approval notification. Callers hold approvalMu and refresh the project afterwards.
func (s *Service) setAllocationStatus(ctx context.Context, id, status, event string, actor model.Actor, reason string) error {
	var before model.Node
	if _, err := s.store.UpdateNode(ctx, id, func(n *model.Node) error {
		before = *n
		entry := model.NewHistoryEntry(event, actor, status)
		entry.StatusFrom = new(n.Status)
		if reason != "" {
			entry.Reason = new(reason)
		}
		n.Status = status
		n.Pending = nil
		if status == model.StatusEnded {
			n.Expired = nil
		}
		n.WaitingKey = ""
		n.History = append(slices.Clone(n.History), entry)
		return nil
	}); err != nil {
		return fmt.Errorf("update allocation %s: %w", id, err)
	}
	s.resolveWaiting(ctx, before, actor)
	return nil
}

// RequestDeletion marks a released or archived leaf for deletion: the
// reconciler empties its OpenStack project on the next passes — servers,
// volumes, snapshots, images, networks, everything — deletes it and then the
// leaf. Not reversible once the reconciler has started. Allowed only where a
// reconciler is configured to act on it (SetDeletionAllowed), and to whoever may
// release: the owner, the project's admins and the managers above it.
func (s *Service) RequestDeletion(id string, actor model.Actor, userTokens common.TokenList) (model.Node, error) {
	ctx, cancel := s.newCtx()
	defer cancel()

	current, err := s.loadNode(ctx, id)
	if err != nil {
		return model.Node{}, err
	}
	if !s.deletable(*current) {
		return model.Node{}, refuse(common.ErrConflict, "Projekte lassen sich hier nicht endgültig löschen.", "Projects cannot be deleted for good here.")
	}
	if !current.IsProject() || !slices.Contains(model.RetiredStatuses, current.Status) {
		return model.Node{}, refuse(common.ErrConflict, "Endgültig löschen lassen sich nur gelöschte oder stillgelegte Projekte.", "Only deleted or archived projects can be deleted for good.")
	}
	if err := s.authorizeProjectLevel(ctx, userTokens, current); err != nil {
		return model.Node{}, err
	}
	if slices.Contains(current.Flags, model.FlagDeleteRequested) {
		return *current, nil
	}

	var updated model.Node
	if _, err := s.store.UpdateNode(ctx, id, func(n *model.Node) error {
		if !slices.Contains(model.RetiredStatuses, n.Status) {
			return refuse(common.ErrConflict, "Endgültig löschen lassen sich nur gelöschte oder stillgelegte Projekte.", "Only deleted or archived projects can be deleted for good.")
		}
		if !slices.Contains(n.Flags, model.FlagDeleteRequested) {
			n.Flags = append(slices.Clone(n.Flags), model.FlagDeleteRequested)
			n.History = append(slices.Clone(n.History), model.NewHistoryEntry("deletion_requested", actor, n.Status))
		}
		updated = *n
		return nil
	}); err != nil {
		return model.Node{}, err
	}
	return updated, nil
}

// SetDeletingProviders names the providers that act on a deletion request,
// set at startup from their configuration: a mark nobody acts on would leave a
// project "being deleted" for ever.
func (s *Service) SetDeletingProviders(providers ...string) { s.deleting = providers }

// deletable: every provider that carries the leaf out — or would, by its
// resources — deletes on request. A leaf no provider carries out may be marked
// wherever any provider deletes.
func (s *Service) deletable(n model.Node) bool {
	concerned := map[string]bool{}
	for p := range n.Bindings {
		concerned[p] = true
	}
	limit := n.EffectiveLimit()
	for _, r := range s.resources() {
		if r.Provider != "" && limit.Holds(r.ID) {
			concerned[r.Provider] = true
		}
	}
	if len(concerned) == 0 {
		return len(s.deleting) > 0
	}
	for p := range concerned {
		if !slices.Contains(s.deleting, p) {
			return false
		}
	}
	return true
}

// DeleteNode removes a budget subtree. Refused while a budget in the subtree is
// still pending or awaiting a change decision — those must be decided first so
// nothing is silently discarded.
//
// The allocations below are not in the way (decided 25.11.2026): a request still waiting is withdrawn, a waiting change dropped, and every allocation that pays — or paid for a project given up that still holds its resources — moves under the budget above the deleted one, marked as expired because its budget was deleted. The project keeps its resources (overbooking is accepted there), its owner and managers are told, and the allocation is extended elsewhere or cut after the period (SweepExpiry). Ended and rejected allocations go with the subtree.
//
// Under the approval lock: a request created under the budget while its subtree is being collected would otherwise be deleted with it, unseen. Nothing of the subtree remains to carry a history entry, so who deleted it is written to the log.
func (s *Service) DeleteNode(id string, actor model.Actor, userTokens common.TokenList) error {
	s.approvalMu.Lock()
	defer s.approvalMu.Unlock()

	ctx, cancel := s.newCtx()
	defer cancel()

	current, err := s.loadNode(ctx, id)
	if err != nil {
		return err
	}
	if current.Kind != model.KindBudget {
		return invalid("Ein Projekt wird nicht als Budget gelöscht, sondern abgelehnt oder gelöscht.", "A project is not deleted as a budget; it is rejected or deleted.")
	}
	if current.ID == model.RootNodeID || current.ID == model.UnassignedNodeID || current.ParentID == nil {
		return refuse(common.ErrForbidden, "Die Struktur-Knoten (Wurzel und Nicht zugeordnet) lassen sich nicht löschen.", "The structural nodes (root and Unassigned) cannot be deleted.")
	}

	if manages, err := s.managesNode(ctx, userTokens, current); err != nil {
		return err
	} else if !manages {
		return errNoPermission
	}

	// Collect the budget subtree, then every node inside it.
	parentMap, err := s.buildSubtreeParentMap(ctx, []model.Node{*current})
	if err != nil {
		return fmt.Errorf("collect budget subtree: %w", err)
	}
	budgetIDs := make([]string, 0, len(parentMap))
	for budgetID := range parentMap {
		budgetIDs = append(budgetIDs, budgetID)
	}
	descendants, err := s.store.ListNodes(ctx, nodestore.NodeQuery{ParentIDs: budgetIDs}, 0, 0)
	if err != nil {
		return fmt.Errorf("load subtree nodes: %w", err)
	}

	deleteIDs := append([]string{}, budgetIDs...)
	var moving []model.Node
	for _, n := range descendants {
		if n.Kind == model.KindBudget {
			if n.Status == model.StatusPending || n.Status == model.StatusChangePending {
				return invalid("Löschen nicht möglich: Über das Budget %q (%s) ist noch nicht entschieden.", "Cannot delete: the budget %q (%s) has not been decided on yet.", n.ID, n.Name)
			}
			continue // in deleteIDs via parentMap
		}
		if n.Kind != model.KindAllocation {
			return invalid("Löschen nicht möglich: %q (%s) ist im Status %q.", "Cannot delete: %q (%s) is in status %q.", n.ID, n.Name, n.Status)
		}
		switch {
		case n.Status == model.StatusPending, n.Status == model.StatusChangePending, isPaying(n), model.IsRetiredStatus(n.Status):
			moving = append(moving, n)
		default:
			deleteIDs = append(deleteIDs, n.ID)
		}
	}

	// The budget above: where what still pays goes.
	above := *current.ParentID
	budgetNames := map[string]string{}
	for _, b := range append(s.mustList(ctx, budgetIDs), *current) {
		budgetNames[b.ID] = nodeLabel(b)
	}
	touched := map[string]bool{}
	var expired []model.Node
	for _, a := range moving {
		touched[a.ProjectID] = true
		if a.Status == model.StatusPending {
			if err := s.setAllocationStatus(ctx, a.ID, model.StatusEnded, "allocation_ended", actor, fmt.Sprintf("Das Budget %q wurde gelöscht", budgetNames[derefParent(a.ParentID)])); err != nil {
				return err
			}
			if _, err := s.store.UpdateNode(ctx, a.ID, func(n *model.Node) error { n.ParentID = &above; return nil }); err != nil {
				return err
			}
			continue
		}
		var before model.Node
		if _, err := s.store.UpdateNode(ctx, a.ID, func(n *model.Node) error {
			before = *n
			from := derefParent(n.ParentID)
			entry := model.NewHistoryEntry("budget_deleted", actor, n.Status)
			if n.Status == model.StatusChangePending {
				entry.StatusTo = model.StatusApproved
				entry.StatusFrom = new(model.StatusChangePending)
				n.Status = model.StatusApproved
			}
			entry.ParentFrom = &from
			entry.ParentTo = &above
			entry.Reason = new(fmt.Sprintf("Das Budget %q wurde gelöscht; die Zuteilung hängt jetzt am Budget darüber und ist als abgelaufen markiert", budgetNames[from]))
			n.Pending = nil
			n.WaitingKey = ""
			n.ParentID = &above
			if isPaying(*n) {
				n.Expired = &model.Expiry{At: entry.Timestamp, Cause: model.ExpiryBudgetDeleted, BudgetName: budgetNames[from], Due: s.expiryDue()}
			}
			n.History = append(slices.Clone(n.History), entry)
			return nil
		}); err != nil {
			return fmt.Errorf("move allocation %s: %w", a.ID, err)
		}
		s.resolveWaiting(ctx, before, actor)
		if isPaying(before) {
			if moved, err := s.store.GetNode(ctx, a.ID); err == nil && moved != nil {
				expired = append(expired, *moved)
			}
		}
	}

	if err := s.store.DeleteNodes(ctx, deleteIDs); err != nil {
		return fmt.Errorf("delete subtree: %w", err)
	}
	for pid := range touched {
		if _, err := s.refreshProject(ctx, pid, actor); err != nil {
			return err
		}
	}
	for _, a := range expired {
		if p, err := s.store.GetNode(ctx, a.ProjectID); err == nil && p != nil {
			s.notifyExpired(ctx, a, *p, actor)
		}
	}
	if s.log != nil {
		s.log.Infow("budget subtree deleted", "node_id", current.ID, "name", current.Name, "actor", actor.Email, "acted_by", actor.ActedBy, "via", actor.Channel(), "deleted_ids", deleteIDs, "allocations_moved", len(moving), "moved_to", above)
	}
	return nil
}

// mustList loads the nodes with the given ids, an empty list where that fails (only names are read from it).
func (s *Service) mustList(ctx context.Context, ids []string) []model.Node {
	out, err := s.store.ListNodes(ctx, nodestore.NodeQuery{IDs: ids}, 0, 0)
	if err != nil {
		return nil
	}
	return out
}
