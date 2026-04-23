package applogic

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/webserver"
)

// GetDelegationsDelegatedToMe returns delegations where the caller is in the admin_scope.
// Used for the "delegations delegated to me" view.
func (s *Service) GetDelegationsDelegatedToMe(userTokens common.TokenList, limit, offset int) ([]common.Delegation, error) {
	if len(userTokens) == 0 {
		return nil, fmt.Errorf("no user tokens found")
	}

	ctx, cancel := s.newCtx()
	defer cancel()
	limit, offset = normalizePagination(limit, offset)

	delegations, err := s.store.GetDelegationsByAdminScope(ctx, userTokens, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("load delegations by admin scope: %w", err)
	}

	return s.attachUsage(ctx, delegations)
}

// GetDelegationsByAdminScope returns delegations whose parent_id matches one of the caller's
// group tokens. Used for the "delegations I've made" view.
func (s *Service) GetDelegationsByAdminScope(userTokens common.TokenList, limit, offset int) ([]common.Delegation, error) {
	if len(userTokens) == 0 {
		return nil, fmt.Errorf("no user tokens found")
	}

	ctx, cancel := s.newCtx()
	defer cancel()
	limit, offset = normalizePagination(limit, offset)

	delegations, err := s.store.GetDelegationsByParentTokens(ctx, userTokens, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("load delegations by parent tokens: %w", err)
	}

	return s.attachUsage(ctx, delegations)
}

// ListDelegationsEligibleForMe returns all delegations the caller may submit requests to.
//
// Two sources are combined:
//  1. Pool delegations whose admin_scope contains an owner_token from an eligibility rule
//     that lists any of the caller's tokens in eligible_requesters.
//  2. Allowance delegations where the caller is directly in the admin_scope
//     (these are auto-approved on create).
func (s *Service) ListDelegationsEligibleForMe(userTokens common.TokenList, limit, offset int) ([]common.Delegation, error) {
	if len(userTokens) == 0 {
		return nil, fmt.Errorf("no user tokens found")
	}
	limit, offset = normalizePagination(limit, offset)
	ctx, cancel := s.newCtx()
	defer cancel()

	// Collect owner tokens from eligibility rules (delegations the user may request from via rules).
	rules, err := s.store.GetEligibilityRulesByRequesterTokens(ctx, userTokens)
	if err != nil {
		return nil, fmt.Errorf("load eligibility rules: %w", err)
	}
	ownerTokens := make(common.TokenList, 0, len(rules))
	for _, r := range rules {
		ownerTokens = append(ownerTokens, r.OwnerToken)
	}

	// Single store call: fetch all delegations reachable via eligibility-rule owner tokens OR
	// directly by the user's own tokens (for allowance delegations).
	combined := append(ownerTokens, userTokens...)
	all, err := s.store.GetDelegationsByAdminScope(ctx, combined, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("load eligible delegations: %w", err)
	}

	// Post-filter: include a delegation if it was reachable via an eligibility rule owner token,
	// OR if it is an allowance delegation directly accessible to the user.
	ownerSet := common.NewTokenSet(ownerTokens)
	seen := map[string]struct{}{}
	var result []common.Delegation
	for _, d := range all {
		if _, ok := seen[d.ID]; ok {
			continue
		}
		if ownerSet.ContainsAny(d.AdminScope) {
			// Reachable via eligibility rule — include regardless of strategy.
			seen[d.ID] = struct{}{}
			result = append(result, d)
		} else if d.DelegationStrategy == common.DelegationStrategyAllowance {
			// Allowance delegation directly in user's scope — auto-approved on create.
			seen[d.ID] = struct{}{}
			result = append(result, d)
		}
	}

	return s.attachUsage(ctx, result)
}

// CreateDelegation creates a new child delegation after parent and permission checks.
func (s *Service) CreateDelegation(req webserver.CreateDelegationRequest, userEmail string) (common.Delegation, error) {
	if req.ParentID == nil || strings.TrimSpace(*req.ParentID) == "" {
		return common.Delegation{}, fmt.Errorf("parent delegation ID is required")
	}

	if userEmail == "" {
		return common.Delegation{}, common.ErrForbidden
	}

	ctx, cancel := s.newCtx()
	defer cancel()

	parent, err := s.store.GetDelegationByID(ctx, *req.ParentID)
	if err != nil {
		return common.Delegation{}, fmt.Errorf("load parent delegation: %w", err)
	}
	if parent == nil {
		return common.Delegation{}, fmt.Errorf("parent delegation not found")
	}
	if parent.DelegationStrategy == common.DelegationStrategyAllowance {
		return common.Delegation{}, fmt.Errorf("allowance delegations cannot be further delegated")
	}
	if !parent.CanDelegate {
		return common.Delegation{}, fmt.Errorf("parent delegation does not have delegation privileges")
	}

	// The child's limit cannot exceed the parent's limit for any resource.
	// This prevents creating a child that could theoretically allocate more than the parent ever has.
	for _, id := range s.quotaResourceIDs {
		parentCap := parent.Quota.Limit[id]
		childCap := req.Quota.Limit[id]
		if parentCap != common.UnlimitedQuota && childCap != common.UnlimitedQuota && childCap > parentCap {
			return common.Delegation{}, fmt.Errorf("child limit for %q (%d) exceeds parent limit (%d)", id, childCap, parentCap)
		}
		if parentCap != common.UnlimitedQuota && childCap == common.UnlimitedQuota {
			return common.Delegation{}, fmt.Errorf("child limit for %q cannot be unlimited when parent limit is %d", id, parentCap)
		}
	}

	newDelegation := common.Delegation{
		ID:                 "group_" + uuid.New().String(),
		Name:               req.Name,
		ParentID:           req.ParentID,
		CanDelegate:        req.CanDelegate,
		DelegationStrategy: req.DelegationStrategy,
		AdminScope:         req.AdminScope,
		Quota:              req.Quota,
		CreatedBy:          userEmail,
		CreatedAt:          time.Now().UTC().Format(time.RFC3339),
		EndDate:            req.EndDate,
	}

	if err := s.store.UpsertDelegation(ctx, newDelegation); err != nil {
		return common.Delegation{}, fmt.Errorf("persist delegation state: %w", err)
	}
	return newDelegation, nil
}

// UpdateDelegation applies editable delegation changes after authorization checks.
func (s *Service) UpdateDelegation(id string, req webserver.UpdateDelegationRequest, userEmail string) (common.Delegation, error) {
	if userEmail == "" {
		return common.Delegation{}, common.ErrForbidden
	}

	ctx, cancel := s.newCtx()
	defer cancel()

	current, err := s.store.GetDelegationByID(ctx, id)
	if err != nil {
		return common.Delegation{}, fmt.Errorf("load delegation: %w", err)
	}
	if current == nil {
		return common.Delegation{}, fmt.Errorf("group %w", common.ErrNotFound)
	}

	updated := *current
	if req.Name != nil {
		updated.Name = *req.Name
	}
	if req.AdminScope != nil {
		updated.AdminScope = *req.AdminScope
	}
	if req.Quota != nil {
		updated.Quota = *req.Quota
	}
	if req.EndDate != nil {
		updated.EndDate = req.EndDate
	}
	if req.DelegationStrategy != nil {
		updated.DelegationStrategy = *req.DelegationStrategy
	}
	updated.CreatedBy = userEmail

	// When the limit is being reduced, verify it doesn't fall below current active usage.
	if req.Quota != nil {
		subtreeUsage, err := s.loadSubtreeUsage(ctx, []common.Delegation{updated})
		if err != nil {
			return common.Delegation{}, fmt.Errorf("compute current usage for limit check: %w", err)
		}
		activeUsage := subtreeUsage[updated.ID].TotalQuota(s.quotaResourceIDs)
		for _, id := range s.quotaResourceIDs {
			newCap := updated.Quota.Limit[id]
			if newCap != common.UnlimitedQuota && activeUsage[id] > newCap {
				return common.Delegation{}, fmt.Errorf("new limit for %q (%d) is below current active usage (%d)", id, newCap, activeUsage[id])
			}
		}
	}

	if err := s.store.UpsertDelegation(ctx, updated); err != nil {
		return common.Delegation{}, fmt.Errorf("persist delegation state: %w", err)
	}
	return updated, nil
}

// DeleteDelegation removes a delegation subtree and clears linked project funding.
func (s *Service) DeleteDelegation(id string, userEmail string) error {
	if userEmail == "" {
		return common.ErrForbidden
	}

	ctx, cancel := s.newCtx()
	defer cancel()

	targetDelegation, err := s.store.GetDelegationByID(ctx, id)
	if err != nil {
		return fmt.Errorf("load delegation: %w", err)
	}
	if targetDelegation == nil {
		return fmt.Errorf("group %w", common.ErrNotFound)
	}

	// BFS: collect the target and all its descendants.
	parentMap, err := s.buildSubtreeParentMap(ctx, []common.Delegation{*targetDelegation})
	if err != nil {
		return fmt.Errorf("collect delegation subtree: %w", err)
	}

	deletedIDs := make([]string, 0, len(parentMap))
	for delegID := range parentMap {
		deletedIDs = append(deletedIDs, delegID)
	}

	if err := s.store.DeleteDelegations(ctx, deletedIDs); err != nil {
		return fmt.Errorf("persist delegation state: %w", err)
	}
	if err := s.store.ClearProjectFundingByDelegationIDs(ctx, deletedIDs); err != nil {
		return fmt.Errorf("persist delegation state: %w", err)
	}

	return nil
}

// ListDelegationsEligibleForOwner returns all delegations the given owner tokens may
// submit project requests to. Only root admins may call this — it is used by the
// promote flow so the root admin can see which delegations are valid funding choices
// for a specific project owner.
func (s *Service) ListDelegationsEligibleForOwner(callerTokens common.TokenList, ownerTokens common.TokenList, limit, offset int) ([]common.Delegation, error) {
	if !s.rootAdminTokens.ContainsAny(callerTokens) {
		return nil, common.ErrForbidden
	}
	if len(ownerTokens) == 0 {
		return nil, fmt.Errorf("owner_tokens must not be empty")
	}
	return s.ListDelegationsEligibleForMe(ownerTokens, limit, offset)
}
