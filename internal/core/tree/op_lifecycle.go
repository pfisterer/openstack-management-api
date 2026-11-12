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
	"github.com/pfisterer/openstack-management-api/internal/core/nodestore"
)

// ── Reads / views ─────────────────────────────────────────────────────────────

// CreateNode creates a child node under req.ParentID. Managers of the parent (or
// its ancestors) create directly approved children; eligible requesters submit a
// pending request, which may be auto-approved by the parent's AutoApprove policy.
func (s *Service) CreateNode(req CreateNodeRequest, actor model.Actor, userEmail string, userTokens common.TokenList) (model.Node, error) {
	if strings.TrimSpace(userEmail) == "" || len(userTokens) == 0 {
		return model.Node{}, errNoPermission
	}
	req.AdminScope = common.CanonicalTokens(req.AdminScope)
	req.EligibleRequesters = common.CanonicalTokens(req.EligibleRequesters)
	var err error
	if req.TerminationDate, err = normalizeEnd(req.TerminationDate); err != nil {
		return model.Node{}, err
	}

	// Validate the request shape before taking the approval lock.
	//
	// Every node needs a name, leaves included: it is what the requester sees in
	// the UI and what the reconciler names the OpenStack project after. Without
	// it the purpose had to stand in — a whole sentence truncated to Keystone's
	// name length.
	if strings.TrimSpace(req.Name) == "" {
		return model.Node{}, invalid("Ein Name ist erforderlich.")
	}
	if err := validateName(strings.TrimSpace(req.Name)); err != nil {
		return model.Node{}, err
	}
	if err := validateReason(req.Reason); err != nil {
		return model.Node{}, err
	}
	switch req.Kind {
	case model.KindProject:
		if err := s.validateLeafLimit(req.Limit); err != nil {
			return model.Node{}, err
		}
		if name := req.budgetSetting(); name != "" {
			return model.Node{}, invalid("%s ist eine Einstellung von Budgets.", name)
		}
	case model.KindBudget:
		// A budget without an admin scope is invisible in the UI: "My Budgets"
		// matches AdminScope directly (the ancestor rule does not apply there),
		// so nobody would find it, and every request under it would surface at
		// the nearest ancestor manager instead. The structural "unassigned" node
		// is the deliberate exception and is created by Bootstrap, not here.
		if len(req.AdminScope) == 0 {
			return model.Node{}, invalid("Ein Budget braucht mindestens einen Eintrag unter „Verwaltet von“ (admin_scope).")
		}
		if err := s.validateBudgetLimit(req.Limit); err != nil {
			return model.Node{}, err
		}
		if err := s.validateAutoApprove(req.AutoApprove); err != nil {
			return model.Node{}, err
		}
		if err := validateSubBudgetRequests(req.AutoApprove, req.AllowSubBudgetRequests); err != nil {
			return model.Node{}, err
		}
	default:
		return model.Node{}, invalid("Unbekannte Art %q.", req.Kind)
	}

	// Validated before the approval lock: checking the group tokens talks to the
	// role provider, and that call has no business holding up every other
	// approval in the process.
	// Whether the caller manages the parent decides which groups they may add; it is decided again under the lock below, this read only serves the check of the members.
	validateCtx, cancelValidate := s.newCtx()
	normalizedAuthorizedUsers, err := s.normalizeCreateMembers(validateCtx, req, userTokens)
	cancelValidate()
	if err != nil {
		return model.Node{}, err
	}

	// Serialize the capacity-affecting approval paths below.
	s.approvalMu.Lock()
	defer s.approvalMu.Unlock()

	ctx, cancel := s.newCtx()
	defer cancel()

	parent, err := s.store.GetNode(ctx, req.ParentID)
	if err != nil {
		return model.Node{}, fmt.Errorf("load parent node: %w", err)
	}
	if parent == nil {
		return model.Node{}, refuse(common.ErrNotFound, "Das übergeordnete Budget wurde nicht gefunden.")
	}
	if parent.Kind != model.KindBudget {
		return model.Node{}, invalid("Darüber muss ein Budget stehen.")
	}
	if parent.Status != model.StatusApproved {
		return model.Node{}, refuse(common.ErrConflict, "Unter einem Budget im Status %q lässt sich nichts anlegen.", parent.Status)
	}

	// The parent and the budgets above it: who manages the parent, and what bounds the new node.
	parentChain, err := s.nodeChain(ctx, parent.ID)
	if err != nil {
		return model.Node{}, err
	}
	isManager := managesInChain(common.NewTokenSet(userTokens), parentChain)
	if !isManager && !isEligibleRequester(userTokens, parent) {
		return model.Node{}, errNoPermission
	}
	if req.Kind == model.KindProject {
		req.Limit = s.withDefaultAvailabilities(parentChain, req.Limit)
		if err := s.checkLeafLimit(ctx, model.Node{}, parentChain, req.Limit, nil, false); err != nil {
			return model.Node{}, err
		}
		if !isManager {
			if err := s.checkProjectsPerOwner(ctx, parent, userEmail); err != nil {
				return model.Node{}, err
			}
		}
	}
	// Requesters may be limited to leaves: a course budget usually wants
	// projects from its students, not a sub-budget tree underneath. Managers are
	// exempt — they own the structure and create sub-budgets directly.
	if req.Kind == model.KindBudget && !isManager && !parent.SubBudgetRequestsAllowed() {
		return model.Node{}, refuse(common.ErrForbidden, "Dieses Budget nimmt keine Teilbudget-Anfragen an. Stattdessen lässt sich ein Projekt anfragen.")
	}
	// Passing on the parent's limit hands out all of it, so only those who
	// could give that limit directly may.
	if req.InheritsLimit {
		if !isManager {
			return model.Node{}, refuse(common.ErrForbidden, "Ein Budget, das das Limit des Budgets darüber übernimmt, legen nur Verwaltende an.")
		}
		req.Limit = common.ProjectQuota{}
	}

	// Nothing outlives the budget it draws from, and a project runs no longer
	// than the budgets above allow: no end asked means the latest allowed, a
	// later one is refused. A sub-budget inherits the term cap the same way.
	var maxTerm *int
	terminationDate, err := s.fitNodeEnd(req.Kind, req.TerminationDate, parentChain)
	if req.Kind == model.KindBudget {
		if err == nil {
			maxTerm, err = fitMaxTerm(req.MaxProjectTermDays, model.ResolveMaxProjectTermDays(parentChain))
		}
		if err == nil {
			// Unlike the term, nothing is copied down: what the budgets above set applies anyway (strictest along the chain), and copying would freeze it here.
			err = model.ValidateCaps(req.Caps, model.ResolveCaps(parentChain, s.capDefs()), false, req.InheritsLimit, s.capDefs())
		}
	}
	if err != nil {
		return model.Node{}, err
	}

	now := s.now().UTC().Format(time.RFC3339)
	node := model.Node{
		Kind:            req.Kind,
		ParentID:        &parent.ID,
		Status:          model.StatusPending,
		Name:            strings.TrimSpace(req.Name),
		Reason:          req.Reason,
		Limit:           req.Limit,
		TerminationDate: terminationDate,
		CreatedBy:       userEmail,
		CreatedAt:       now,
	}

	createdEntry := model.NewHistoryEntry("created", actor, model.StatusPending)
	createdEntry.LimitTo = &req.Limit
	createdEntry.Reason = new(req.Reason)

	if req.Kind == model.KindProject {
		node.ID = "p_" + uuid.New().String()
		owner, err := normalizeOwnerToken(userEmail)
		if err != nil {
			return model.Node{}, err
		}
		node.Owner = owner
		node.AuthorizedUsers = normalizedAuthorizedUsers
		// Optional on a project: the people who administer it together with the
		// owner. It grants self-service rights only, no OpenStack role.
		node.AdminScope = req.AdminScope
	} else {
		node.ID = "b_" + uuid.New().String()
		node.AdminScope = req.AdminScope
		node.EligibleRequesters = req.EligibleRequesters
		node.AutoApprove = req.AutoApprove
		node.AllowSubBudgetRequests = req.AllowSubBudgetRequests
		node.AllowRequestsBeyondAutoApprove = req.AllowRequestsBeyondAutoApprove
		node.AutoApproveExtensions = req.AutoApproveExtensions
		node.MaxProjectTermDays = maxTerm
		node.Caps = req.Caps.Clone()
		node.InheritsLimit = req.InheritsLimit
	}
	node.History = []model.HistoryEntry{createdEntry}

	// Decide the initial status.
	switch {
	case isManager:
		// Direct creation by a manager of the parent chain — approved immediately,
		// subject to the same checks an explicit approval would run.
		if req.Kind == model.KindProject {
			if err := s.checkCapacity(ctx, parentChain, node.Limit, nil); err != nil {
				return model.Node{}, err
			}
		} else if !node.InheritsLimit {
			if err := s.checkBudgetLimit(ctx, node, parentChain, node.Limit, false); err != nil {
				return model.Node{}, err
			}
		}
		node.Status = model.StatusApproved
		approvedEntry := model.NewHistoryEntry("approved", actor, model.StatusApproved)
		approvedEntry.StatusFrom = new(model.StatusPending)
		approvedEntry.Reason = new("Direkt angelegt von einer Person, die das Budget verwaltet")
		node.History = append(node.History, approvedEntry)

	case req.Kind == model.KindProject && parent.AutoApprove != nil:
		// A request for nothing at all is no allocation the policy could be about; granted on the spot, it would still be an OpenStack project with its groups, created on demand without anybody looking. It waits for a manager.
		if allZero(node.Limit) {
			break
		}
		ok, err := s.autoApprovable(ctx, parent, node.Owner, node.Limit, nil)
		if err != nil {
			return model.Node{}, err
		}
		if ok {
			node.Status = model.StatusApproved
			node.History = append(node.History, autoApprovedEntry(actor, model.StatusPending, autoApproveReason(parent)))
		} else if !parent.RequestsBeyondAutoApproveAllowed() {
			return model.Node{}, errBeyondAutoApprove
		}

	case req.Kind == model.KindBudget && parent.AutoApprove.IsPool():
		// A pool grants a sub-budget whenever the edge rule allows it: a budget limit is a cap, not a reservation, and the projects below it are held to every ancestor when they are granted. A budget with a per-requester limit takes no sub-budget requests at all (validateSubBudgetRequests). As with projects, a request for nothing at all waits for a manager.
		if allZero(node.Limit) {
			break
		}
		// What a manager's approval would refuse is not granted either: it waits, and a manager may trim it — or, where the budget takes no requests beyond its policy, it is refused with the reason.
		if err := s.checkBudgetLimit(ctx, node, parentChain, node.Limit, false); err != nil {
			if !parent.RequestsBeyondAutoApproveAllowed() {
				return model.Node{}, err
			}
			break
		}
		node.Status = model.StatusApproved
		node.History = append(node.History, autoApprovedEntry(actor, model.StatusPending, autoApproveReason(parent)))
	}

	if err := s.store.UpsertNode(ctx, node); err != nil {
		return model.Node{}, fmt.Errorf("persist node: %w", err)
	}
	return node, nil
}

// normalizeCreateMembers checks the members of a new project for CreateNode, with the parent's managers free to add any group. Their ceiling depends on whether the requested limit holds anything of OpenStack.
func (s *Service) normalizeCreateMembers(ctx context.Context, req CreateNodeRequest, userTokens common.TokenList) ([]common.AuthorizedUser, error) {
	ceiling := s.memberCeilingOf(model.Node{}, req.Limit)
	if len(req.AuthorizedUsers) == 0 {
		return s.normalizeAuthorizedUsers(ctx, req.AuthorizedUsers, ceiling, userTokens, false)
	}
	// The parent and the budgets above it; empty when there is no such parent, which CreateNode refuses next.
	parentChain, err := s.nodeChain(ctx, req.ParentID)
	if err != nil {
		return nil, err
	}
	manager := managesInChain(common.NewTokenSet(userTokens), parentChain)
	return s.normalizeAuthorizedUsers(ctx, req.AuthorizedUsers, ceiling, userTokens, manager)
}

// allZero reports whether a limit asks for nothing: every value 0, availabilities included.
func allZero(q common.ProjectQuota) bool {
	for _, v := range q {
		if v != 0 {
			return false
		}
	}
	return true
}

// checkProjectsPerOwner refuses a request that would give owner more live projects under budget than the configured maximum (see SetMaxProjectsPerOwner). Live are the ones that hold or wait for an OpenStack project: pending, active and released — released ones keep theirs until it is deleted. Rejected and archived ones do not count.
func (s *Service) checkProjectsPerOwner(ctx context.Context, budget *model.Node, ownerEmail string) error {
	if s.maxProjectsPerOwner <= 0 {
		return nil
	}
	owner, err := normalizeOwnerToken(ownerEmail)
	if err != nil {
		return err
	}
	n, err := s.store.CountNodes(ctx, nodestore.NodeQuery{
		ParentIDs: []string{budget.ID},
		Kinds:     []string{model.KindProject},
		Statuses:  liveProjectStatuses,
		Owner:     owner,
	})
	if err != nil {
		return fmt.Errorf("count projects of the requester: %w", err)
	}
	if n >= s.maxProjectsPerOwner {
		return refuse(common.ErrForbidden, "Im Budget %[2]q bestehen bereits %[1]d eigene Projekte, erlaubt sind höchstens %[3]d. Ein nicht mehr benötigtes löschen oder die Verwaltenden des Budgets fragen.", n, nodeLabel(*budget), s.maxProjectsPerOwner)
	}
	return nil
}

// liveProjectStatuses are the statuses a project counts in against checkProjectsPerOwner.
var liveProjectStatuses = []string{model.StatusPending, model.StatusApproved, model.StatusChangePending, model.StatusReleased}

// UpdateNode applies immediate edits to a budget. Policy fields (name, admin
// scope, eligible requesters, auto-approve) require a manager of the node or its
// ancestors. Limit and termination date require a manager of the PARENT chain —
// you cannot raise your own budget; request a change instead.
//
// Project leaves accept exactly one direct edit: a rename, by their owner or a
// manager of the chain. A name is a label, not an allocation — sending it
// through the approval cycle would put a manager in front of a typo fix and,
// worse, park the project in change_pending until they got around to it.
// Everything else about a leaf still goes through RequestChange.
func (s *Service) UpdateNode(id string, req UpdateNodeRequest, actor model.Actor, userTokens common.TokenList) (model.Node, error) {
	var err error
	if req.TerminationDate, err = normalizeEnd(req.TerminationDate); err != nil {
		return model.Node{}, err
	}
	if req.AdminScope != nil {
		req.AdminScope = new(common.CanonicalTokens(*req.AdminScope))
	}
	if req.EligibleRequesters != nil {
		req.EligibleRequesters = new(common.CanonicalTokens(*req.EligibleRequesters))
	}
	// A new end date is carried down the subtree, which must not interleave
	// with an approval deciding on one of its nodes.
	s.approvalMu.Lock()
	defer s.approvalMu.Unlock()

	ctx, cancel := s.newCtx()
	defer cancel()

	current, err := s.loadLive(ctx, id)
	if err != nil {
		return model.Node{}, err
	}
	if current.Kind != model.KindBudget && !req.leafDirectEdit() {
		return model.Node{}, invalid("An einem Projekt lassen sich nur Name und Admins direkt ändern; alles andere über eine Änderungsanfrage.")
	}

	// Which fields need whom is listed once, in updateFieldClass.
	wantsPolicyEdit := req.policyEdit()
	wantsCapacityEdit := req.capacityEdit()
	// An edit that sends nothing needs nobody's rights and would only add an empty history entry: refused, so no edit passes without a check (found 10.11.2026).
	if !wantsPolicyEdit && !wantsCapacityEdit {
		return model.Node{}, invalid("Nichts zu ändern: Die Anfrage setzt kein Feld.")
	}

	// The budgets above: they decide who may make the edit and bound what it sets.
	var chain []model.Node
	if wantsPolicyEdit || wantsCapacityEdit {
		if chain, err = s.parentChainNodes(ctx, current); err != nil {
			return model.Node{}, err
		}
	}
	caller := common.NewTokenSet(userTokens)
	if wantsPolicyEdit {
		// Renaming their own project and choosing who administers it with them
		// is the owner's business; a project's admins pass managesNode through
		// its own admin scope. Every edit on a budget belongs to a manager.
		if !(current.IsLeaf() && isOwner(userTokens, current)) && !managesNodeIn(caller, current, chain) {
			return model.Node{}, errNoPermission
		}
	}
	if wantsCapacityEdit && !managesInChain(caller, chain) {
		return model.Node{}, errNoPermission
	}

	// The root node's admin scope is owned by the configuration (synced on every
	// startup); an API edit would silently revert on the next deployment.
	if current.ID == model.RootNodeID && req.AdminScope != nil {
		return model.Node{}, refuse(common.ErrForbidden, "Wer das oberste Budget verwaltet, steht in ROOT_ADMIN_TOKENS.")
	}

	updated := *current
	if req.Name != nil {
		if strings.TrimSpace(*req.Name) == "" {
			return model.Node{}, invalid("Ein Name ist erforderlich.")
		}
		if err := validateName(strings.TrimSpace(*req.Name)); err != nil {
			return model.Node{}, err
		}
		updated.Name = strings.TrimSpace(*req.Name)
	}
	// The purpose obeys the rules of creation: required, and no longer than any other reason.
	if req.Reason != nil {
		purpose := strings.TrimSpace(*req.Reason)
		if purpose == "" {
			return model.Node{}, invalid("Ein Zweck ist erforderlich.")
		}
		if err := validateReason(purpose); err != nil {
			return model.Node{}, err
		}
		updated.Reason = purpose
	}
	if req.AdminScope != nil {
		updated.AdminScope = *req.AdminScope
	}
	if req.EligibleRequesters != nil {
		updated.EligibleRequesters = *req.EligibleRequesters
	}
	if req.AllowSubBudgetRequests != nil {
		updated.AllowSubBudgetRequests = req.AllowSubBudgetRequests
	}
	if req.AllowRequestsBeyondAutoApprove != nil {
		updated.AllowRequestsBeyondAutoApprove = req.AllowRequestsBeyondAutoApprove
	}
	if req.AutoApproveExtensions != nil {
		updated.AutoApproveExtensions = req.AutoApproveExtensions
	}
	if req.ClearAutoApprove {
		updated.AutoApprove = nil
	} else if req.AutoApprove != nil {
		if err := s.validateAutoApprove(req.AutoApprove); err != nil {
			return model.Node{}, err
		}
		updated.AutoApprove = req.AutoApprove
	}
	if req.AutoApprove != nil || req.AllowSubBudgetRequests != nil {
		if err := validateSubBudgetRequests(updated.AutoApprove, updated.AllowSubBudgetRequests); err != nil {
			return model.Node{}, err
		}
	}
	if req.ClearMaxProjectTermDays {
		if bound := model.ResolveMaxProjectTermDays(chain); bound != nil {
			return model.Node{}, invalid("Im Budget darüber laufen Projekte höchstens %d Tage, deshalb braucht dieses auch diese Grenze.", *bound)
		}
		updated.MaxProjectTermDays = nil
	} else if req.MaxProjectTermDays != nil {
		if _, err := fitMaxTerm(req.MaxProjectTermDays, model.ResolveMaxProjectTermDays(chain)); err != nil {
			return model.Node{}, err
		}
		updated.MaxProjectTermDays = req.MaxProjectTermDays
	}
	// Caps only tighten what the budgets above allow. Lowering one needs nothing carried down: what applies below is the strictest along the chain, so a budget below with a looser value of its own simply gets this one. Checked after InheritsLimit below, which decides whether the budget may hold Grant caps.
	if req.ClearCaps {
		updated.Caps = nil
	} else if req.Caps != nil {
		next := maps.Clone(current.Caps)
		if next == nil {
			next = common.Caps{}
		}
		for id, v := range req.Caps {
			if v == nil {
				delete(next, id)
			} else {
				next[id] = *v
			}
		}
		updated.Caps = next.Clone()
	}
	if req.ClearTerminationDate || req.TerminationDate != nil {
		if bound := chainEnd(chain); req.ClearTerminationDate && bound != nil {
			return model.Node{}, invalid("Das Budget darüber endet am %s, deshalb braucht dieses auch ein Enddatum.", dateOnly(*bound))
		}
		if req.TerminationDate != nil {
			if _, err := s.fitNodeEnd(current.Kind, req.TerminationDate, chain); err != nil {
				return model.Node{}, err
			}
		}
	}
	if req.ClearTerminationDate {
		updated.TerminationDate = nil
	} else if req.TerminationDate != nil {
		updated.TerminationDate = req.TerminationDate
	}

	historyEntry := model.NewHistoryEntry("updated", actor, updated.Status)
	if req.ClearTerminationDate || req.TerminationDate != nil {
		historyEntry.TerminationDateFrom = current.TerminationDate
		historyEntry.TerminationDateTo = updated.TerminationDate
	}
	if updated.Reason != current.Reason {
		historyEntry.ReasonFrom = new(current.Reason)
		historyEntry.ReasonTo = new(updated.Reason)
	}

	// An inherited limit is the one of the budget above and cannot be set by
	// hand. Switched on, the budget's own limit is dropped: the one above
	// already bounds everything below it. Switched off, it keeps the limit it
	// had inherited unless given another, which goes through the usual checks.
	binding := bindingInChain(chain)
	inherits := current.InheritsLimit
	if req.InheritsLimit != nil {
		inherits = *req.InheritsLimit
	}
	if inherits && req.Limit != nil {
		return model.Node{}, invalid("Dieses Budget übernimmt das Limit des Budgets darüber. Für ein eigenes Limit das Übernehmen ausschalten.")
	}
	// Only for an edit of the limit or of inheriting it: an edit of access or
	// policy alone has nothing to inherit, and must not fail on it.
	if inherits && wantsCapacityEdit && binding == nil {
		return model.Node{}, invalid("Das oberste Budget hat nichts darüber, dessen Limit es übernehmen könnte.")
	}
	if inherits && !current.InheritsLimit {
		historyEntry.LimitFrom = &current.Limit
		historyEntry.Reason = new("Übernimmt das Limit des Budgets darüber")
		updated.Limit = common.ProjectQuota{}
		// Its Grant caps are those above now, like its limit. A copy: updated shares the map with current.
		next := maps.Clone(updated.Caps)
		for id := range model.GrantCaps(next, s.capDefs()) {
			delete(next, id)
		}
		updated.Caps = next.Clone()
	}
	if !inherits && current.InheritsLimit && req.Limit == nil && binding != nil {
		req.Limit = new(maps.Clone(binding.Limit))
	}
	// Switched off, it keeps the Grant caps it passed on, each unless the request sets or removes it: nothing changes below until somebody changes them.
	if !inherits && current.InheritsLimit && !req.ClearCaps && chain != nil {
		next := maps.Clone(updated.Caps)
		if next == nil {
			next = common.Caps{}
		}
		for id, v := range model.GrantCaps(model.ResolveCaps(chain, s.capDefs()), s.capDefs()) {
			if _, named := req.Caps[id]; !named {
				next[id] = v
			}
		}
		updated.Caps = next.Clone()
	}
	updated.InheritsLimit = inherits
	if req.Caps != nil {
		if err := model.ValidateCaps(updated.Caps, model.ResolveCaps(chain, s.capDefs()), current.ParentID == nil, inherits, s.capDefs()); err != nil {
			return model.Node{}, err
		}
	}
	historyEntry.CapsFrom, historyEntry.CapsTo = common.DiffCaps(current.Caps, updated.Caps)

	if req.Limit != nil {
		if err := s.checkBudgetLimit(ctx, *current, chain, *req.Limit, true); err != nil {
			return model.Node{}, err
		}
		historyEntry.LimitFrom = &current.Limit
		historyEntry.LimitTo = req.Limit
		updated.Limit = *req.Limit
	}

	updated.History = append(slices.Clone(current.History), historyEntry)

	if err := s.store.UpsertNode(ctx, updated); err != nil {
		return model.Node{}, fmt.Errorf("persist node: %w", err)
	}
	if req.TerminationDate != nil {
		if _, err := s.shortenSubtreeEnds(ctx, []string{updated.ID}, *updated.TerminationDate, actor, endsWithReason(updated)); err != nil {
			return model.Node{}, err
		}
	}
	if req.MaxProjectTermDays != nil {
		days := *updated.MaxProjectTermDays
		if _, err := s.shortenSubtreeTerms(ctx, []string{updated.ID}, days, s.now(), actor, termReason(updated, days)); err != nil {
			return model.Node{}, err
		}
	}
	return updated, nil
}

// RequestChange proposes modifications to a node. On a pending node the request
// is amended in place (it is not yet approved). On an approved or change_pending
// leaf a change that needs nobody's decision (see leafChangeDecision) is applied
// at once; any other proposal is stored as pending changes and the node
// transitions to (or stays in) change_pending until the parent chain decides.
func (s *Service) RequestChange(id string, req ChangeNodeRequest, actor model.Actor, userTokens common.TokenList) (model.Node, error) {
	var err error
	if req.TerminationDate, err = normalizeEnd(req.TerminationDate); err != nil {
		return model.Node{}, err
	}
	// A change may take effect on the spot (see leafChangeDecision), which
	// commits capacity just like an approval does.
	s.approvalMu.Lock()
	defer s.approvalMu.Unlock()

	ctx, cancel := s.newCtx()
	defer cancel()

	current, err := s.loadLive(ctx, id)
	if err != nil {
		return model.Node{}, err
	}

	// Authorization: the leaf owner, a budget's own managers (asking their parent
	// for more), or a manager of the parent chain.
	// The budgets above: they decide who may ask, bound a new limit and end,
	// and the policy of the one a leaf draws from decides whether the change
	// needs anybody's approval.
	chain, err := s.parentChainNodes(ctx, current)
	if err != nil {
		return model.Node{}, err
	}
	caller := common.NewTokenSet(userTokens)
	if !(current.IsLeaf() && isOwner(userTokens, current)) && !managesNodeIn(caller, current, chain) {
		return model.Node{}, errNoPermission
	}
	var parent *model.Node
	if current.IsLeaf() && len(chain) > 0 {
		parent = &chain[0]
	}

	if req.TerminationDate != nil {
		if _, err := s.fitNodeEnd(current.Kind, req.TerminationDate, chain); err != nil {
			return model.Node{}, err
		}
	}

	if req.Limit != nil && current.InheritsLimit {
		return model.Node{}, invalid("Dieses Budget übernimmt das Limit des Budgets darüber.")
	}
	// What is asked for is checked as far as asking goes; room for it is
	// the decision's business (leafChangeDecision, or a manager).
	if req.Limit != nil {
		var err error
		if current.IsLeaf() {
			err = s.checkLeafLimit(ctx, *current, chain, *req.Limit, nil, false)
		} else {
			err = s.validateBudgetLimit(*req.Limit)
		}
		if err != nil {
			return model.Node{}, err
		}
	}

	if err := validateOptionalReason(req.Reason); err != nil {
		return model.Node{}, err
	}

	// The ceiling on members is the one of the project as it will be: a limit that brings a provider in brings its ceiling in too, also for the members it already has.
	var requestedLimit []common.ProjectQuota
	if req.Limit != nil {
		requestedLimit = append(requestedLimit, *req.Limit)
	}
	ceiling := s.memberCeilingOf(*current, requestedLimit...)
	if req.AuthorizedUsers == nil && current.IsLeaf() {
		if err := checkMemberCeilingFalls(len(current.AuthorizedUsers), s.memberCeilingOf(*current), ceiling); err != nil {
			return model.Node{}, err
		}
	}

	var normalizedAuthorizedUsers *[]common.AuthorizedUser
	if req.AuthorizedUsers != nil {
		if !current.IsLeaf() {
			return model.Node{}, invalid("Mitglieder gibt es nur an Projekten.")
		}
		manager := managesInChain(caller, chain)
		keep := [][]common.AuthorizedUser{current.AuthorizedUsers}
		if current.Pending != nil && current.Pending.AuthorizedUsers != nil {
			keep = append(keep, *current.Pending.AuthorizedUsers)
		}
		normalized, err := s.normalizeAuthorizedUsers(ctx, *req.AuthorizedUsers, ceiling, userTokens, manager, keep...)
		if err != nil {
			return model.Node{}, err
		}
		normalizedAuthorizedUsers = &normalized
	}

	updated := *current

	if current.Status == model.StatusPending {
		// Amend the not-yet-approved request in place.
		historyEntry := model.NewHistoryEntry("amended", actor, model.StatusPending)
		historyEntry.StatusFrom = new(model.StatusPending)
		if req.Limit != nil {
			historyEntry.LimitFrom = &current.Limit
			historyEntry.LimitTo = req.Limit
			updated.Limit = *req.Limit
		}
		if req.TerminationDate != nil {
			historyEntry.TerminationDateFrom = current.TerminationDate
			historyEntry.TerminationDateTo = req.TerminationDate
			updated.TerminationDate = req.TerminationDate
		}
		if normalizedAuthorizedUsers != nil {
			updated.AuthorizedUsers = *normalizedAuthorizedUsers
		}
		if req.Reason != nil {
			historyEntry.Reason = req.Reason
			updated.Reason = *req.Reason
		}
		updated.History = append(slices.Clone(current.History), historyEntry)
		if err := s.store.UpsertNode(ctx, updated); err != nil {
			return model.Node{}, fmt.Errorf("persist node: %w", err)
		}
		return updated, nil
	}

	// approved / change_pending: store (or replace) the pending proposal.
	pending := &model.PendingChanges{}
	if req.Limit != nil {
		pending.Limit = req.Limit
	}
	if req.TerminationDate != nil {
		pending.TerminationDate = req.TerminationDate
	}
	pending.AuthorizedUsers = normalizedAuthorizedUsers
	if pending.Limit == nil && pending.TerminationDate == nil && pending.AuthorizedUsers == nil {
		return model.Node{}, invalid("Die Änderungsanfrage enthält keine Änderung.")
	}

	historyEntry := model.NewHistoryEntry("change_requested", actor, model.StatusChangePending)
	historyEntry.StatusFrom = &current.Status
	if req.Limit != nil {
		historyEntry.LimitFrom = &current.Limit
		historyEntry.LimitTo = req.Limit
	}
	if req.TerminationDate != nil {
		historyEntry.TerminationDateFrom = current.TerminationDate
		historyEntry.TerminationDateTo = req.TerminationDate
	}
	if req.Reason != nil {
		historyEntry.Reason = req.Reason
	}

	updated.History = append(slices.Clone(current.History), historyEntry)

	direct, reason, beyondPolicy := false, "", false
	if current.IsLeaf() {
		if direct, reason, beyondPolicy, err = s.leafChangeDecision(ctx, current, parent, pending); err != nil {
			return model.Node{}, err
		}
	}
	if beyondPolicy && !parent.RequestsBeyondAutoApproveAllowed() {
		// A manager of the budget may still propose — and then approve — it;
		// the hard limit is for the people who request from it.
		if !managesInChain(caller, chain) {
			return model.Node{}, errBeyondAutoApprove
		}
	}
	if direct {
		// Applied as proposed — this also replaces an earlier proposal still
		// waiting for a decision, exactly as a new proposal would.
		if pending.Limit != nil {
			updated.Limit = *pending.Limit
		}
		if pending.TerminationDate != nil {
			updated.TerminationDate = pending.TerminationDate
		}
		if pending.AuthorizedUsers != nil {
			updated.AuthorizedUsers = *pending.AuthorizedUsers
		}
		updated.Pending = nil
		updated.Status = model.StatusApproved
		updated.History = append(updated.History, autoApprovedEntry(actor, model.StatusChangePending, reason))
	} else {
		updated.Pending = pending
		updated.Status = model.StatusChangePending
	}

	if err := s.store.UpsertNode(ctx, updated); err != nil {
		return model.Node{}, fmt.Errorf("persist node: %w", err)
	}
	return updated, nil
}

// errBeyondAutoApprove refuses a request that a budget with a hard
// auto-approve limit would otherwise queue for a manager.
var errBeyondAutoApprove = refuse(common.ErrForbidden, "Das ist mehr, als das Budget automatisch genehmigt, und darüber hinaus nimmt es keine Anfragen an. Etwas zurückgeben oder die Verwaltenden bitten, den eigenen Anteil zu erhöhen.")

// leafChangeDecision decides whether a proposed change to an active leaf takes
// effect without a manager, and why. Every part of the change has to qualify —
// one part that needs a decision sends the whole proposal to a manager, who
// sees it as the owner wrote it:
//
//   - members: always — who works in a project is its owner's business;
//   - resources: a pure shrink always; growth when the budget's auto-approve
//     policy would grant the new size as a new request;
//   - end date: an earlier one always; a later one when the budget's
//     auto-approve policy covers extensions and the date stays within the
//     budget's own end (the maximum term was checked before).
//
// beyondPolicy reports that the resources asked for are more than the policy
// grants — what a budget that takes no requests beyond it refuses. An
// extension the budget leaves to its managers is not that: it is a question
// the budget wants asked, so it waits for them.
func (s *Service) leafChangeDecision(ctx context.Context, current, parent *model.Node, change *model.PendingChanges) (direct bool, reason string, beyondPolicy bool, err error) {
	viaPolicy := false
	if change.Limit != nil && !s.limitShrinks(current.Limit, *change.Limit) {
		ok, err := s.autoApprovable(ctx, parent, current.Owner, *change.Limit, current.Limit)
		if err != nil {
			return false, "", false, err
		}
		if !ok {
			return false, "", true, nil
		}
		viaPolicy = true
	}
	if change.TerminationDate != nil && !endsNoLater(*change.TerminationDate, current.TerminationDate) {
		if !parent.ExtensionsAutoApproved() || parent.Status != model.StatusApproved ||
			!endsNoLater(*change.TerminationDate, parent.TerminationDate) {
			return false, "", false, nil
		}
		viaPolicy = true
	}
	if viaPolicy {
		return true, autoApproveReason(parent), false, nil
	}
	return true, "Direkt übernommen (gibt Ressourcen zurück, endet früher oder ändert nur Mitglieder)", false, nil
}

// ApproveNode approves a pending node or the pending changes of a change_pending
// node. Only managers of the PARENT chain may approve — a node's own admin scope
// deliberately does not count, so nobody approves their own budget or a peer's
// auto-approved request. ModifiedLimit lets the approver grant a different limit.
func (s *Service) ApproveNode(id string, req ApproveNodeRequest, actor model.Actor, userTokens common.TokenList) (model.Node, error) {
	s.approvalMu.Lock()
	defer s.approvalMu.Unlock()

	ctx, cancel := s.newCtx()
	defer cancel()

	current, err := s.loadNode(ctx, id)
	if err != nil {
		return model.Node{}, err
	}
	if current.Status != model.StatusPending && current.Status != model.StatusChangePending {
		return model.Node{}, refuse(common.ErrConflict, "Im Status %q lässt sich nichts genehmigen.", current.Status)
	}

	ancestors, err := s.parentChainNodes(ctx, current)
	if err != nil {
		return model.Node{}, err
	}
	if !managesInChain(common.NewTokenSet(userTokens), ancestors) {
		return model.Node{}, errNoPermission
	}
	if err := checkRevision(current, req.ExpectedRevision); err != nil {
		return model.Node{}, err
	}

	// Resolve the final values: explicit modification > pending proposal > current.
	finalLimit := current.Limit
	if current.Pending != nil && current.Pending.Limit != nil {
		finalLimit = *current.Pending.Limit
	}
	if req.ModifiedLimit != nil {
		finalLimit = *req.ModifiedLimit
	}

	finalTerminationDate := current.TerminationDate
	if current.Pending != nil && current.Pending.TerminationDate != nil {
		finalTerminationDate = current.Pending.TerminationDate
	}
	finalAuthorizedUsers := current.AuthorizedUsers
	if current.Pending != nil && current.Pending.AuthorizedUsers != nil {
		finalAuthorizedUsers = *current.Pending.AuthorizedUsers
	}

	// Requests made before the budget above got (or moved up) its end still
	// ask for more: they are granted to the budget's end, not refused whole.
	// The same for a project's maximum term, counted from today.
	bound, _ := s.endBound(current.Kind, ancestors)
	if bound != nil &&
		(finalTerminationDate == nil || !endsNoLater(*finalTerminationDate, bound)) {
		finalTerminationDate = bound
	}

	// A change_pending node's current limit is in force: a leaf's is already
	// charged and is replaced, a budget's is what everything below relies on.
	inForce := current.Status == model.StatusChangePending
	if current.IsLeaf() {
		var subtract common.ProjectQuota
		if inForce {
			subtract = current.Limit
		}
		if err := s.checkLeafLimit(ctx, *current, ancestors, finalLimit, subtract, true); err != nil {
			return model.Node{}, err
		}
	} else if !current.InheritsLimit {
		if err := s.checkBudgetLimit(ctx, *current, ancestors, finalLimit, inForce); err != nil {
			return model.Node{}, err
		}
	}

	historyEntry := model.NewHistoryEntry("approved", actor, model.StatusApproved)
	historyEntry.StatusFrom = &current.Status
	if req.ModifiedLimit != nil || (current.Pending != nil && current.Pending.Limit != nil) {
		historyEntry.LimitFrom = &current.Limit
		historyEntry.LimitTo = &finalLimit
	}
	if !sameEnd(current.TerminationDate, finalTerminationDate) {
		historyEntry.TerminationDateFrom = current.TerminationDate
		historyEntry.TerminationDateTo = finalTerminationDate
	}

	// Written on the node as it is now: a reconciler pass since the read is kept, and a request that changed after all is refused rather than overwritten.
	var updated model.Node
	if _, err := s.store.UpdateNode(ctx, id, func(n *model.Node) error {
		if n.Status != current.Status {
			return refuse(common.ErrConflict, "Im Status %q lässt sich nichts genehmigen.", n.Status)
		}
		if err := checkRevision(n, req.ExpectedRevision); err != nil {
			return err
		}
		n.Status = model.StatusApproved
		n.Limit = finalLimit
		n.TerminationDate = finalTerminationDate
		n.AuthorizedUsers = finalAuthorizedUsers
		n.Pending = nil
		n.History = append(slices.Clone(n.History), historyEntry)
		updated = *n
		return nil
	}); err != nil {
		return model.Node{}, err
	}
	if updated.ID == "" {
		return model.Node{}, errNodeNotFound
	}
	if !current.IsLeaf() && finalTerminationDate != nil {
		if _, err := s.shortenSubtreeEnds(ctx, []string{updated.ID}, *finalTerminationDate, actor, endsWithReason(updated)); err != nil {
			return model.Node{}, err
		}
	}
	return updated, nil
}

// checkRevision refuses to decide on a node whose revision is not the one the caller saw, and a decision that does not say which one that was.
func checkRevision(n *model.Node, expected *int) error {
	if expected == nil {
		return invalid("expected_revision ist erforderlich: der Stand der Anfrage, wie sie angezeigt wurde.")
	}
	if got := n.CurrentRevision(); got != *expected {
		return refuse(common.ErrConflict, "Die Anfrage hat sich seit der Anzeige geändert (Stand %d, jetzt %d). Bitte neu laden und noch einmal entscheiden.", *expected, got)
	}
	return nil
}

// sameEnd reports whether two optional end dates name the same day (model.SameEnd), so a date written differently is no change.
func sameEnd(a, b *string) bool { return model.SameEnd(a, b) }

// RejectNode rejects a pending node (→ rejected, terminal) or discards the pending
// changes of a change_pending node (→ back to approved — the previously approved
// state stays valid; the old model killed the whole project here).
func (s *Service) RejectNode(id string, req RejectNodeRequest, actor model.Actor, userTokens common.TokenList) (model.Node, error) {
	if err := validateOptionalReason(req.Reason); err != nil {
		return model.Node{}, err
	}
	// Serialized with the approvals, so an approval and a rejection of the same request cannot both pass their checks.
	s.approvalMu.Lock()
	defer s.approvalMu.Unlock()

	ctx, cancel := s.newCtx()
	defer cancel()

	current, err := s.loadNode(ctx, id)
	if err != nil {
		return model.Node{}, err
	}
	if current.Status != model.StatusPending && current.Status != model.StatusChangePending {
		return model.Node{}, refuse(common.ErrConflict, "Im Status %q lässt sich nichts ablehnen.", current.Status)
	}

	if manages, err := s.managesParentChain(ctx, userTokens, current); err != nil {
		return model.Node{}, err
	} else if !manages {
		return model.Node{}, errNoPermission
	}

	var updated model.Node
	if _, err := s.store.UpdateNode(ctx, id, func(n *model.Node) error {
		if n.Status != current.Status || derefParent(n.ParentID) != derefParent(current.ParentID) {
			return refuse(common.ErrConflict, "Inzwischen geändert (Status %q). Bitte neu laden.", n.Status)
		}
		var historyEntry model.HistoryEntry
		if n.Status == model.StatusPending {
			historyEntry = model.NewHistoryEntry("rejected", actor, model.StatusRejected)
			n.Status = model.StatusRejected
		} else {
			historyEntry = model.NewHistoryEntry("change_rejected", actor, model.StatusApproved)
			n.Status = model.StatusApproved
		}
		historyEntry.StatusFrom = new(current.Status)
		if req.Reason != nil && strings.TrimSpace(*req.Reason) != "" {
			historyEntry.Reason = req.Reason
		}
		n.Pending = nil
		n.History = append(slices.Clone(n.History), historyEntry)
		updated = *n
		return nil
	}); err != nil {
		return model.Node{}, err
	}
	if updated.ID == "" {
		return model.Node{}, errNodeNotFound
	}
	return updated, nil
}
