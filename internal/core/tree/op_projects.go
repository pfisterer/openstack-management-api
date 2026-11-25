package tree

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
)

// AllocationSpec is one allocation a new project asks for: what it wants from which budget, until when.
type AllocationSpec struct {
	BudgetID string              `json:"budget_id" binding:"required"`
	Limit    common.ProjectQuota `json:"limit" binding:"required"`
	// TerminationDate may not lie after the end of any budget above BudgetID; omitted, the latest the budget allows.
	TerminationDate *string `json:"termination_date"`
}

// createProject creates a project and its allocations.
//
// What it asks for comes in one of three ways: req.Allocations names them (the split the portal showed, possibly changed); req.ParentID and req.Limit name one budget for all of it, as a project with a single allocation always looked; or req.Limit alone, which is split automatically over the budgets the caller may request from (planSplit). Each allocation then runs through the rules of its budget: a manager of it creates it approved, an auto-approve policy grants it, or it waits. Where the split was made — the first and the last way — a budget grants on the spot what its policy allows and the rest waits as a change on top (decided 25.11.2026, (1)); a single budget named directly grants all or nothing, as before.
//
// The project starts as soon as one allocation is approved, or with StartAll once every one is decided (decided 25.11.2026, (3)).
func (s *Service) createProject(req CreateNodeRequest, actor model.Actor, userEmail string, userTokens common.TokenList) (model.Node, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return model.Node{}, invalid("Ein Name ist erforderlich.", "A name is required.")
	}
	if err := validateName(name); err != nil {
		return model.Node{}, err
	}
	if err := validateReason(req.Reason); err != nil {
		return model.Node{}, err
	}
	if setting := req.budgetSetting(); setting != "" {
		return model.Node{}, invalid("%s ist eine Einstellung von Budgets.", "%s is a setting of budgets.", setting)
	}
	switch req.StartWhen {
	case "", model.StartNow, model.StartAll:
	default:
		return model.Node{}, invalid("start_when ist %q oder %q.", "start_when is %q or %q.", model.StartNow, model.StartAll)
	}
	owner, err := normalizeOwnerToken(userEmail)
	if err != nil {
		return model.Node{}, err
	}
	adminScope := common.CanonicalTokens(req.AdminScope)
	end, err := normalizeEnd(req.TerminationDate)
	if err != nil {
		return model.Node{}, err
	}

	specs := slices.Clone(req.Allocations)
	split := len(specs) > 0
	if len(specs) == 0 && req.ParentID != "" {
		specs = []AllocationSpec{{BudgetID: req.ParentID, Limit: req.Limit, TerminationDate: end}}
	}
	for i := range specs {
		if specs[i].TerminationDate, err = normalizeEnd(specs[i].TerminationDate); err != nil {
			return model.Node{}, err
		}
		if specs[i].TerminationDate == nil {
			specs[i].TerminationDate = end
		}
		if err := s.validateLeafLimit(specs[i].Limit); err != nil {
			return model.Node{}, err
		}
	}

	// The members are checked before the lock: that asks the role provider. Their ceiling is the one of everything asked for.
	var total []common.ProjectQuota
	for _, sp := range specs {
		total = append(total, sp.Limit)
	}
	if len(specs) == 0 {
		total = append(total, req.Limit)
	}
	validateCtx, cancelValidate := s.newCtx()
	members, err := s.normalizeAuthorizedUsers(validateCtx, req.AuthorizedUsers, s.memberCeilingOf(model.Node{}, total...), userTokens, s.managesAnyOf(validateCtx, userTokens, specs))
	cancelValidate()
	if err != nil {
		return model.Node{}, err
	}

	s.approvalMu.Lock()
	defer s.approvalMu.Unlock()
	ctx, cancel := s.newCtx()
	defer cancel()

	if len(specs) == 0 {
		if err := s.validateLeafLimit(req.Limit); err != nil {
			return model.Node{}, err
		}
		plan, err := s.planSplit(ctx, req.Limit, end, nil, owner, userTokens)
		if err != nil {
			return model.Node{}, err
		}
		if len(plan.Unplaced) > 0 {
			return model.Node{}, refuse(common.ErrForbidden, "Für %v gibt es kein Budget, das es genehmigt oder eine Anfrage danach annimmt.", "No budget grants %v or takes a request for it.", plan.Unplaced)
		}
		for _, p := range plan.Parts {
			specs = append(specs, AllocationSpec{BudgetID: p.BudgetID, Limit: p.Limit, TerminationDate: p.TerminationDate})
		}
		split = true
	}
	if len(specs) == 0 {
		return model.Node{}, invalid("Das Projekt braucht mindestens eine Zuteilung.", "The project needs at least one allocation.")
	}

	// Everything that can be checked without writing, for every allocation, before anything is written.
	type placed struct {
		spec    AllocationSpec
		budget  model.Node
		chain   []model.Node
		manager bool
	}
	var todo []placed
	seen := map[string]bool{}
	granted := map[string]string{}
	caller := common.NewTokenSet(userTokens)
	for i, sp := range specs {
		if seen[sp.BudgetID] {
			return model.Node{}, invalid("Das Budget %q kommt zweimal vor; je Budget gibt es eine Zuteilung.", "The budget %q appears twice; there is one allocation per budget.", sp.BudgetID)
		}
		seen[sp.BudgetID] = true
		budget, chain, err := s.allocationTarget(ctx, sp.BudgetID)
		if err != nil {
			return model.Node{}, err
		}
		manager := managesInChain(caller, chain)
		if !manager && !isEligibleRequester(userTokens, budget) {
			return model.Node{}, refuse(common.ErrForbidden, "Aus dem Budget %q lässt sich nichts beantragen.", "Nothing can be requested from the budget %q.", nodeLabel(*budget))
		}
		// The first allocation gets the availabilities preset for the areas it asks for, as a project with one allocation always did; the others carry only what they name, so nothing is granted twice.
		if i == 0 {
			sp.Limit = s.withDefaultAvailabilities(chain, sp.Limit)
		}
		if err := s.checkLeafLimit(ctx, model.Node{}, chain, sp.Limit, nil, false); err != nil {
			return model.Node{}, err
		}
		for rid, v := range sp.Limit {
			if s.isBool(rid) && v == 1 {
				if other, dup := granted[rid]; dup {
					return model.Node{}, invalid("%q kommt aus den Budgets %q und %q; eine Verfügbarkeit kommt nur aus einer Zuteilung.", "%q comes from the budgets %q and %q; an availability comes from one allocation only.", rid, other, nodeLabel(*budget))
				}
				granted[rid] = nodeLabel(*budget)
			}
		}
		if !manager {
			if err := s.checkProjectsPerOwner(ctx, budget, userEmail); err != nil {
				return model.Node{}, err
			}
		}
		if sp.TerminationDate, err = s.fitNodeEnd(model.KindAllocation, sp.TerminationDate, chain); err != nil {
			return model.Node{}, err
		}
		todo = append(todo, placed{spec: sp, budget: *budget, chain: chain, manager: manager})
	}

	now := s.now().UTC().Format(time.RFC3339)
	project := model.Node{
		ID:              "p_" + uuid.New().String(),
		Kind:            model.KindProject,
		Status:          model.StatusPending,
		Name:            name,
		Reason:          req.Reason,
		Owner:           owner,
		AuthorizedUsers: members,
		AdminScope:      adminScope,
		StartWhen:       cmpOr(req.StartWhen, model.StartNow),
		Limit:           common.ProjectQuota{},
		TerminationDate: end,
		CreatedBy:       userEmail,
		CreatedAt:       now,
	}
	created := model.NewHistoryEntry("created", actor, model.StatusPending)
	created.Reason = new(req.Reason)
	project.History = []model.HistoryEntry{created}
	if err := s.store.UpsertNode(ctx, project); err != nil {
		return model.Node{}, fmt.Errorf("persist project: %w", err)
	}
	var written []string
	undo := func() {
		_ = s.store.DeleteNodes(ctx, append(written, project.ID))
	}
	var allocs []model.Node
	for _, t := range todo {
		a, err := s.placeAllocation(ctx, project, t.budget, t.chain, t.spec.Limit, t.spec.TerminationDate, req.Reason, t.manager, split, actor, userEmail)
		if err != nil {
			undo()
			return model.Node{}, err
		}
		written = append(written, a.ID)
		allocs = append(allocs, a)
	}
	stored, err := s.refreshProject(ctx, project.ID, actor)
	if err != nil {
		undo()
		return model.Node{}, err
	}
	for _, a := range allocs {
		if slices.Contains(model.ManagedStatuses, a.Status) {
			s.notifyWaiting(ctx, a, *stored, actor)
		}
	}
	out, err := s.attachUsage(ctx, []model.Node{*stored}, userTokens)
	if err != nil {
		return *stored, nil
	}
	return out[0], nil
}

// cmpOr is the first non-empty string.
func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// managesAnyOf reports whether the caller manages one of the budgets named (or one above), which lets them add any group as members. False where that cannot be read; the check under the lock decides the rest.
func (s *Service) managesAnyOf(ctx context.Context, userTokens common.TokenList, specs []AllocationSpec) bool {
	caller := common.NewTokenSet(userTokens)
	for _, sp := range specs {
		chain, err := s.nodeChain(ctx, sp.BudgetID)
		if err == nil && managesInChain(caller, chain) {
			return true
		}
	}
	return false
}

// allocationTarget loads a budget an allocation is to be placed under, with its chain (the budget first).
func (s *Service) allocationTarget(ctx context.Context, budgetID string) (*model.Node, []model.Node, error) {
	budget, err := s.store.GetNode(ctx, budgetID)
	if err != nil {
		return nil, nil, fmt.Errorf("load budget: %w", err)
	}
	if budget == nil {
		return nil, nil, refuse(common.ErrNotFound, "Das Budget %q wurde nicht gefunden.", "The budget %q was not found.", budgetID)
	}
	if budget.Kind != model.KindBudget {
		return nil, nil, invalid("Eine Zuteilung hängt unter einem Budget.", "An allocation hangs under a budget.")
	}
	if budget.Status != model.StatusApproved {
		return nil, nil, refuse(common.ErrConflict, "Unter einem Budget im Status %q lässt sich nichts anlegen.", "Nothing can be created under a budget in status %q.", budget.Status)
	}
	if budget.ID == model.UnassignedNodeID {
		return nil, nil, invalid("Unter „Nicht zugeordnet“ lässt sich nichts anlegen.", "Nothing can be created under “Unassigned”.")
	}
	chain, err := s.nodeChain(ctx, budget.ID)
	if err != nil {
		return nil, nil, err
	}
	return budget, chain, nil
}

// placeAllocation writes a new allocation of project under budget and decides it by the budget's rules: approved when a manager of it asks, granted by its auto-approve policy, else waiting — or refused where the budget takes no requests beyond its policy. With partial, a policy that grants only part of it approves that part, and the rest waits as a change on top. Callers hold approvalMu, have checked the limit (checkLeafLimit) and fitted the end, and refresh the project afterwards.
func (s *Service) placeAllocation(ctx context.Context, project model.Node, budget model.Node, chain []model.Node, limit common.ProjectQuota, end *string, reason string, manager, partial bool, actor model.Actor, userEmail string) (model.Node, error) {
	now := s.now().UTC().Format(time.RFC3339)
	node := model.Node{
		ID:              "a_" + uuid.New().String(),
		Kind:            model.KindAllocation,
		ParentID:        &budget.ID,
		ProjectID:       project.ID,
		Status:          model.StatusPending,
		Name:            project.Name,
		Owner:           project.Owner,
		Reason:          strings.TrimSpace(reason),
		Limit:           maps.Clone(limit),
		TerminationDate: end,
		CreatedBy:       userEmail,
		CreatedAt:       now,
	}
	if node.Limit == nil {
		node.Limit = common.ProjectQuota{}
	}
	created := model.NewHistoryEntry("created", actor, model.StatusPending)
	created.LimitTo = new(maps.Clone(node.Limit))
	if node.Reason != "" {
		created.Reason = new(node.Reason)
	}
	node.History = []model.HistoryEntry{created}

	switch {
	case manager:
		if err := s.checkCapacity(ctx, chain, node.Limit, nil); err != nil {
			return model.Node{}, err
		}
		node.Status = model.StatusApproved
		entry := model.NewHistoryEntry("approved", actor, model.StatusApproved)
		entry.StatusFrom = new(model.StatusPending)
		entry.Reason = new("Direkt angelegt von einer Person, die das Budget verwaltet")
		node.History = append(node.History, entry)
	case budget.AutoApprove != nil && !allZero(node.Limit):
		// A request for nothing at all waits for a manager: granted on the spot, it would still be a project with its groups, created without anybody looking.
		ok, err := s.autoApprovable(ctx, &budget, node.Owner, node.Limit, nil)
		if err != nil {
			return model.Node{}, err
		}
		if ok {
			node.Status = model.StatusApproved
			node.History = append(node.History, autoApprovedEntry(actor, model.StatusPending, autoApproveReason(&budget)))
			break
		}
		if !budget.RequestsBeyondAutoApproveAllowed() {
			return model.Node{}, errBeyondAutoApprove
		}
		if !partial {
			break
		}
		g, err := s.autoGrantable(ctx, &budget, chain, node.Owner, node.Limit)
		if err != nil {
			return model.Node{}, err
		}
		if allZero(g) {
			break
		}
		asked := node.Limit
		node.Limit = g
		node.Status = model.StatusApproved
		node.History = append(node.History, autoApprovedEntry(actor, model.StatusPending, autoApproveReason(&budget)+": der Teil, den das Budget ohne Antrag gibt"))
		node.Pending = &model.PendingChanges{Limit: &asked}
		node.Status = model.StatusChangePending
		rest := model.NewHistoryEntry("change_requested", actor, model.StatusChangePending)
		rest.StatusFrom = new(model.StatusApproved)
		rest.LimitFrom = new(maps.Clone(g))
		rest.LimitTo = new(maps.Clone(asked))
		rest.Reason = new("Der Rest wartet auf die Verwaltenden des Budgets")
		node.History = append(node.History, rest)
	}
	if err := s.store.UpsertNode(ctx, node); err != nil {
		return model.Node{}, fmt.Errorf("persist allocation: %w", err)
	}
	return node, nil
}

// createAllocation adds an allocation to an existing project: its owner or admins ask a budget for it (a request under that budget's rules — to extend a project from another budget, or to move what it has elsewhere), or a manager of the budget grants it of their own accord, with a reason, approved at once (the former extra allocation from above, decided 25.11.2026). A project has one live allocation per budget; more from the same budget is a change of that one.
func (s *Service) createAllocation(req CreateNodeRequest, actor model.Actor, userEmail string, userTokens common.TokenList) (model.Node, error) {
	if req.ProjectID == "" || req.ParentID == "" {
		return model.Node{}, invalid("project_id und parent_id (das Budget) sind erforderlich.", "project_id and parent_id (the budget) are required.")
	}
	if setting := req.budgetSetting(); setting != "" {
		return model.Node{}, invalid("%s ist eine Einstellung von Budgets.", "%s is a setting of budgets.", setting)
	}
	if len(req.AuthorizedUsers) > 0 {
		return model.Node{}, invalid("Mitglieder gibt es nur an Projekten.", "Only projects have members.")
	}
	if err := s.validateLeafLimit(req.Limit); err != nil {
		return model.Node{}, err
	}
	if err := validateReason(req.Reason); err != nil {
		return model.Node{}, err
	}
	end, err := normalizeEnd(req.TerminationDate)
	if err != nil {
		return model.Node{}, err
	}

	s.approvalMu.Lock()
	defer s.approvalMu.Unlock()
	ctx, cancel := s.newCtx()
	defer cancel()

	project, err := s.loadProject(ctx, req.ProjectID)
	if err != nil {
		return model.Node{}, err
	}
	if project.Status != model.StatusPending && project.Status != model.StatusApproved {
		return model.Node{}, refuse(common.ErrConflict, "Ein Projekt im Status %q bekommt keine Zuteilung mehr.", "A project in status %q gets no more allocations.", project.Status)
	}
	budget, chain, err := s.allocationTarget(ctx, req.ParentID)
	if err != nil {
		return model.Node{}, err
	}
	manager := managesInChain(common.NewTokenSet(userTokens), chain)
	holder := holdsProject(userTokens, project)
	switch {
	case manager:
		if strings.TrimSpace(req.Reason) == "" {
			return model.Node{}, invalid("Eine Begründung ist erforderlich: Wer das Projekt verantwortet, sieht diese Zuteilung und soll wissen, warum es sie gibt.", "A reason is required: whoever is responsible for the project sees this allocation and should know why it exists.")
		}
	case holder:
		if !isEligibleRequester(userTokens, budget) {
			return model.Node{}, refuse(common.ErrForbidden, "Aus dem Budget %q lässt sich nichts beantragen.", "Nothing can be requested from the budget %q.", nodeLabel(*budget))
		}
		if err := s.checkProjectsPerOwner(ctx, budget, project.OwnerEmail()); err != nil {
			return model.Node{}, err
		}
	default:
		return model.Node{}, errNoPermission
	}
	allocs, err := s.allocationsOf(ctx, project.ID)
	if err != nil {
		return model.Node{}, err
	}
	for _, a := range allocs[project.ID] {
		if derefParent(a.ParentID) == budget.ID && slices.Contains(liveAllocationStatuses, a.Status) {
			return model.Node{}, refuse(common.ErrConflict, "Das Projekt hat bereits eine Zuteilung aus dem Budget %q; mehr daraus ist eine Änderung dieser Zuteilung.", "The project already has an allocation from the budget %q; more from it is a change of that allocation.", nodeLabel(*budget))
		}
	}
	probe := model.Node{ProjectID: project.ID}
	if err := s.checkLeafLimit(ctx, probe, chain, req.Limit, nil, false); err != nil {
		return model.Node{}, err
	}
	if err := checkMemberCeilingFalls(len(project.AuthorizedUsers), s.memberCeilingOf(*project), s.memberCeilingOf(*project, req.Limit)); err != nil {
		return model.Node{}, err
	}
	if end, err = s.fitNodeEnd(model.KindAllocation, end, chain); err != nil {
		return model.Node{}, err
	}
	a, err := s.placeAllocation(ctx, *project, *budget, chain, req.Limit, end, req.Reason, manager, false, actor, userEmail)
	if err != nil {
		return model.Node{}, err
	}
	entry := model.NewHistoryEntry("allocation_added", actor, project.Status)
	entry.AllocationFrom = &budget.ID
	entry.LimitTo = new(maps.Clone(a.Limit))
	if r := strings.TrimSpace(req.Reason); r != "" {
		entry.Reason = &r
	}
	if _, err := s.store.UpdateNode(ctx, project.ID, func(n *model.Node) error {
		n.History = append(slices.Clone(n.History), entry)
		return nil
	}); err != nil {
		return model.Node{}, err
	}
	stored, err := s.refreshProject(ctx, project.ID, actor)
	if err != nil {
		return model.Node{}, err
	}
	if slices.Contains(model.ManagedStatuses, a.Status) {
		s.notifyWaiting(ctx, a, *stored, actor)
	}
	out, err := s.attachUsage(ctx, []model.Node{a}, userTokens)
	if err != nil {
		return a, nil
	}
	return out[0], nil
}

// updateProject is UpdateNode on a project: its name and its admins, by its owner, its admins and whoever manages every budget paying for it. A new name is carried to its allocations. Callers hold approvalMu.
func (s *Service) updateProject(ctx context.Context, current *model.Node, req UpdateNodeRequest, actor model.Actor, userTokens common.TokenList) (model.Node, error) {
	if err := s.authorizeProjectLevel(ctx, userTokens, current); err != nil {
		return model.Node{}, err
	}
	updated := *current
	entry := model.NewHistoryEntry("updated", actor, current.Status)
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			return model.Node{}, invalid("Ein Name ist erforderlich.", "A name is required.")
		}
		if err := validateName(name); err != nil {
			return model.Node{}, err
		}
		updated.Name = name
	}
	if req.AdminScope != nil {
		updated.AdminScope = *req.AdminScope
	}
	updated.History = append(slices.Clone(current.History), entry)
	if err := s.store.UpsertNode(ctx, updated); err != nil {
		return model.Node{}, fmt.Errorf("persist node: %w", err)
	}
	if updated.Name != current.Name {
		if err := s.syncAllocations(ctx, updated); err != nil {
			return model.Node{}, err
		}
	}
	out, err := s.attachUsage(ctx, []model.Node{updated}, userTokens)
	if err != nil {
		return updated, nil
	}
	return out[0], nil
}

// liveLimits are what the live allocations of a project hold and ask for: the ceiling on its members follows every provider one of them concerns.
func liveLimits(allocs []model.Node) []common.ProjectQuota {
	var out []common.ProjectQuota
	for _, a := range allocs {
		if !slices.Contains(liveAllocationStatuses, a.Status) {
			continue
		}
		out = append(out, a.Limit)
		if a.Pending != nil && a.Pending.Limit != nil {
			out = append(out, *a.Pending.Limit)
		}
	}
	return out
}

// requestProjectChange is RequestChange on a project. Members change at once — who works in a project is its owner's business — by its owner, its admins and the managers of a budget paying for it, within the ceiling of the providers it concerns. A change of resources or of the end belongs to an allocation; on a project with a single live allocation it is that allocation's change, on one with several the allocation has to be named. Callers hold approvalMu.
func (s *Service) requestProjectChange(ctx context.Context, current *model.Node, req ChangeNodeRequest, actor model.Actor, userTokens common.TokenList) (model.Node, error) {
	allocs, err := s.allocationsOf(ctx, current.ID)
	if err != nil {
		return model.Node{}, err
	}
	funder, err := s.managesSomeAllocation(ctx, userTokens, allocs[current.ID])
	if err != nil {
		return model.Node{}, err
	}
	if !holdsProject(userTokens, current) && !funder {
		return model.Node{}, errNoPermission
	}
	// Without members it is all the allocation's: answered with the allocation, as a change of a project's only leaf always was.
	if req.AuthorizedUsers == nil {
		alloc, err := s.soleLive(ctx, current, liveAllocationStatuses)
		if err != nil {
			return model.Node{}, err
		}
		return s.requestChangeLocked(ctx, alloc, req, actor, userTokens)
	}
	// Members first, held to the ceiling of the project as it will be, then the rest on the allocation.
	var alloc *model.Node
	limits := liveLimits(allocs[current.ID])
	if req.Limit != nil || req.TerminationDate != nil {
		if alloc, err = s.soleLive(ctx, current, liveAllocationStatuses); err != nil {
			return model.Node{}, err
		}
		if req.Limit != nil {
			limits = append(limits, *req.Limit)
		}
	}
	normalized, err := s.normalizeAuthorizedUsers(ctx, *req.AuthorizedUsers, s.memberCeilingOf(*current, limits...), userTokens, funder, current.AuthorizedUsers)
	if err != nil {
		return model.Node{}, err
	}
	if _, err := s.store.UpdateNode(ctx, current.ID, func(n *model.Node) error {
		entry := model.NewHistoryEntry("members_changed", actor, n.Status)
		entry.Reason = req.Reason
		n.AuthorizedUsers = normalized
		n.History = append(slices.Clone(n.History), entry)
		return nil
	}); err != nil {
		return model.Node{}, err
	}
	if alloc != nil {
		sub := req
		sub.AuthorizedUsers = nil
		if _, err := s.requestChangeLocked(ctx, alloc, sub, actor, userTokens); err != nil {
			return model.Node{}, err
		}
	}
	stored, err := s.loadNode(ctx, current.ID)
	if err != nil {
		return model.Node{}, err
	}
	out, err := s.attachUsage(ctx, []model.Node{*stored}, userTokens)
	if err != nil {
		return *stored, nil
	}
	return out[0], nil
}
