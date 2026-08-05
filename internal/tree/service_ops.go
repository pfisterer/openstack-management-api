package tree

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pfisterer/openstack-management-api/internal/common"
)

// ── Reads / views ─────────────────────────────────────────────────────────────

// GetNode fetches a single node, enforcing read authorization: the leaf owner, an
// authorized user, a manager of the node or its ancestors, or an eligible
// requester of the budget may read it. Returns (nil, nil) when not found.
func (s *Service) GetNode(id string, userTokens common.TokenList) (*Node, error) {
	if len(userTokens) == 0 {
		return nil, common.ErrForbidden
	}
	ctx, cancel := s.newCtx()
	defer cancel()

	node, err := s.store.GetNode(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("load node: %w", err)
	}
	if node == nil {
		return nil, nil
	}

	allowed := isOwner(userTokens, node) ||
		isAuthorizedUser(userTokens, node) ||
		isEligibleRequester(userTokens, node)
	if !allowed {
		manages, err := s.managesNode(ctx, userTokens, node)
		if err != nil {
			return nil, err
		}
		allowed = manages
	}
	if !allowed {
		return nil, common.ErrForbidden
	}

	withUsage, err := s.attachUsage(ctx, []Node{*node})
	if err != nil {
		return nil, err
	}
	return &withUsage[0], nil
}

// ListChildren returns the direct children of a budget. Management view: only
// managers of the budget (or its ancestors) may list children.
func (s *Service) ListChildren(parentID string, userTokens common.TokenList, limit, offset int) ([]Node, error) {
	if len(userTokens) == 0 {
		return nil, common.ErrForbidden
	}
	limit, offset = normalizePagination(limit, offset)
	ctx, cancel := s.newCtx()
	defer cancel()

	parent, err := s.store.GetNode(ctx, parentID)
	if err != nil {
		return nil, fmt.Errorf("load parent node: %w", err)
	}
	if parent == nil {
		return nil, fmt.Errorf("node %w", common.ErrNotFound)
	}
	if manages, err := s.managesNode(ctx, userTokens, parent); err != nil {
		return nil, err
	} else if !manages {
		return nil, common.ErrForbidden
	}

	children, err := s.store.ListNodes(ctx, NodeQuery{ParentIDs: []string{parentID}}, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("load children: %w", err)
	}
	return s.attachUsage(ctx, children)
}

// ListMine returns the leaves owned by the given user (email-scoped view).
func (s *Service) ListMine(userEmail string, limit, offset int) ([]Node, error) {
	if strings.TrimSpace(userEmail) == "" {
		return nil, fmt.Errorf("missing user email")
	}
	limit, offset = normalizePagination(limit, offset)
	ctx, cancel := s.newCtx()
	defer cancel()

	owner, err := normalizeOwnerToken(userEmail)
	if err != nil {
		return nil, err
	}
	leaves, err := s.store.ListNodes(ctx, NodeQuery{Kinds: []string{KindProject}, Owner: owner}, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("load owned projects: %w", err)
	}
	return s.attachUsage(ctx, leaves)
}

// ListMyBudgets returns the budgets whose AdminScope directly contains one of the
// caller's tokens — "budgets delegated to me". Children of these are navigated
// via ListChildren.
func (s *Service) ListMyBudgets(userTokens common.TokenList, limit, offset int) ([]Node, error) {
	if len(userTokens) == 0 {
		return nil, fmt.Errorf("no user tokens found")
	}
	limit, offset = normalizePagination(limit, offset)
	ctx, cancel := s.newCtx()
	defer cancel()

	budgets, err := s.store.ListNodes(ctx, NodeQuery{
		Kinds:         []string{KindBudget},
		AdminScopeAny: userTokens,
	}, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("load administered budgets: %w", err)
	}
	return s.attachUsage(ctx, budgets)
}

// ListEligibleForMe returns the approved budgets the caller may submit requests to.
func (s *Service) ListEligibleForMe(userTokens common.TokenList, limit, offset int) ([]Node, error) {
	if len(userTokens) == 0 {
		return nil, fmt.Errorf("no user tokens found")
	}
	limit, offset = normalizePagination(limit, offset)
	ctx, cancel := s.newCtx()
	defer cancel()

	budgets, err := s.store.ListNodes(ctx, NodeQuery{
		Kinds:       []string{KindBudget},
		Statuses:    []string{StatusApproved},
		EligibleAny: userTokens,
	}, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("load eligible budgets: %w", err)
	}
	return s.attachUsage(ctx, budgets)
}

// ListEligibleForOwner returns the approved budgets the given owner tokens may
// request under. Root-admin only — used by the promote flow so the admin can see
// which budgets make sense as a promotion target for a specific owner.
func (s *Service) ListEligibleForOwner(callerTokens common.TokenList, ownerTokens common.TokenList, limit, offset int) ([]Node, error) {
	if !common.NewTokenSet(s.rootAdminTokens).ContainsAny(callerTokens) {
		return nil, common.ErrForbidden
	}
	if len(ownerTokens) == 0 {
		return nil, fmt.Errorf("owner_tokens must not be empty")
	}
	return s.ListEligibleForMe(ownerTokens, limit, offset)
}

// ListToManage returns the nodes awaiting a decision by the caller: pending and
// change_pending nodes (budgets AND leaves) plus imported leaves, hanging under
// the budgets the caller directly administers.
//
// includeSubtree widens that to the whole subtree below those budgets. Off by
// default, because a request under a delegated sub-budget is that manager's job:
// for a root admin the wide list is the entire organization, which drowns the
// handful of requests actually addressed to them. On it answers the other
// question — "is anything stuck anywhere below me?".
func (s *Service) ListToManage(userTokens common.TokenList, includeSubtree bool, limit, offset int) ([]Node, error) {
	if len(userTokens) == 0 {
		return nil, fmt.Errorf("no user tokens found")
	}
	limit, offset = normalizePagination(limit, offset)
	ctx, cancel := s.newCtx()
	defer cancel()

	administered, err := s.store.ListNodes(ctx, NodeQuery{
		Kinds:         []string{KindBudget},
		AdminScopeAny: userTokens,
	}, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("load administered budgets: %w", err)
	}
	if len(administered) == 0 {
		return []Node{}, nil
	}

	// The parents whose children the caller decides on.
	var parentIDs []string
	if includeSubtree {
		parentMap, err := s.buildSubtreeParentMap(ctx, administered)
		if err != nil {
			return nil, fmt.Errorf("collect administered subtrees: %w", err)
		}
		for id := range parentMap {
			parentIDs = append(parentIDs, id)
		}
	} else {
		parentIDs, err = s.undelegatedBudgetIDs(ctx, administered, userTokens)
		if err != nil {
			return nil, err
		}
	}

	waiting, err := s.store.ListNodes(ctx, NodeQuery{
		ParentIDs: parentIDs,
		Statuses:  []string{StatusPending, StatusChangePending, StatusImported},
	}, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("load requests to manage: %w", err)
	}
	// The name of the funding budget travels with the request, so the inbox can
	// say where something arrived without a lookup per entry.
	return s.attachParentNames(ctx, waiting)
}

// ── Create / request ──────────────────────────────────────────────────────────

// CreateNode creates a child node under req.ParentID. Managers of the parent (or
// its ancestors) create directly approved children; eligible requesters submit a
// pending request, which may be auto-approved by the parent's AutoApprove policy.
func (s *Service) CreateNode(req CreateNodeRequest, actor string, userEmail string, userTokens common.TokenList) (Node, error) {
	if strings.TrimSpace(userEmail) == "" || len(userTokens) == 0 {
		return Node{}, common.ErrForbidden
	}

	// Validate the request shape before taking the approval lock.
	switch req.Kind {
	case KindProject:
		if err := s.validateLeafLimit(req.Limit); err != nil {
			return Node{}, err
		}
	case KindBudget:
		if strings.TrimSpace(req.Name) == "" {
			return Node{}, fmt.Errorf("budgets require a name")
		}
		// A budget without an admin scope is invisible in the UI: "My Budgets"
		// matches AdminScope directly (the ancestor rule does not apply there),
		// so nobody would find it, and every request under it would surface at
		// the nearest ancestor manager instead. The structural "unassigned" node
		// is the deliberate exception and is created by Bootstrap, not here.
		if len(req.AdminScope) == 0 {
			return Node{}, fmt.Errorf("budgets require at least one entry in admin_scope (who manages it)")
		}
		if err := s.validateBudgetLimit(req.Limit); err != nil {
			return Node{}, err
		}
		if err := s.validateAutoApprove(req.AutoApprove); err != nil {
			return Node{}, err
		}
	default:
		return Node{}, fmt.Errorf("invalid kind %q", req.Kind)
	}

	normalizedAuthorizedUsers, err := normalizeAuthorizedUsers(req.AuthorizedUsers)
	if err != nil {
		return Node{}, err
	}

	// Serialize the capacity-affecting approval paths below.
	s.approvalMu.Lock()
	defer s.approvalMu.Unlock()

	ctx, cancel := s.newCtx()
	defer cancel()

	parent, err := s.store.GetNode(ctx, req.ParentID)
	if err != nil {
		return Node{}, fmt.Errorf("load parent node: %w", err)
	}
	if parent == nil {
		return Node{}, fmt.Errorf("parent node %w", common.ErrNotFound)
	}
	if parent.Kind != KindBudget {
		return Node{}, fmt.Errorf("parent must be a budget")
	}
	if parent.Status != StatusApproved {
		return Node{}, fmt.Errorf("cannot create nodes under a budget in status %q", parent.Status)
	}

	isManager, err := s.managesNode(ctx, userTokens, parent)
	if err != nil {
		return Node{}, err
	}
	if !isManager && !isEligibleRequester(userTokens, parent) {
		return Node{}, common.ErrForbidden
	}
	// Requesters may be limited to leaves: a course budget usually wants
	// projects from its students, not a sub-budget tree underneath. Managers are
	// exempt — they own the structure and create sub-budgets directly.
	if req.Kind == KindBudget && !isManager && !parent.SubBudgetRequestsAllowed() {
		return Node{}, fmt.Errorf("%w: this budget does not accept sub-budget requests — request a project instead", common.ErrForbidden)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	node := Node{
		Kind:            req.Kind,
		ParentID:        &parent.ID,
		Status:          StatusPending,
		Name:            strings.TrimSpace(req.Name),
		Reason:          req.Reason,
		Limit:           req.Limit,
		TerminationDate: req.TerminationDate,
		CreatedBy:       userEmail,
		CreatedAt:       now,
	}

	createdEntry := newHistoryEntry("created", actor, StatusPending)
	createdEntry.LimitTo = &req.Limit
	createdEntry.Reason = ptr(req.Reason)

	if req.Kind == KindProject {
		node.ID = "p_" + uuid.New().String()
		owner, err := normalizeOwnerToken(userEmail)
		if err != nil {
			return Node{}, err
		}
		node.Owner = owner
		node.AuthorizedUsers = normalizedAuthorizedUsers
	} else {
		node.ID = "b_" + uuid.New().String()
		node.AdminScope = req.AdminScope
		node.EligibleRequesters = req.EligibleRequesters
		node.AutoApprove = req.AutoApprove
		node.AllowSubBudgetRequests = req.AllowSubBudgetRequests
	}
	node.History = []HistoryEntry{createdEntry}

	// Decide the initial status.
	switch {
	case isManager:
		// Direct creation by a manager of the parent chain — approved immediately,
		// subject to the same checks an explicit approval would run.
		if req.Kind == KindProject {
			ancestors, err := s.nodeChain(ctx, parent.ID)
			if err != nil {
				return Node{}, err
			}
			if err := s.checkCapacity(ctx, ancestors, node.Limit, nil); err != nil {
				return Node{}, err
			}
		} else {
			if err := s.validateChildBudgetLimit(parent, node.Limit); err != nil {
				return Node{}, err
			}
		}
		node.Status = StatusApproved
		approvedEntry := newHistoryEntry("approved", actor, StatusApproved)
		approvedEntry.StatusFrom = ptr(StatusPending)
		approvedEntry.Reason = ptr("Created directly by a manager")
		node.History = append(node.History, approvedEntry)

	case req.Kind == KindProject && parent.AutoApprove != nil:
		// Auto-approve: cumulative per-owner usage under this budget must stay
		// within the per-requester limit, and every ancestor must have capacity.
		usage, err := s.ownerActiveUsage(ctx, parent.ID, node.Owner)
		if err != nil {
			return Node{}, fmt.Errorf("compute per-requester usage: %w", err)
		}
		cumulative := quotaAdd(usage, node.Limit, s.resourceIDs)
		if quotaFits(cumulative, parent.AutoApprove.PerRequesterLimit, s.resourceIDs) {
			ancestors, err := s.nodeChain(ctx, parent.ID)
			if err != nil {
				return Node{}, err
			}
			if err := s.checkCapacity(ctx, ancestors, node.Limit, nil); err == nil {
				node.Status = StatusApproved
				autoEntry := newHistoryEntry("approved", "system:auto-approval", StatusApproved)
				autoEntry.StatusFrom = ptr(StatusPending)
				autoEntry.Reason = ptr("Auto-approved (within per-requester limit)")
				node.History = append(node.History, autoEntry)
			}
		}
	}

	if err := s.store.UpsertNode(ctx, node); err != nil {
		return Node{}, fmt.Errorf("persist node: %w", err)
	}
	return node, nil
}

// ── Direct edit (budgets) ─────────────────────────────────────────────────────

// UpdateNode applies immediate edits to a budget. Policy fields (name, admin
// scope, eligible requesters, auto-approve) require a manager of the node or its
// ancestors. Limit and termination date require a manager of the PARENT chain —
// you cannot raise your own budget; request a change instead.
func (s *Service) UpdateNode(id string, req UpdateNodeRequest, actor string, userTokens common.TokenList) (Node, error) {
	ctx, cancel := s.newCtx()
	defer cancel()

	current, err := s.store.GetNode(ctx, id)
	if err != nil {
		return Node{}, fmt.Errorf("load node: %w", err)
	}
	if current == nil {
		return Node{}, fmt.Errorf("node %w", common.ErrNotFound)
	}
	if current.Kind != KindBudget {
		return Node{}, fmt.Errorf("only budgets can be edited directly; use request-change for leaves")
	}
	if IsTerminalStatus(current.Status) {
		return Node{}, fmt.Errorf("cannot edit node in status %q", current.Status)
	}

	wantsPolicyEdit := req.Name != nil || req.AdminScope != nil || req.EligibleRequesters != nil ||
		req.AutoApprove != nil || req.ClearAutoApprove || req.AllowSubBudgetRequests != nil
	wantsCapacityEdit := req.Limit != nil || req.TerminationDate != nil || req.ClearTerminationDate

	if wantsPolicyEdit {
		if manages, err := s.managesNode(ctx, userTokens, current); err != nil {
			return Node{}, err
		} else if !manages {
			return Node{}, common.ErrForbidden
		}
	}
	if wantsCapacityEdit {
		if manages, err := s.managesParentChain(ctx, userTokens, current); err != nil {
			return Node{}, err
		} else if !manages {
			return Node{}, common.ErrForbidden
		}
	}

	// The root node's admin scope is owned by the configuration (synced on every
	// startup); an API edit would silently revert on the next deployment.
	if current.ID == RootNodeID && req.AdminScope != nil {
		return Node{}, fmt.Errorf("the root admin scope is managed via ROOT_ADMIN_TOKENS: %w", common.ErrForbidden)
	}

	updated := *current
	if req.Name != nil {
		updated.Name = strings.TrimSpace(*req.Name)
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
	if req.ClearAutoApprove {
		updated.AutoApprove = nil
	} else if req.AutoApprove != nil {
		if err := s.validateAutoApprove(req.AutoApprove); err != nil {
			return Node{}, err
		}
		updated.AutoApprove = req.AutoApprove
	}
	if req.ClearTerminationDate {
		updated.TerminationDate = nil
	} else if req.TerminationDate != nil {
		updated.TerminationDate = req.TerminationDate
	}

	historyEntry := newHistoryEntry("updated", actor, updated.Status)

	if req.Limit != nil {
		if err := s.validateBudgetLimit(*req.Limit); err != nil {
			return Node{}, err
		}
		if current.ParentID != nil {
			parent, err := s.store.GetNode(ctx, *current.ParentID)
			if err != nil {
				return Node{}, fmt.Errorf("load parent for limit check: %w", err)
			}
			if parent != nil {
				if err := s.validateChildBudgetLimit(parent, *req.Limit); err != nil {
					return Node{}, err
				}
			}
		}
		historyEntry.LimitFrom = &current.Limit
		historyEntry.LimitTo = req.Limit
		updated.Limit = *req.Limit

		// A reduction must not fall below the subtree's current active usage.
		subtreeUsage, err := s.loadSubtreeUsage(ctx, []Node{updated})
		if err != nil {
			return Node{}, fmt.Errorf("compute current usage for limit check: %w", err)
		}
		activeUsage := subtreeUsage[updated.ID].Total(s.resourceIDs)
		for _, resourceID := range s.resourceIDs {
			newCap := updated.Limit[resourceID]
			if newCap != common.UnlimitedQuota && activeUsage[resourceID] > newCap {
				return Node{}, fmt.Errorf("new limit for %q (%d) is below current active usage (%d)", resourceID, newCap, activeUsage[resourceID])
			}
		}
	}

	updated.History = append(slices.Clone(current.History), historyEntry)

	if err := s.store.UpsertNode(ctx, updated); err != nil {
		return Node{}, fmt.Errorf("persist node: %w", err)
	}
	return updated, nil
}

// ── Change requests ───────────────────────────────────────────────────────────

// RequestChange proposes modifications that require approval by the parent chain.
// On a pending node the request is amended in place (it is not yet approved); on
// an approved or change_pending node the proposal is stored as pending changes and
// the node transitions to (or stays in) change_pending.
func (s *Service) RequestChange(id string, req ChangeNodeRequest, actor string, userTokens common.TokenList) (Node, error) {
	ctx, cancel := s.newCtx()
	defer cancel()

	current, err := s.store.GetNode(ctx, id)
	if err != nil {
		return Node{}, fmt.Errorf("load node: %w", err)
	}
	if current == nil {
		return Node{}, fmt.Errorf("node %w", common.ErrNotFound)
	}
	if current.Status == StatusImported {
		return Node{}, fmt.Errorf("imported nodes are read-only until promoted: %w", common.ErrForbidden)
	}
	if IsTerminalStatus(current.Status) {
		return Node{}, fmt.Errorf("cannot modify node in status %q", current.Status)
	}

	// Authorization: the leaf owner, a budget's own managers (asking their parent
	// for more), or a manager of the parent chain.
	allowed := false
	if current.IsLeaf() {
		allowed = isOwner(userTokens, current)
	}
	if !allowed {
		if manages, err := s.managesNode(ctx, userTokens, current); err != nil {
			return Node{}, err
		} else {
			allowed = manages
		}
	}
	if !allowed {
		return Node{}, common.ErrForbidden
	}

	if req.Limit != nil {
		var validationErr error
		if current.IsLeaf() {
			validationErr = s.validateLeafLimit(*req.Limit)
		} else {
			validationErr = s.validateBudgetLimit(*req.Limit)
		}
		if validationErr != nil {
			return Node{}, validationErr
		}
	}

	var normalizedAuthorizedUsers *[]common.AuthorizedUser
	if req.AuthorizedUsers != nil {
		if !current.IsLeaf() {
			return Node{}, fmt.Errorf("authorized_users can only be changed on project leaves")
		}
		normalized, err := normalizeAuthorizedUsers(*req.AuthorizedUsers)
		if err != nil {
			return Node{}, err
		}
		normalizedAuthorizedUsers = &normalized
	}

	updated := *current

	if current.Status == StatusPending {
		// Amend the not-yet-approved request in place.
		historyEntry := newHistoryEntry("amended", actor, StatusPending)
		historyEntry.StatusFrom = ptr(StatusPending)
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
			return Node{}, fmt.Errorf("persist node: %w", err)
		}
		return updated, nil
	}

	// approved / change_pending: store (or replace) the pending proposal.
	pending := &PendingChanges{}
	if req.Limit != nil {
		pending.Limit = req.Limit
	}
	if req.TerminationDate != nil {
		pending.TerminationDate = req.TerminationDate
	}
	pending.AuthorizedUsers = normalizedAuthorizedUsers
	if pending.Limit == nil && pending.TerminationDate == nil && pending.AuthorizedUsers == nil {
		return Node{}, fmt.Errorf("change request contains no changes")
	}

	historyEntry := newHistoryEntry("change_requested", actor, StatusChangePending)
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

	updated.Pending = pending
	updated.Status = StatusChangePending
	updated.History = append(slices.Clone(current.History), historyEntry)

	if err := s.store.UpsertNode(ctx, updated); err != nil {
		return Node{}, fmt.Errorf("persist node: %w", err)
	}
	return updated, nil
}

// ── Approve / reject / release ────────────────────────────────────────────────

// ApproveNode approves a pending node or the pending changes of a change_pending
// node. Only managers of the PARENT chain may approve — a node's own admin scope
// deliberately does not count, so nobody approves their own budget or a peer's
// auto-approved request. ModifiedLimit lets the approver grant a different limit.
func (s *Service) ApproveNode(id string, req ApproveNodeRequest, actor string, userTokens common.TokenList) (Node, error) {
	s.approvalMu.Lock()
	defer s.approvalMu.Unlock()

	ctx, cancel := s.newCtx()
	defer cancel()

	current, err := s.store.GetNode(ctx, id)
	if err != nil {
		return Node{}, fmt.Errorf("load node: %w", err)
	}
	if current == nil {
		return Node{}, fmt.Errorf("node %w", common.ErrNotFound)
	}
	if current.Status != StatusPending && current.Status != StatusChangePending {
		return Node{}, fmt.Errorf("cannot approve node in status %q", current.Status)
	}

	if manages, err := s.managesParentChain(ctx, userTokens, current); err != nil {
		return Node{}, err
	} else if !manages {
		return Node{}, common.ErrForbidden
	}

	// Resolve the final values: explicit modification > pending proposal > current.
	finalLimit := current.Limit
	if current.Pending != nil && current.Pending.Limit != nil {
		finalLimit = *current.Pending.Limit
	}
	if req.ModifiedLimit != nil {
		var validationErr error
		if current.IsLeaf() {
			validationErr = s.validateLeafLimit(*req.ModifiedLimit)
		} else {
			validationErr = s.validateBudgetLimit(*req.ModifiedLimit)
		}
		if validationErr != nil {
			return Node{}, validationErr
		}
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

	ancestors, err := s.parentChainNodes(ctx, current)
	if err != nil {
		return Node{}, err
	}

	if current.IsLeaf() {
		// Capacity: a change_pending leaf's CURRENT limit is already committed and
		// must be subtracted before adding the final limit.
		var subtract common.ProjectQuota
		if current.Status == StatusChangePending {
			subtract = current.Limit
		}
		if err := s.checkCapacity(ctx, ancestors, finalLimit, subtract); err != nil {
			return Node{}, err
		}
	} else {
		if len(ancestors) > 0 {
			if err := s.validateChildBudgetLimit(&ancestors[0], finalLimit); err != nil {
				return Node{}, err
			}
		}
		// A budget shrinking below its subtree's active usage would strand
		// already-approved leaves.
		if current.Status == StatusChangePending {
			probe := *current
			probe.Limit = finalLimit
			subtreeUsage, err := s.loadSubtreeUsage(ctx, []Node{probe})
			if err != nil {
				return Node{}, fmt.Errorf("compute subtree usage for limit check: %w", err)
			}
			activeUsage := subtreeUsage[probe.ID].Total(s.resourceIDs)
			for _, resourceID := range s.resourceIDs {
				newCap := finalLimit[resourceID]
				if newCap != common.UnlimitedQuota && activeUsage[resourceID] > newCap {
					return Node{}, fmt.Errorf("new limit for %q (%d) is below current active usage (%d)", resourceID, newCap, activeUsage[resourceID])
				}
			}
		}
	}

	historyEntry := newHistoryEntry("approved", actor, StatusApproved)
	historyEntry.StatusFrom = &current.Status
	if req.ModifiedLimit != nil || (current.Pending != nil && current.Pending.Limit != nil) {
		historyEntry.LimitFrom = &current.Limit
		historyEntry.LimitTo = &finalLimit
	}

	updated := *current
	updated.Status = StatusApproved
	updated.Limit = finalLimit
	updated.TerminationDate = finalTerminationDate
	updated.AuthorizedUsers = finalAuthorizedUsers
	updated.Pending = nil
	updated.History = append(slices.Clone(current.History), historyEntry)

	if err := s.store.UpsertNode(ctx, updated); err != nil {
		return Node{}, fmt.Errorf("persist node: %w", err)
	}
	return updated, nil
}

// RejectNode rejects a pending node (→ rejected, terminal) or discards the pending
// changes of a change_pending node (→ back to approved — the previously approved
// state stays valid; the old model killed the whole project here).
func (s *Service) RejectNode(id string, req RejectNodeRequest, actor string, userTokens common.TokenList) (Node, error) {
	ctx, cancel := s.newCtx()
	defer cancel()

	current, err := s.store.GetNode(ctx, id)
	if err != nil {
		return Node{}, fmt.Errorf("load node: %w", err)
	}
	if current == nil {
		return Node{}, fmt.Errorf("node %w", common.ErrNotFound)
	}
	if current.Status != StatusPending && current.Status != StatusChangePending {
		return Node{}, fmt.Errorf("cannot reject node in status %q", current.Status)
	}

	if manages, err := s.managesParentChain(ctx, userTokens, current); err != nil {
		return Node{}, err
	} else if !manages {
		return Node{}, common.ErrForbidden
	}

	updated := *current
	var historyEntry HistoryEntry
	if current.Status == StatusPending {
		historyEntry = newHistoryEntry("rejected", actor, StatusRejected)
		updated.Status = StatusRejected
	} else {
		historyEntry = newHistoryEntry("change_rejected", actor, StatusApproved)
		updated.Status = StatusApproved
	}
	historyEntry.StatusFrom = &current.Status
	if req.Reason != nil && strings.TrimSpace(*req.Reason) != "" {
		historyEntry.Reason = req.Reason
	}

	updated.Pending = nil
	updated.History = append(slices.Clone(current.History), historyEntry)

	if err := s.store.UpsertNode(ctx, updated); err != nil {
		return Node{}, fmt.Errorf("persist node: %w", err)
	}
	return updated, nil
}

// ReleaseNode marks an approved leaf as released, returning its capacity and
// driving OpenStack deprovisioning on the next reconcile. The owner or a manager
// of the parent chain may release.
func (s *Service) ReleaseNode(id string, actor string, userTokens common.TokenList) (Node, error) {
	ctx, cancel := s.newCtx()
	defer cancel()

	current, err := s.store.GetNode(ctx, id)
	if err != nil {
		return Node{}, fmt.Errorf("load node: %w", err)
	}
	if current == nil {
		return Node{}, fmt.Errorf("node %w", common.ErrNotFound)
	}
	if !current.IsLeaf() {
		return Node{}, fmt.Errorf("only project leaves can be released; delete budgets instead")
	}
	if current.Status != StatusApproved {
		return Node{}, fmt.Errorf("cannot release node in status %q", current.Status)
	}

	allowed := isOwner(userTokens, current)
	if !allowed {
		if manages, err := s.managesParentChain(ctx, userTokens, current); err != nil {
			return Node{}, err
		} else {
			allowed = manages
		}
	}
	if !allowed {
		return Node{}, common.ErrForbidden
	}

	historyEntry := newHistoryEntry("released", actor, StatusReleased)
	historyEntry.StatusFrom = ptr(StatusApproved)
	historyEntry.LimitFrom = &current.Limit

	updated := *current
	updated.Status = StatusReleased
	updated.Pending = nil
	updated.History = append(slices.Clone(current.History), historyEntry)

	if err := s.store.UpsertNode(ctx, updated); err != nil {
		return Node{}, fmt.Errorf("persist node: %w", err)
	}
	return updated, nil
}

// ── Structure operations ──────────────────────────────────────────────────────

// ReparentNode moves a node under a new parent budget. Requires management rights
// on the node's CURRENT parent chain (you give it away) AND on the new parent (you
// receive it). Active nodes are capacity-checked against the new ancestor chain.
func (s *Service) ReparentNode(id string, req ReparentNodeRequest, actor string, userTokens common.TokenList) (Node, error) {
	s.approvalMu.Lock()
	defer s.approvalMu.Unlock()

	ctx, cancel := s.newCtx()
	defer cancel()

	current, err := s.store.GetNode(ctx, id)
	if err != nil {
		return Node{}, fmt.Errorf("load node: %w", err)
	}
	if current == nil {
		return Node{}, fmt.Errorf("node %w", common.ErrNotFound)
	}
	if current.ID == RootNodeID || current.ID == UnassignedNodeID {
		return Node{}, fmt.Errorf("structural nodes cannot be moved: %w", common.ErrForbidden)
	}
	if current.Status == StatusImported {
		return Node{}, fmt.Errorf("imported nodes are moved via promote: %w", common.ErrForbidden)
	}
	if IsTerminalStatus(current.Status) {
		return Node{}, fmt.Errorf("cannot move node in status %q", current.Status)
	}
	if current.ParentID != nil && *current.ParentID == req.NewParentID {
		return Node{}, fmt.Errorf("node is already under this parent")
	}

	newParent, err := s.store.GetNode(ctx, req.NewParentID)
	if err != nil {
		return Node{}, fmt.Errorf("load new parent: %w", err)
	}
	if newParent == nil {
		return Node{}, fmt.Errorf("new parent %w", common.ErrNotFound)
	}
	if newParent.Kind != KindBudget || newParent.Status != StatusApproved {
		return Node{}, fmt.Errorf("new parent must be an approved budget")
	}
	if newParent.ID == UnassignedNodeID {
		return Node{}, fmt.Errorf("nodes cannot be moved into the unassigned collection: %w", common.ErrForbidden)
	}

	// Cycle guard: the new parent must not lie inside the node's own subtree.
	newParentChain, err := s.nodeChain(ctx, newParent.ID)
	if err != nil {
		return Node{}, err
	}
	for _, ancestor := range newParentChain {
		if ancestor.ID == current.ID {
			return Node{}, fmt.Errorf("cannot move a node into its own subtree")
		}
	}

	// Authorization on both sides.
	if manages, err := s.managesParentChain(ctx, userTokens, current); err != nil {
		return Node{}, err
	} else if !manages {
		return Node{}, common.ErrForbidden
	}
	if manages, err := s.managesNode(ctx, userTokens, newParent); err != nil {
		return Node{}, err
	} else if !manages {
		return Node{}, common.ErrForbidden
	}

	// Capacity / limit invariants against the new chain for nodes that carry
	// active consumption. Pending nodes move freely — they are checked at approval.
	if slices.Contains(ActiveStatuses, current.Status) {
		if current.IsLeaf() {
			if err := s.checkCapacity(ctx, newParentChain, current.Limit, nil); err != nil {
				return Node{}, err
			}
		} else {
			if err := s.validateChildBudgetLimit(newParent, current.Limit); err != nil {
				return Node{}, err
			}
			subtreeUsage, err := s.loadSubtreeUsage(ctx, []Node{*current})
			if err != nil {
				return Node{}, fmt.Errorf("compute subtree usage for move: %w", err)
			}
			moved := subtreeUsage[current.ID].Total(s.resourceIDs)
			if err := s.checkCapacity(ctx, newParentChain, moved, nil); err != nil {
				return Node{}, err
			}
		}
	}

	historyEntry := newHistoryEntry("reparented", actor, current.Status)
	historyEntry.ParentFrom = current.ParentID
	historyEntry.ParentTo = &newParent.ID

	updated := *current
	updated.ParentID = &newParent.ID
	updated.History = append(slices.Clone(current.History), historyEntry)

	if err := s.store.UpsertNode(ctx, updated); err != nil {
		return Node{}, fmt.Errorf("persist node: %w", err)
	}
	return updated, nil
}

// TransferOwner hands a leaf to a new responsible person. Only managers of the
// parent chain may transfer (e.g. when the current owner leaves the organization).
func (s *Service) TransferOwner(id string, req TransferOwnerRequest, actor string, userTokens common.TokenList) (Node, error) {
	ctx, cancel := s.newCtx()
	defer cancel()

	current, err := s.store.GetNode(ctx, id)
	if err != nil {
		return Node{}, fmt.Errorf("load node: %w", err)
	}
	if current == nil {
		return Node{}, fmt.Errorf("node %w", common.ErrNotFound)
	}
	if !current.IsLeaf() {
		return Node{}, fmt.Errorf("only project leaves have an owner")
	}
	if current.Status == StatusImported {
		return Node{}, fmt.Errorf("imported nodes get their owner via promote: %w", common.ErrForbidden)
	}
	if IsTerminalStatus(current.Status) {
		return Node{}, fmt.Errorf("cannot transfer node in status %q", current.Status)
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
		return Node{}, fmt.Errorf("node is already owned by %s", newOwner)
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
func (s *Service) PromoteNode(id string, req PromoteNodeRequest, actor string, userTokens common.TokenList) (Node, error) {
	s.approvalMu.Lock()
	defer s.approvalMu.Unlock()

	ctx, cancel := s.newCtx()
	defer cancel()

	current, err := s.store.GetNode(ctx, id)
	if err != nil {
		return Node{}, fmt.Errorf("load node: %w", err)
	}
	if current == nil {
		return Node{}, fmt.Errorf("node %w", common.ErrNotFound)
	}
	if current.Status != StatusImported {
		return Node{}, fmt.Errorf("only imported nodes can be promoted: %w", common.ErrForbidden)
	}

	newParent, err := s.store.GetNode(ctx, req.NewParentID)
	if err != nil {
		return Node{}, fmt.Errorf("load new parent: %w", err)
	}
	if newParent == nil {
		return Node{}, fmt.Errorf("new parent %w", common.ErrNotFound)
	}
	if newParent.Kind != KindBudget || newParent.Status != StatusApproved || newParent.ID == UnassignedNodeID {
		return Node{}, fmt.Errorf("promotion target must be an approved budget")
	}

	// Both sides: the imported node's chain (unassigned → root) and the target.
	if manages, err := s.managesParentChain(ctx, userTokens, current); err != nil {
		return Node{}, err
	} else if !manages {
		return Node{}, common.ErrForbidden
	}
	if manages, err := s.managesNode(ctx, userTokens, newParent); err != nil {
		return Node{}, err
	} else if !manages {
		return Node{}, common.ErrForbidden
	}

	owner, err := normalizeOwnerToken(req.Owner)
	if err != nil {
		return Node{}, err
	}

	effectiveLimit := current.Limit
	if len(req.Limit) > 0 {
		if err := s.validateLeafLimit(req.Limit); err != nil {
			return Node{}, err
		}
		effectiveLimit = req.Limit
	}

	// Early capacity check against the target chain for a better UX; the final,
	// authoritative check runs again at approval time.
	newParentChain, err := s.nodeChain(ctx, newParent.ID)
	if err != nil {
		return Node{}, err
	}
	if err := s.checkCapacity(ctx, newParentChain, effectiveLimit, nil); err != nil {
		return Node{}, err
	}

	normalizedAuthorizedUsers, err := normalizeAuthorizedUsers(req.AuthorizedUsers)
	if err != nil {
		return Node{}, err
	}

	historyEntry := newHistoryEntry("promote_requested", actor, StatusImported)
	historyEntry.ParentFrom = current.ParentID
	historyEntry.ParentTo = &newParent.ID
	historyEntry.OwnerTo = &owner
	historyEntry.Reason = ptr(req.Reason)

	updated := *current
	updated.ParentID = &newParent.ID
	updated.Owner = owner
	updated.Reason = req.Reason
	updated.TerminationDate = req.TerminationDate
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

// DeleteNode removes a budget subtree. Refused while any node in the subtree is
// still pending, awaiting a change decision, active, or an unpromoted import —
// those must be decided/released first so nothing is silently discarded.
func (s *Service) DeleteNode(id string, actor string, userTokens common.TokenList) error {
	_ = actor
	ctx, cancel := s.newCtx()
	defer cancel()

	current, err := s.store.GetNode(ctx, id)
	if err != nil {
		return fmt.Errorf("load node: %w", err)
	}
	if current == nil {
		return fmt.Errorf("node %w", common.ErrNotFound)
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

// parentChainNodes returns the node's ancestor budgets (parent upward, exclusive).
func (s *Service) parentChainNodes(ctx context.Context, node *Node) ([]Node, error) {
	if node.ParentID == nil {
		return nil, nil
	}
	return s.nodeChain(ctx, *node.ParentID)
}

// normalizePagination clamps incoming pagination values to safe limits.
func normalizePagination(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = common.DefaultPageLimit
	}
	if limit > common.MaxPageLimit {
		limit = common.MaxPageLimit
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}
