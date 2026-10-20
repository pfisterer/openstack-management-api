package tree

import (
	"context"
	"fmt"
	"maps"
	"reflect"
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
		return model.Node{}, common.ErrForbidden
	}
	req.AdminScope = common.CanonicalTokens(req.AdminScope)
	req.EligibleRequesters = common.CanonicalTokens(req.EligibleRequesters)

	// Validate the request shape before taking the approval lock.
	//
	// Every node needs a name, leaves included: it is what the requester sees in
	// the UI and what the reconciler names the OpenStack project after. Without
	// it the purpose had to stand in — a whole sentence truncated to Keystone's
	// name length.
	if strings.TrimSpace(req.Name) == "" {
		return model.Node{}, fmt.Errorf("a name is required")
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
		if req.MaxProjectTermDays != nil {
			return model.Node{}, fmt.Errorf("max_project_term_days is a budget setting")
		}
		if req.ReleaseMax != nil {
			return model.Node{}, fmt.Errorf("release_max is a budget setting")
		}
		if req.InheritsLimit {
			return model.Node{}, fmt.Errorf("inherits_limit is a budget setting")
		}
	case model.KindBudget:
		// A budget without an admin scope is invisible in the UI: "My Budgets"
		// matches AdminScope directly (the ancestor rule does not apply there),
		// so nobody would find it, and every request under it would surface at
		// the nearest ancestor manager instead. The structural "unassigned" node
		// is the deliberate exception and is created by Bootstrap, not here.
		if len(req.AdminScope) == 0 {
			return model.Node{}, fmt.Errorf("budgets require at least one entry in admin_scope (who manages it)")
		}
		if err := s.validateBudgetLimit(req.Limit); err != nil {
			return model.Node{}, err
		}
		if err := s.validateAutoApprove(req.AutoApprove); err != nil {
			return model.Node{}, err
		}
	default:
		return model.Node{}, fmt.Errorf("invalid kind %q", req.Kind)
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
		return model.Node{}, fmt.Errorf("parent node %w", common.ErrNotFound)
	}
	if parent.Kind != model.KindBudget {
		return model.Node{}, fmt.Errorf("parent must be a budget")
	}
	if parent.Status != model.StatusApproved {
		return model.Node{}, fmt.Errorf("%w: cannot create nodes under a budget in status %q", common.ErrConflict, parent.Status)
	}

	isManager, err := s.managesNode(ctx, userTokens, parent)
	if err != nil {
		return model.Node{}, err
	}
	if !isManager && !isEligibleRequester(userTokens, parent) {
		return model.Node{}, common.ErrForbidden
	}
	parentChain, err := s.nodeChain(ctx, parent.ID)
	if err != nil {
		return model.Node{}, err
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
		return model.Node{}, fmt.Errorf("%w: this budget does not accept sub-budget requests — request a project instead", common.ErrForbidden)
	}
	// Passing on the parent's limit hands out all of it, so only those who
	// could give that limit directly may.
	if req.InheritsLimit {
		if !isManager {
			return model.Node{}, fmt.Errorf("%w: only a manager creates a budget that inherits its limit", common.ErrForbidden)
		}
		req.Limit = common.ProjectQuota{}
	}

	// Nothing outlives the budget it draws from, and a project runs no longer
	// than the budgets above allow: no end asked means the latest allowed, a
	// later one is refused. A sub-budget inherits the term cap the same way.
	var terminationDate *string
	var maxTerm *int
	if req.Kind == model.KindProject {
		terminationDate, err = fitProjectEnd(req.TerminationDate, parentChain, time.Now())
	} else {
		terminationDate, err = fitEnd(req.TerminationDate, chainEnd(parentChain))
		if err == nil {
			maxTerm, err = fitMaxTerm(req.MaxProjectTermDays, chainMaxTerm(parentChain))
		}
		if err == nil {
			// Unlike the term, nothing is copied down: what the budgets above set applies anyway (strictest along the chain), and copying would freeze it here.
			err = model.ValidateReleaseMax(req.ReleaseMax, model.ResolveReleaseMax(parentChain))
		}
	}
	if err != nil {
		return model.Node{}, err
	}

	now := time.Now().UTC().Format(time.RFC3339)
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
		node.ReleaseMax = req.ReleaseMax.Normalized()
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
		approvedEntry.Reason = new("Created directly by a manager")
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

	case req.Kind == model.KindBudget && parent.AutoApprove != nil:
		// As with projects: a request for nothing at all waits for a manager.
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
		requester, err := normalizeOwnerToken(userEmail)
		if err != nil {
			return model.Node{}, err
		}
		ok, err := s.subBudgetAutoApprovable(ctx, parent, parentChain, node, requester)
		if err != nil {
			return model.Node{}, err
		}
		if ok {
			node.Status = model.StatusApproved
			node.History = append(node.History, autoApprovedEntry(actor, model.StatusPending, autoApproveReason(parent)))
		} else if !parent.RequestsBeyondAutoApproveAllowed() {
			return model.Node{}, errBeyondAutoApprove
		}
	}

	if err := s.store.UpsertNode(ctx, node); err != nil {
		return model.Node{}, fmt.Errorf("persist node: %w", err)
	}
	return node, nil
}

// normalizeCreateMembers checks the members of a new project for CreateNode, with the parent's managers free to add any group.
func (s *Service) normalizeCreateMembers(ctx context.Context, req CreateNodeRequest, userTokens common.TokenList) ([]common.AuthorizedUser, error) {
	if len(req.AuthorizedUsers) == 0 {
		return s.normalizeAuthorizedUsers(ctx, req.AuthorizedUsers, userTokens, false)
	}
	manager := false
	if parent, err := s.store.GetNode(ctx, req.ParentID); err != nil {
		return nil, fmt.Errorf("load parent node: %w", err)
	} else if parent != nil {
		if manager, err = s.managesNode(ctx, userTokens, parent); err != nil {
			return nil, err
		}
	}
	return s.normalizeAuthorizedUsers(ctx, req.AuthorizedUsers, userTokens, manager)
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
		return fmt.Errorf("%w: you already have %d projects in budget %q, at most %d are allowed — release one you no longer need or ask one of its managers", common.ErrForbidden, n, nodeLabel(*budget), s.maxProjectsPerOwner)
	}
	return nil
}

// liveProjectStatuses are the statuses a project counts in against checkProjectsPerOwner.
var liveProjectStatuses = []string{model.StatusPending, model.StatusApproved, model.StatusChangePending, model.StatusReleased}

// isLeafDirectEdit reports whether req touches nothing but the name and the
// admin scope — the direct edits a project leaf accepts. Neither costs anything,
// so neither goes through the approval cycle.
func isLeafDirectEdit(req UpdateNodeRequest) bool {
	return (req.Name != nil || req.AdminScope != nil) &&
		req.EligibleRequesters == nil &&
		req.AutoApprove == nil && !req.ClearAutoApprove &&
		req.AllowSubBudgetRequests == nil && req.AllowRequestsBeyondAutoApprove == nil &&
		req.AutoApproveExtensions == nil && req.MaxProjectTermDays == nil && !req.ClearMaxProjectTermDays &&
		req.ReleaseMax == nil &&
		req.Limit == nil && req.TerminationDate == nil && !req.ClearTerminationDate &&
		req.InheritsLimit == nil
}

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
	if current.Kind != model.KindBudget && !isLeafDirectEdit(req) {
		return model.Node{}, fmt.Errorf("only the name and the admins can be edited directly on a project; use request-change for anything else")
	}

	wantsPolicyEdit := req.Name != nil || req.AdminScope != nil || req.EligibleRequesters != nil ||
		req.AutoApprove != nil || req.ClearAutoApprove || req.AllowSubBudgetRequests != nil ||
		req.AllowRequestsBeyondAutoApprove != nil || req.AutoApproveExtensions != nil ||
		req.MaxProjectTermDays != nil || req.ClearMaxProjectTermDays || req.ReleaseMax != nil
	wantsCapacityEdit := req.Limit != nil || req.TerminationDate != nil || req.ClearTerminationDate || req.InheritsLimit != nil

	if wantsPolicyEdit {
		// Renaming their own project and choosing who administers it with them
		// is the owner's business; a project's admins pass managesNode through
		// its own admin scope. Every edit on a budget belongs to a manager.
		allowed := current.IsLeaf() && isOwner(userTokens, current)
		if !allowed {
			manages, err := s.managesNode(ctx, userTokens, current)
			if err != nil {
				return model.Node{}, err
			}
			allowed = manages
		}
		if !allowed {
			return model.Node{}, common.ErrForbidden
		}
	}
	if wantsCapacityEdit {
		if manages, err := s.managesParentChain(ctx, userTokens, current); err != nil {
			return model.Node{}, err
		} else if !manages {
			return model.Node{}, common.ErrForbidden
		}
	}

	// The root node's admin scope is owned by the configuration (synced on every
	// startup); an API edit would silently revert on the next deployment.
	if current.ID == model.RootNodeID && req.AdminScope != nil {
		return model.Node{}, fmt.Errorf("the root admin scope is managed via ROOT_ADMIN_TOKENS: %w", common.ErrForbidden)
	}

	updated := *current
	if req.Name != nil {
		if strings.TrimSpace(*req.Name) == "" {
			return model.Node{}, fmt.Errorf("a name is required")
		}
		if err := validateName(strings.TrimSpace(*req.Name)); err != nil {
			return model.Node{}, err
		}
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
	var chain []model.Node
	if wantsCapacityEdit || req.ClearMaxProjectTermDays || req.MaxProjectTermDays != nil || req.ReleaseMax != nil {
		if chain, err = s.parentChainNodes(ctx, current); err != nil {
			return model.Node{}, err
		}
	}
	if req.ClearMaxProjectTermDays {
		if bound := chainMaxTerm(chain); bound != nil {
			return model.Node{}, fmt.Errorf("projects can run at most %d days in the budget above, so this one needs that limit too", *bound)
		}
		updated.MaxProjectTermDays = nil
	} else if req.MaxProjectTermDays != nil {
		if _, err := fitMaxTerm(req.MaxProjectTermDays, chainMaxTerm(chain)); err != nil {
			return model.Node{}, err
		}
		updated.MaxProjectTermDays = req.MaxProjectTermDays
	}
	// A budget's release maximums only tighten what the budgets above allow. Lowering them needs nothing carried down: what applies below is the strictest along the chain, so a budget below with longer times of its own simply gets these.
	if req.ReleaseMax != nil {
		if err := model.ValidateReleaseMax(req.ReleaseMax, model.ResolveReleaseMax(chain)); err != nil {
			return model.Node{}, err
		}
		updated.ReleaseMax = req.ReleaseMax.Normalized()
	}
	if req.ClearTerminationDate || req.TerminationDate != nil {
		if bound := chainEnd(chain); req.ClearTerminationDate && bound != nil {
			return model.Node{}, fmt.Errorf("the budget above ends on %s, so this one needs an end date too", dateOnly(*bound))
		}
		if req.TerminationDate != nil {
			if _, err := fitEnd(req.TerminationDate, chainEnd(chain)); err != nil {
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
	if req.ReleaseMax != nil && !reflect.DeepEqual(current.ReleaseMax, updated.ReleaseMax) {
		historyEntry.ReleaseMaxFrom = current.ReleaseMax.Clone()
		historyEntry.ReleaseMaxTo = updated.ReleaseMax.Clone()
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
		return model.Node{}, fmt.Errorf("the limit of this budget is inherited from the budget above; switch inheriting off to set one")
	}
	// Only where the chain was loaded: an edit of access or policy alone does
	// not load it, and an inheriting budget then looked like the root.
	if inherits && wantsCapacityEdit && binding == nil {
		return model.Node{}, fmt.Errorf("the root budget has nothing to inherit from")
	}
	if inherits && !current.InheritsLimit {
		historyEntry.LimitFrom = &current.Limit
		historyEntry.Reason = new("Inherits the limit of the budget above")
		updated.Limit = common.ProjectQuota{}
	}
	if !inherits && current.InheritsLimit && req.Limit == nil && binding != nil {
		req.Limit = new(maps.Clone(binding.Limit))
	}
	updated.InheritsLimit = inherits

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
		if _, err := s.shortenSubtreeTerms(ctx, []string{updated.ID}, days, time.Now(), actor, termReason(updated, days)); err != nil {
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
	allowed := false
	if current.IsLeaf() {
		allowed = isOwner(userTokens, current)
	}
	if !allowed {
		if manages, err := s.managesNode(ctx, userTokens, current); err != nil {
			return model.Node{}, err
		} else {
			allowed = manages
		}
	}
	if !allowed {
		return model.Node{}, common.ErrForbidden
	}

	// The budgets above: they bound a new limit and end, and the policy of
	// the one a leaf draws from decides whether the change needs anybody's
	// approval.
	chain, err := s.parentChainNodes(ctx, current)
	if err != nil {
		return model.Node{}, err
	}
	var parent *model.Node
	if current.IsLeaf() && len(chain) > 0 {
		parent = &chain[0]
	}

	if req.TerminationDate != nil {
		if current.IsLeaf() {
			_, err = fitProjectEnd(req.TerminationDate, chain, time.Now())
		} else {
			_, err = fitEnd(req.TerminationDate, chainEnd(chain))
		}
		if err != nil {
			return model.Node{}, err
		}
	}

	if req.Limit != nil && current.InheritsLimit {
		return model.Node{}, fmt.Errorf("the limit of this budget is inherited from the budget above")
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

	var normalizedAuthorizedUsers *[]common.AuthorizedUser
	if req.AuthorizedUsers != nil {
		if !current.IsLeaf() {
			return model.Node{}, fmt.Errorf("authorized_users can only be changed on project leaves")
		}
		manager, err := s.managesParentChain(ctx, userTokens, current)
		if err != nil {
			return model.Node{}, err
		}
		keep := [][]common.AuthorizedUser{current.AuthorizedUsers}
		if current.Pending != nil && current.Pending.AuthorizedUsers != nil {
			keep = append(keep, *current.Pending.AuthorizedUsers)
		}
		normalized, err := s.normalizeAuthorizedUsers(ctx, *req.AuthorizedUsers, userTokens, manager, keep...)
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
		return model.Node{}, fmt.Errorf("change request contains no changes")
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
		manages, err := s.managesParentChain(ctx, userTokens, current)
		if err != nil {
			return model.Node{}, err
		}
		if !manages {
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
var errBeyondAutoApprove = fmt.Errorf("%w: this is more than the budget grants automatically, and it takes no requests beyond that — give something back or ask one of its managers to raise your share", common.ErrForbidden)

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
	return true, "Applied directly (gives resources back, ends sooner or changes members only)", false, nil
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
		return model.Node{}, fmt.Errorf("%w: cannot approve node in status %q", common.ErrConflict, current.Status)
	}

	if manages, err := s.managesParentChain(ctx, userTokens, current); err != nil {
		return model.Node{}, err
	} else if !manages {
		return model.Node{}, common.ErrForbidden
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

	ancestors, err := s.parentChainNodes(ctx, current)
	if err != nil {
		return model.Node{}, err
	}
	// Requests made before the budget above got (or moved up) its end still
	// ask for more: they are granted to the budget's end, not refused whole.
	// The same for a project's maximum term, counted from today.
	bound := chainEnd(ancestors)
	if current.IsLeaf() {
		bound, _ = projectBound(ancestors, time.Now())
	}
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
			return fmt.Errorf("%w: cannot approve node in status %q", common.ErrConflict, n.Status)
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
		return model.Node{}, fmt.Errorf("node %w", common.ErrNotFound)
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
		return fmt.Errorf("expected_revision is required: the revision of the request as it was shown")
	}
	if got := n.CurrentRevision(); got != *expected {
		return fmt.Errorf("%w: the request changed since it was shown (revision %d, now %d) — reload it and decide again", common.ErrConflict, *expected, got)
	}
	return nil
}

// sameEnd reports whether two optional end dates are the same.
func sameEnd(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

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
		return model.Node{}, fmt.Errorf("%w: cannot reject node in status %q", common.ErrConflict, current.Status)
	}

	if manages, err := s.managesParentChain(ctx, userTokens, current); err != nil {
		return model.Node{}, err
	} else if !manages {
		return model.Node{}, common.ErrForbidden
	}

	var updated model.Node
	if _, err := s.store.UpdateNode(ctx, id, func(n *model.Node) error {
		if n.Status != current.Status || derefParent(n.ParentID) != derefParent(current.ParentID) {
			return fmt.Errorf("%w: the node changed meanwhile (status %q); reload", common.ErrConflict, n.Status)
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
		return model.Node{}, fmt.Errorf("node %w", common.ErrNotFound)
	}
	return updated, nil
}
