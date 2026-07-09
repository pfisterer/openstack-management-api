package applogic

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/webserver"
)

// newHistoryEntry creates a HistoryEntry with the current timestamp and the
// required fields filled in. Callers set optional fields (StatusFrom, QuotaFrom, etc.) after construction.
func newHistoryEntry(event, actor, statusTo string) common.HistoryEntry {
	return common.HistoryEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Event:     event,
		Actor:     actor,
		StatusTo:  statusTo,
	}
}

// GetProjectByID fetches a single project by ID, enforcing read authorization.
//
// Access is granted if any of the caller's tokens matches either:
//   - A token in the project's RequesterTokens (the requester always sees their own project), or
//   - A token in the AdminScope of the funding delegation or any of its ancestors
//     (managers up the tree can inspect projects consuming capacity they oversee).
//
// Unfunded projects (funded_by is nil, status still pending) are only visible to the requester.
func (s *Service) GetProjectByID(id string, userTokens common.TokenList) (*common.Project, error) {
	if len(userTokens) == 0 {
		return nil, common.ErrForbidden
	}

	ctx, cancel := s.newCtx()
	defer cancel()

	req, err := s.store.GetProjectByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("load project: %w", err)
	}
	if req == nil {
		return nil, nil
	}

	callerSet := common.NewTokenSet(userTokens)

	// Requester always has access.
	if callerSet.ContainsAny(req.RequesterTokens) {
		return req, nil
	}

	// Root admins can access any openstack_only record (they have no requester tokens or funder).
	if req.Status == common.ProjectStatusOpenStackOnly && s.rootAdminTokens.ContainsAny(userTokens) {
		return req, nil
	}

	// For funded projects, walk the delegation ancestor chain and check scope membership.
	if req.FundedBy != nil {
		delegID := req.FundedBy
		for delegID != nil {
			deleg, err := s.store.GetDelegationByID(ctx, *delegID)
			if err != nil {
				return nil, fmt.Errorf("load delegation for auth check: %w", err)
			}
			if deleg == nil {
				break
			}
			if callerSet.ContainsAny(deleg.AdminScope) {
				return req, nil
			}
			delegID = deleg.ParentID
		}
	}

	return nil, common.ErrForbidden
}

// ListProjectsBy returns projects created by the user/group (matching their tokens).
// openstack_only records are excluded — they are synthetic reconciler imports with no
// meaningful requester association and are only visible to root admins via
// ListProjectsManagedBy.
func (s *Service) ListProjectsBy(userEmail string, limit, offset int) ([]common.Project, error) {
	if userEmail == "" {
		return nil, fmt.Errorf("missing user email")
	}

	limit, offset = normalizePagination(limit, offset)

	ctx, cancel := s.newCtx()
	defer cancel()
	projects, err := s.store.ListProjectsBy(ctx, userEmail, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list projects by requester tokens: %w", err)
	}

	result := projects[:0]
	for _, p := range projects {
		if p.Status != common.ProjectStatusOpenStackOnly {
			result = append(result, p)
		}
	}
	return result, nil
}

// ListProjectsManagedBy returns pending/change_pending projects funded by delegations
// the caller administers (i.e. delegations where their tokens are in admin_scope).
func (s *Service) ListProjectsManagedBy(userEmail string, userTokens common.TokenList, limit, offset int) ([]common.Project, error) {
	if userEmail == "" {
		return nil, fmt.Errorf("missing user email")
	}
	if len(userTokens) == 0 {
		return nil, fmt.Errorf("no user tokens found")
	}

	limit, offset = normalizePagination(limit, offset)
	ctx, cancel := s.newCtx()
	defer cancel()

	delegations, err := s.store.GetDelegationsByAdminScope(ctx, userTokens, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("load admin delegations: %w", err)
	}
	if len(delegations) == 0 {
		return []common.Project{}, nil
	}

	ids := make([]string, len(delegations))
	for i, d := range delegations {
		ids[i] = d.ID
	}

	projects, err := s.store.GetProjectsByFundedByIDs(ctx, ids, common.ManagedProjectStatuses, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("load manageable projects: %w", err)
	}

	// Root admins additionally see all openstack_only records (unmanaged OS projects with no
	// funding delegation). These are imported by the reconciler and require root-level attention.
	if s.rootAdminTokens.ContainsAny(userTokens) {
		osOnly, err := s.store.ListProjectsByStatus(ctx, []string{common.ProjectStatusOpenStackOnly}, 0, 0)
		if err != nil {
			return nil, fmt.Errorf("load openstack_only for root admin: %w", err)
		}
		projects = append(projects, osOnly...)
	}

	return projects, nil
}

// CreateProject validates and stores a new project, with optional auto-approval.
func (s *Service) CreateProject(req webserver.CreateProjectRequest, actor string, userEmail string, userTokens common.TokenList) (common.Project, error) {
	if userEmail == "" {
		return common.Project{}, fmt.Errorf("no current user")
	}
	if len(userTokens) == 0 {
		return common.Project{}, fmt.Errorf("no current user tokens")
	}

	normalizedAuthorizedUsers, err := normalizeAuthorizedUsers(req.AuthorizedUsers)
	if err != nil {
		return common.Project{}, err
	}

	ctx, cancel := s.newCtx()
	defer cancel()

	eligibleDelegations, err := s.ListDelegationsEligibleForMe(userTokens, 0, 0)
	if err != nil {
		return common.Project{}, fmt.Errorf("load eligible delegations: %w", err)
	}
	var fundingDelegation *common.Delegation
	for i := range eligibleDelegations {
		if eligibleDelegations[i].ID == req.FundingDelegationID {
			fundingDelegation = &eligibleDelegations[i]
			break
		}
	}
	if fundingDelegation == nil {
		return common.Project{}, fmt.Errorf("funding delegation %q not found or not eligible for this user", req.FundingDelegationID)
	}

	createdEntry := newHistoryEntry("created", actor, common.ProjectStatusPending)
	createdEntry.QuotaTo = &req.Quota
	createdEntry.TerminationDate = &req.TerminationDate

	newReq := common.Project{
		ID:              "req_" + uuid.New().String(),
		Status:          common.ProjectStatusPending,
		RequesterTokens: append(common.TokenList{}, userTokens...),
		Quota:           req.Quota,
		Reason:          req.Reason,
		FundedBy:        &fundingDelegation.ID,
		Pending:         nil,
		TerminationDate: req.TerminationDate,
		AuthorizedUsers: normalizedAuthorizedUsers,
		History:         []common.HistoryEntry{createdEntry},
	}

	// Auto-approval: only if the chosen delegation is an allowance type whose per-user cap
	// covers the project and all ancestor pools have sufficient remaining capacity.
	if fundingDelegation.DelegationStrategy == common.DelegationStrategyAllowance &&
		quotaFits(req.Quota, fundingDelegation.Quota.Limit, s.quotaResourceIDs) {

		poolAncestors, err := s.collectPoolAncestors(ctx, fundingDelegation)
		if err != nil {
			return common.Project{}, fmt.Errorf("collect pool ancestors for auto-approval check: %w", err)
		}
		if err := s.checkPoolCapacity(ctx, poolAncestors, req.Quota, nil); err == nil {
			newReq.Status = common.ProjectStatusApproved
			autoApprovalEntry := newHistoryEntry("approved", "system:auto-approval", common.ProjectStatusApproved)
			autoApprovalEntry.Group = &fundingDelegation.ID
			autoApprovalEntry.StatusFrom = ptr(common.ProjectStatusPending)
			autoApprovalEntry.Reason = ptr("Auto-approved (per-user allowance)")
			newReq.History = append(newReq.History, autoApprovalEntry)
		}
	}

	if err := s.store.UpsertProject(ctx, newReq); err != nil {
		return common.Project{}, fmt.Errorf("persist project state: %w", err)
	}
	return newReq, nil
}

// UpdateProject records requested changes and transitions the project to change_pending.
func (s *Service) UpdateProject(id string, req webserver.UpdateProjectRequest, actor string, userTokens common.TokenList) (common.Project, error) {
	ctx, cancel := s.newCtx()
	defer cancel()

	current, err := s.store.GetProjectByID(ctx, id)
	if err != nil {
		return common.Project{}, fmt.Errorf("load project: %w", err)
	}
	if current == nil {
		return common.Project{}, fmt.Errorf("project %w", common.ErrNotFound)
	}
	if current.Status == common.ProjectStatusOpenStackOnly {
		return common.Project{}, fmt.Errorf("openstack_only resources are read-only and cannot be modified: %w", common.ErrForbidden)
	}

	// Authorization: only the project's requester (or a root admin) may propose changes.
	if !isProjectRequester(userTokens, current) && !s.rootAdminTokens.ContainsAny(userTokens) {
		return common.Project{}, common.ErrForbidden
	}

	quotaTo := current.Quota
	if req.Quota != nil {
		quotaTo = *req.Quota
	}

	pending := &common.PendingChanges{Quota: &quotaTo}

	if req.TerminationDate != nil && *req.TerminationDate != current.TerminationDate {
		pending.TerminationDate = req.TerminationDate
	}

	if req.AuthorizedUsers != nil {
		normalized, err := normalizeAuthorizedUsers(*req.AuthorizedUsers)
		if err != nil {
			return common.Project{}, err
		}
		pending.AuthorizedUsers = &normalized
	}

	historyEntry := newHistoryEntry("change_requested", actor, common.ProjectStatusChangePending)
	historyEntry.StatusFrom = &current.Status
	historyEntry.QuotaFrom = &current.Quota
	historyEntry.QuotaTo = &quotaTo
	if pending.TerminationDate != nil {
		historyEntry.TerminationDateFrom = &current.TerminationDate
		historyEntry.TerminationDateTo = pending.TerminationDate
	}

	updated := *current
	updated.Pending = pending
	updated.Status = common.ProjectStatusChangePending
	updated.History = append(slices.Clone(current.History), historyEntry)

	if err := s.store.UpsertProject(ctx, updated); err != nil {
		return common.Project{}, fmt.Errorf("persist project state: %w", err)
	}
	return updated, nil
}

// ApproveProject finalizes a project from eligible delegation context and pending changes.
func (s *Service) ApproveProject(id string, req webserver.ApproveProjectRequest, actor string, userEmail string, userTokens common.TokenList) (common.Project, error) {
	if userEmail == "" || len(userTokens) == 0 {
		return common.Project{}, common.ErrForbidden
	}

	ctx, cancel := s.newCtx()
	defer cancel()

	current, err := s.store.GetProjectByID(ctx, id)
	if err != nil {
		return common.Project{}, fmt.Errorf("load project: %w", err)
	}
	if current == nil {
		return common.Project{}, fmt.Errorf("project %w", common.ErrNotFound)
	}

	if current.Status == common.ProjectStatusOpenStackOnly {
		return common.Project{}, fmt.Errorf("openstack_only resources are read-only and cannot be approved: %w", common.ErrForbidden)
	}

	group, err := s.store.GetDelegationByID(ctx, req.DelegationID)
	if err != nil {
		return common.Project{}, fmt.Errorf("load delegation: %w", err)
	}
	if group == nil {
		return common.Project{}, fmt.Errorf("delegation %w", common.ErrNotFound)
	}

	if !common.NewTokenSet(userTokens).ContainsAny(group.AdminScope) {
		return common.Project{}, common.ErrForbidden
	}

	finalQuota := current.Quota
	if req.ModifiedQuota != nil {
		finalQuota = *req.ModifiedQuota
	} else if current.Pending != nil && current.Pending.Quota != nil {
		finalQuota = *current.Pending.Quota
	}

	finalTerminationDate := current.TerminationDate
	if current.Pending != nil && current.Pending.TerminationDate != nil {
		finalTerminationDate = *current.Pending.TerminationDate
	}

	finalAuthorizedUsers := append([]common.AuthorizedUser{}, current.AuthorizedUsers...)
	if current.Pending != nil && current.Pending.AuthorizedUsers != nil {
		finalAuthorizedUsers = append(finalAuthorizedUsers[:0], (*current.Pending.AuthorizedUsers)...)
	}
	normalizedFinalAuthorizedUsers, err := normalizeAuthorizedUsers(finalAuthorizedUsers)
	if err != nil {
		return common.Project{}, err
	}
	finalAuthorizedUsers = normalizedFinalAuthorizedUsers

	historyEntry := newHistoryEntry("approved", actor, common.ProjectStatusApproved)
	historyEntry.StatusFrom = &current.Status
	historyEntry.Group = &req.DelegationID
	if req.ModifiedQuota != nil || (current.Pending != nil && current.Pending.Quota != nil) {
		historyEntry.QuotaFrom = &current.Quota
		historyEntry.QuotaTo = &finalQuota
	}
	if current.Pending != nil && current.Pending.TerminationDate != nil && *current.Pending.TerminationDate != current.TerminationDate {
		historyEntry.TerminationDateFrom = &current.TerminationDate
		historyEntry.TerminationDateTo = &finalTerminationDate
	}
	if current.Pending != nil && current.Pending.AuthorizedUsers != nil {
		historyEntry.Reason = ptr("Approved pending authorization changes")
	}

	// Enforce capacity constraints across the full ancestor pool chain.
	// For change_pending projects, subtract the project's currently committed resources
	// before adding finalQuota — they are already in the active set and must not be
	// double-counted.
	poolAncestors, err := s.collectPoolAncestors(ctx, group)
	if err != nil {
		return common.Project{}, fmt.Errorf("collect pool ancestors for capacity check: %w", err)
	}
	var subtractQuota common.ProjectQuota
	if current.Status == common.ProjectStatusChangePending {
		subtractQuota = current.Quota
	}
	if err := s.checkPoolCapacity(ctx, poolAncestors, finalQuota, subtractQuota); err != nil {
		return common.Project{}, err
	}

	updated := *current
	updated.Status = common.ProjectStatusApproved
	updated.FundedBy = &req.DelegationID
	updated.Quota = finalQuota
	updated.TerminationDate = finalTerminationDate
	updated.AuthorizedUsers = finalAuthorizedUsers
	updated.Pending = nil
	updated.History = append(slices.Clone(current.History), historyEntry)

	if err := s.store.UpsertProject(ctx, updated); err != nil {
		return common.Project{}, fmt.Errorf("persist project state: %w", err)
	}
	return updated, nil
}

// RejectProject transitions a project into a rejected state and appends audit history.
func (s *Service) RejectProject(id string, req webserver.RejectProjectRequest, actor string, userTokens common.TokenList) (common.Project, error) {
	ctx, cancel := s.newCtx()
	defer cancel()

	current, err := s.store.GetProjectByID(ctx, id)
	if err != nil {
		return common.Project{}, fmt.Errorf("load project: %w", err)
	}
	if current == nil {
		return common.Project{}, fmt.Errorf("project %w", common.ErrNotFound)
	}
	if current.Status == common.ProjectStatusOpenStackOnly {
		return common.Project{}, fmt.Errorf("openstack_only resources are read-only and cannot be rejected: %w", common.ErrForbidden)
	}

	// Authorization: only a manager of the funding delegation (or root) may reject.
	if allowed, err := s.canManageProjectFunding(ctx, userTokens, current); err != nil {
		return common.Project{}, err
	} else if !allowed {
		return common.Project{}, common.ErrForbidden
	}

	statusTo := common.ProjectStatusRejected
	if current.Status == common.ProjectStatusChangePending {
		statusTo = common.ProjectStatusChangeRejected
	}

	history := newHistoryEntry("rejected", actor, statusTo)
	history.StatusFrom = &current.Status
	if req.Reason != nil && strings.TrimSpace(*req.Reason) != "" {
		history.Reason = req.Reason
	}

	updated := *current
	updated.Status = statusTo
	updated.Pending = nil
	updated.History = append(slices.Clone(current.History), history)

	if err := s.store.UpsertProject(ctx, updated); err != nil {
		return common.Project{}, fmt.Errorf("persist project state: %w", err)
	}
	return updated, nil
}

// MarkProjectForPromotion marks an openstack_only project to be promoted to a managed
// project on the next reconciler run. Only root admins may call this.
//
// The caller supplies the owner's tokens (req.OwnerTokens) so the service can resolve
// which delegations the owner is eligible to fund projects from. The selected funding
// delegation must be in that set and must have sufficient remaining capacity for the
// project's current quota. The reconciler will tag the existing OpenStack project,
// transition the record to "pending", and remove the flag, after which the normal
// pending → approved flow takes over.
func (s *Service) MarkProjectForPromotion(id string, req webserver.PromoteProjectRequest, actor string, userTokens common.TokenList) (common.Project, error) {
	if !s.rootAdminTokens.ContainsAny(userTokens) {
		return common.Project{}, fmt.Errorf("only root admins may promote openstack_only projects: %w", common.ErrForbidden)
	}

	ctx, cancel := s.newCtx()
	defer cancel()

	current, err := s.store.GetProjectByID(ctx, id)
	if err != nil {
		return common.Project{}, fmt.Errorf("load project: %w", err)
	}
	if current == nil {
		return common.Project{}, fmt.Errorf("project %w", common.ErrNotFound)
	}
	if current.Status != common.ProjectStatusOpenStackOnly {
		return common.Project{}, fmt.Errorf("only openstack_only projects can be promoted: %w", common.ErrForbidden)
	}

	// Resolve eligible delegations for the project owner, not the calling root admin.
	eligibleDelegations, err := s.ListDelegationsEligibleForMe(req.OwnerTokens, 0, 0)
	if err != nil {
		return common.Project{}, fmt.Errorf("load eligible delegations for owner: %w", err)
	}
	var fundingDelegation *common.Delegation
	for i := range eligibleDelegations {
		if eligibleDelegations[i].ID == req.FundingDelegationID {
			fundingDelegation = &eligibleDelegations[i]
			break
		}
	}
	if fundingDelegation == nil {
		return common.Project{}, fmt.Errorf("funding delegation %q is not in the owner's eligible delegations", req.FundingDelegationID)
	}

	// Use the caller-supplied quota if provided, otherwise keep the project's existing quota.
	effectiveQuota := current.Quota
	if len(req.Quota) > 0 {
		effectiveQuota = req.Quota
	}

	// Verify the selected delegation (and all pool ancestors) have room for the effective quota.
	poolAncestors, err := s.collectPoolAncestors(ctx, fundingDelegation)
	if err != nil {
		return common.Project{}, fmt.Errorf("collect pool ancestors for capacity check: %w", err)
	}
	if err := s.checkPoolCapacity(ctx, poolAncestors, effectiveQuota, nil); err != nil {
		return common.Project{}, err
	}

	normalizedAuthorizedUsers, err := normalizeAuthorizedUsers(req.AuthorizedUsers)
	if err != nil {
		return common.Project{}, err
	}

	historyEntry := newHistoryEntry("promote_requested", actor, common.ProjectStatusOpenStackOnly)
	historyEntry.Reason = ptr(req.Reason)

	updated := *current
	updated.FundedBy = &fundingDelegation.ID
	updated.Reason = req.Reason
	updated.TerminationDate = req.TerminationDate
	updated.Quota = effectiveQuota
	updated.RequesterTokens = append(common.TokenList{}, req.OwnerTokens...)
	updated.AuthorizedUsers = normalizedAuthorizedUsers
	updated.Flags = append(slices.Clone(current.Flags), common.ProjectFlagPromoteOnReconcile)
	updated.History = append(slices.Clone(current.History), historyEntry)

	if err := s.store.UpsertProject(ctx, updated); err != nil {
		return common.Project{}, fmt.Errorf("persist project state: %w", err)
	}
	return updated, nil
}

// ReleaseProject marks approved projects as released to return allocated capacity.
func (s *Service) ReleaseProject(id string, actor string, userTokens common.TokenList) (common.Project, error) {
	ctx, cancel := s.newCtx()
	defer cancel()

	current, err := s.store.GetProjectByID(ctx, id)
	if err != nil {
		return common.Project{}, fmt.Errorf("load project: %w", err)
	}
	if current == nil {
		return common.Project{}, fmt.Errorf("project %w", common.ErrNotFound)
	}
	if current.Status == common.ProjectStatusOpenStackOnly {
		return common.Project{}, fmt.Errorf("openstack_only resources are read-only and cannot be released: %w", common.ErrForbidden)
	}

	// Authorization: the requester or a manager of the funding delegation (or root)
	// may release — releasing drives OpenStack deprovisioning on the next reconcile.
	managed, err := s.canManageProjectFunding(ctx, userTokens, current)
	if err != nil {
		return common.Project{}, err
	}
	if !managed && !isProjectRequester(userTokens, current) {
		return common.Project{}, common.ErrForbidden
	}

	if current.Status != common.ProjectStatusApproved {
		return common.Project{}, fmt.Errorf("cannot release")
	}

	history := newHistoryEntry("released", actor, common.ProjectStatusReleased)
	history.StatusFrom = ptr(common.ProjectStatusApproved)
	history.QuotaFrom = &current.Quota

	updated := *current
	updated.Status = common.ProjectStatusReleased
	updated.History = append(slices.Clone(current.History), history)

	if err := s.store.UpsertProject(ctx, updated); err != nil {
		return common.Project{}, fmt.Errorf("persist project state: %w", err)
	}
	return updated, nil
}
