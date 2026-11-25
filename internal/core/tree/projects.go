package tree

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/nodestore"
)

// Projects and their allocations (allocation_leaves, decided 25.11.2026).
//
// A project is a node of its own outside the tree; what it holds comes from its allocations, each a leaf directly under the one budget that pays for it and subject to that budget's rules alone: auto-approve, a person's share (counted per budget), the term, its managers. The project's Limit is the sum of the allocations that pay, cached on the project so that the providers, the usage books and every reader of a project keep reading one node (refreshProject keeps it right). Its owner manages the whole project; the managers of a budget see the project in their table and change, approve or withdraw their own allocation only.

// liveAllocationStatuses are the allocation states that still mean something for the project: asked for, or paying.
var liveAllocationStatuses = []string{model.StatusPending, model.StatusApproved, model.StatusChangePending}

// isPaying reports whether an allocation pays for its project now.
func isPaying(a model.Node) bool { return slices.Contains(model.ActiveStatuses, a.Status) }

// allocationsOf loads the allocations of the given projects, every status, by project.
func (s *Service) allocationsOf(ctx context.Context, projectIDs ...string) (map[string][]model.Node, error) {
	out := map[string][]model.Node{}
	if len(projectIDs) == 0 {
		return out, nil
	}
	list, err := s.store.ListNodes(ctx, nodestore.NodeQuery{Kinds: []string{model.KindAllocation}, ProjectIDs: projectIDs}, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("load allocations: %w", err)
	}
	for _, a := range list {
		out[a.ProjectID] = append(out[a.ProjectID], a)
	}
	for id := range out {
		sortAllocations(out[id])
	}
	return out, nil
}

// sortAllocations orders allocations as a project shows them: live ones first, then by age.
func sortAllocations(allocs []model.Node) {
	rank := func(a model.Node) int {
		switch {
		case isPaying(a):
			return 0
		case a.Status == model.StatusPending:
			return 1
		case model.IsRetiredStatus(a.Status):
			return 2
		default:
			return 3
		}
	}
	slices.SortStableFunc(allocs, func(a, b model.Node) int {
		return cmp.Or(cmp.Compare(rank(a), rank(b)), cmp.Compare(a.CreatedAt, b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
}

// loadProject loads a project by id; anything else is not found.
func (s *Service) loadProject(ctx context.Context, id string) (*model.Node, error) {
	p, err := s.loadNode(ctx, id)
	if err != nil {
		return nil, err
	}
	if !p.IsProject() {
		return nil, invalid("%q ist kein Projekt.", "%q is not a project.", id)
	}
	return p, nil
}

// projectOf loads the project an allocation pays for.
func (s *Service) projectOf(ctx context.Context, alloc *model.Node) (*model.Node, error) {
	p, err := s.store.GetNode(ctx, alloc.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("load project: %w", err)
	}
	if p == nil {
		return nil, refuse(common.ErrNotFound, "Das Projekt der Zuteilung wurde nicht gefunden.", "The project of the allocation was not found.")
	}
	return p, nil
}

// holdsProject reports whether the caller answers for the project: its owner or one of its admins. They manage the whole project — every allocation of it included, which they may give back but not raise beyond what the budget's rules grant.
func holdsProject(userTokens common.TokenList, project *model.Node) bool {
	return isOwner(userTokens, project) || common.NewTokenSet(userTokens).ContainsAny(project.AdminScope)
}

// managesWholeProject reports whether the caller manages the budget of every live allocation of the project (from that budget or above) — and it has one. That is the right a manager of the single budget of a project had before allocations: transferring it, deciding on it as a whole. Root admins manage every project this way.
func (s *Service) managesWholeProject(ctx context.Context, userTokens common.TokenList, allocs []model.Node) (bool, error) {
	caller := common.NewTokenSet(userTokens)
	counted := slices.DeleteFunc(slices.Clone(allocs), func(a model.Node) bool {
		return !slices.Contains(liveAllocationStatuses, a.Status) && !model.IsRetiredStatus(a.Status)
	})
	// A project nothing pays for any more is managed from where it was paid.
	if len(counted) == 0 {
		counted = allocs
	}
	for _, a := range counted {
		chain, err := s.parentChainNodes(ctx, &a)
		if err != nil {
			return false, err
		}
		if !managesInChain(caller, chain) {
			return false, nil
		}
	}
	return len(counted) > 0, nil
}

// authorizeProjectLevel admits who may act on a project as a whole without approval — release it, delete it, end external access: its owner and admins, and whoever manages every budget paying for it (managesWholeProject).
func (s *Service) authorizeProjectLevel(ctx context.Context, userTokens common.TokenList, project *model.Node) error {
	if holdsProject(userTokens, project) {
		return nil
	}
	allocs, err := s.allocationsOf(ctx, project.ID)
	if err != nil {
		return err
	}
	ok, err := s.managesWholeProject(ctx, userTokens, allocs[project.ID])
	if err != nil {
		return err
	}
	if !ok {
		return errNoPermission
	}
	return nil
}

// managesSomeAllocation reports whether the caller manages the budget (or one above) of any allocation of the project: they see it in their budget table, and so may read it.
func (s *Service) managesSomeAllocation(ctx context.Context, userTokens common.TokenList, allocs []model.Node) (bool, error) {
	caller := common.NewTokenSet(userTokens)
	for _, a := range allocs {
		chain, err := s.parentChainNodes(ctx, &a)
		if err != nil {
			return false, err
		}
		if managesInChain(caller, chain) {
			return true, nil
		}
	}
	return false, nil
}

// refreshProject recomputes what a project derives from its allocations (applyAllocations) and writes it when anything changed, on the project as it is stored now. Every write of an allocation is followed by it, under approvalMu. Returns the project as stored afterwards.
func (s *Service) refreshProject(ctx context.Context, projectID string, actor model.Actor) (*model.Node, error) {
	allocs, err := s.allocationsOf(ctx, projectID)
	if err != nil {
		return nil, err
	}
	var out *model.Node
	if _, err := s.store.UpdateNode(ctx, projectID, func(p *model.Node) error {
		if !applyAllocations(p, allocs[projectID], actor) {
			return nodestore.ErrSkipUpdate
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("refresh project %s: %w", projectID, err)
	}
	if out, err = s.store.GetNode(ctx, projectID); err != nil {
		return nil, err
	}
	if out == nil {
		return nil, errNodeNotFound
	}
	return out, nil
}

// applyAllocations sets what a project derives from its allocations and reports whether anything changed:
//   - Limit: the sum of the allocations that pay (approved, change_pending), or for a project given up, of those that paid when it was (released, archived);
//   - TerminationDate: the latest end of those, none if one of them has none; for a project still waiting, of what it asks for;
//   - HomeAllocationID/HomeBudgetID: the oldest of those, kept while it pays (see Node.HomeBudgetID);
//   - Status: a waiting project starts once one allocation pays (StartNow) or once nothing is waiting any more and something pays (StartAll); it is rejected once nothing waits and nothing pays.
func applyAllocations(p *model.Node, allocs []model.Node, actor model.Actor) bool {
	if p.Status == model.StatusImported {
		// It mirrors OpenStack until it is adopted; what it asks for then is decided like any request.
		return false
	}
	changed := false
	statuses := nodestore.ProjectChargedStatuses(p.Status)
	if limit := nodestore.SumAllocations(allocs, statuses); !nodestore.QuotaEqual(limit, p.Limit) || p.Limit == nil {
		p.Limit = limit
		changed = true
	}
	var counted, waiting []model.Node
	for _, a := range allocs {
		switch {
		case slices.Contains(statuses, a.Status):
			counted = append(counted, a)
		case a.Status == model.StatusPending:
			waiting = append(waiting, a)
		}
	}
	ends := counted
	if len(ends) == 0 && p.Status == model.StatusPending {
		ends = waiting
	}
	if len(ends) > 0 {
		var end *string
		for i, a := range ends {
			if a.TerminationDate == nil {
				end = nil
				break
			}
			if i == 0 || !model.EndsNoLater(*a.TerminationDate, end) {
				end = a.TerminationDate
			}
		}
		if !model.SameEnd(end, p.TerminationDate) || (end == nil) != (p.TerminationDate == nil) {
			p.TerminationDate = end
			changed = true
		}
	}
	home := homeOf(p.HomeAllocationID, counted, waiting, allocs)
	if home != nil {
		if p.HomeAllocationID != home.ID || derefParent(p.HomeBudgetID) != derefParent(home.ParentID) {
			p.HomeAllocationID = home.ID
			p.HomeBudgetID = home.ParentID
			changed = true
		}
	}
	if p.Status == model.StatusPending {
		paying := slices.ContainsFunc(allocs, isPaying)
		start := paying
		if p.StartWhen == model.StartAll {
			start = paying && len(waiting) == 0
		}
		switch {
		case start:
			entry := model.NewHistoryEntry("approved", deciderOf(allocs, actor), model.StatusApproved)
			entry.StatusFrom = new(model.StatusPending)
			entry.Reason = new("Gestartet: eine Zuteilung ist genehmigt")
			if p.StartWhen == model.StartAll {
				entry.Reason = new("Gestartet: über alle Zuteilungen ist entschieden")
			}
			p.Status = model.StatusApproved
			p.History = append(slices.Clone(p.History), entry)
			p.Limit = nodestore.SumAllocations(allocs, nodestore.ProjectChargedStatuses(p.Status))
			changed = true
		case !paying && len(waiting) == 0 && len(allocs) > 0:
			entry := model.NewHistoryEntry("rejected", actor, model.StatusRejected)
			entry.StatusFrom = new(model.StatusPending)
			entry.Reason = new("Keine Zuteilung genehmigt")
			p.Status = model.StatusRejected
			p.History = append(slices.Clone(p.History), entry)
			changed = true
		}
	}
	return changed
}

// deciderOf is who made the decision that starts a project: the actor of the latest approval among its paying allocations — a manager, or the auto-approval — else fallback.
func deciderOf(allocs []model.Node, fallback model.Actor) model.Actor {
	var latest *model.HistoryEntry
	for _, a := range allocs {
		if !isPaying(a) {
			continue
		}
		for i := range a.History {
			h := &a.History[i]
			if h.Event == "approved" && (latest == nil || h.Timestamp >= latest.Timestamp) {
				latest = h
			}
		}
	}
	if latest == nil {
		return fallback
	}
	return model.Actor{Email: latest.Actor, ActedBy: latest.ActedBy, Via: latest.Via}
}

// homeOf picks the home allocation: the current one while it still counts, else the oldest that counts, else the oldest waiting, else the oldest at all; nil without allocations.
func homeOf(current string, counted, waiting, all []model.Node) *model.Node {
	for _, set := range [][]model.Node{counted, waiting, all} {
		if len(set) == 0 {
			continue
		}
		if i := slices.IndexFunc(set, func(a model.Node) bool { return a.ID == current }); i >= 0 {
			return &set[i]
		}
		oldest := slices.MinFunc(set, func(a, b model.Node) int {
			return cmp.Or(cmp.Compare(a.CreatedAt, b.CreatedAt), cmp.Compare(a.ID, b.ID))
		})
		return &oldest
	}
	return nil
}

// allocationView is how a project lists one of its allocations.
func allocationView(a model.Node, homeID string) model.Allocation {
	return model.Allocation{
		ID:              a.ID,
		BudgetID:        derefParent(a.ParentID),
		Status:          a.Status,
		Limit:           a.Limit,
		TerminationDate: a.TerminationDate,
		Pending:         a.Pending,
		Reason:          a.Reason,
		CreatedBy:       a.CreatedBy,
		CreatedAt:       a.CreatedAt,
		Expired:         a.Expired,
		Home:            a.ID == homeID,
		Revision:        a.CurrentRevision(),
	}
}

// attachAllocations gives every project in nodes its allocations (Node.Allocations) and every allocation its project (Node.Project, with that project's allocations and without its history). nodes is a slice of copies.
func (s *Service) attachAllocations(ctx context.Context, nodes []model.Node) ([]model.Node, error) {
	var projectIDs []string
	for _, n := range nodes {
		switch n.Kind {
		case model.KindProject:
			projectIDs = append(projectIDs, n.ID)
		case model.KindAllocation:
			projectIDs = append(projectIDs, n.ProjectID)
		}
	}
	if len(projectIDs) == 0 {
		return nodes, nil
	}
	slices.Sort(projectIDs)
	projectIDs = slices.Compact(projectIDs)
	allocs, err := s.allocationsOf(ctx, projectIDs...)
	if err != nil {
		return nil, err
	}
	views := func(p model.Node) []model.Allocation {
		out := make([]model.Allocation, 0, len(allocs[p.ID]))
		for _, a := range allocs[p.ID] {
			out = append(out, allocationView(a, p.HomeAllocationID))
		}
		return out
	}
	projects := map[string]model.Node{}
	var missing []string
	for _, n := range nodes {
		if n.Kind == model.KindProject {
			projects[n.ID] = n
		} else if n.Kind == model.KindAllocation {
			missing = append(missing, n.ProjectID)
		}
	}
	if len(missing) > 0 {
		loaded, err := s.store.ListNodes(ctx, nodestore.NodeQuery{IDs: missing}, 0, 0)
		if err != nil {
			return nil, fmt.Errorf("load projects: %w", err)
		}
		for _, p := range loaded {
			projects[p.ID] = p
		}
	}
	for i := range nodes {
		switch nodes[i].Kind {
		case model.KindProject:
			nodes[i].Allocations = views(nodes[i])
		case model.KindAllocation:
			p, ok := projects[nodes[i].ProjectID]
			if !ok {
				continue
			}
			slim := p
			slim.History = nil
			slim.Allocations = views(p)
			nodes[i].Project = &slim
		}
	}
	return nodes, nil
}

// syncAllocations copies to every allocation of a project what it carries of the project for its budget's listings and rules: name and owner (a person's share is counted by owner). Callers hold approvalMu.
func (s *Service) syncAllocations(ctx context.Context, project model.Node) error {
	allocs, err := s.allocationsOf(ctx, project.ID)
	if err != nil {
		return err
	}
	for _, a := range allocs[project.ID] {
		if a.Name == project.Name && a.Owner == project.Owner {
			continue
		}
		if _, err := s.store.UpdateNode(ctx, a.ID, func(n *model.Node) error {
			n.Name, n.Owner = project.Name, project.Owner
			return nil
		}); err != nil {
			return fmt.Errorf("update allocation %s: %w", a.ID, err)
		}
	}
	return nil
}

// soleLive returns the one allocation of a project in one of statuses, for an operation on a project that means one allocation; an error when there is none or more than one, naming what to do instead.
func (s *Service) soleLive(ctx context.Context, project *model.Node, statuses []string) (*model.Node, error) {
	allocs, err := s.allocationsOf(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	var found []model.Node
	for _, a := range allocs[project.ID] {
		if slices.Contains(statuses, a.Status) {
			found = append(found, a)
		}
	}
	switch len(found) {
	case 1:
		return &found[0], nil
	case 0:
		return nil, refuse(common.ErrConflict, "Das Projekt %q hat keine Zuteilung, auf die sich das beziehen könnte.", "The project %q has no allocation this could refer to.", nodeLabel(*project))
	default:
		return nil, invalid("Das Projekt %q hat mehrere Zuteilungen: Das gilt einer davon, bitte die Zuteilung angeben.", "The project %q has several allocations: this applies to one of them, please name the allocation.", nodeLabel(*project))
	}
}

// checkProjectAvailabilities refuses an allocation limit that grants an availability another live allocation of the project already grants: an availability comes from one place, so that withdrawing it has one answer to "who still depends on this".
func (s *Service) checkProjectAvailabilities(ctx context.Context, projectID, exceptAllocation string, limit common.ProjectQuota) error {
	if projectID == "" {
		return nil
	}
	allocs, err := s.allocationsOf(ctx, projectID)
	if err != nil {
		return err
	}
	for rid, v := range limit {
		if !s.isBool(rid) || v != 1 {
			continue
		}
		for _, a := range allocs[projectID] {
			if a.ID == exceptAllocation || !slices.Contains(liveAllocationStatuses, a.Status) {
				continue
			}
			if a.Limit.Grants(rid) || (a.Pending != nil && a.Pending.Limit != nil && a.Pending.Limit.Grants(rid)) {
				return invalid("Das Projekt hat %q bereits aus dem Budget %q. Eine Verfügbarkeit kommt nur aus einer Zuteilung.", "The project already has %q from the budget %q. An availability comes from one allocation only.", rid, derefParent(a.ParentID))
			}
		}
	}
	return nil
}
