package applogic

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/webserver"
)

// Purpose: Used for "resources delegated to me"
// Returns delegations matching user tokens and path, with optional canDelegate filter.
func (s *Service) GetDelegationsFor(userTokens common.TokenList, limit, offset int) ([]webserver.Delegation, error) {
	// Sanity checks
	if len(userTokens) == 0 {
		return nil, fmt.Errorf("no user tokens found")
	}

	// Pagination
	limit, offset = normalizePagination(limit, offset)

	// Get delegations matching tokens and path
	delegations, err := s.store.GetDelegationsFor(context.Background(), userTokens, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("load delegations by tokens: %w", err)
	}

	// Get all related approved requests to calculate usage
	requests, err := s.store.ListApprovedRequestsByDelegationIDs(context.Background(), delegationIDs(delegations))
	if err != nil {
		return nil, fmt.Errorf("load approved requests by delegation ids: %w", err)
	}
	usageByDelegation := buildUsageByDelegation(requests, s.quotaResourceIDs)

	// Generate return delegations with usage info
	withUsageOut := make([]webserver.Delegation, 0, len(delegations))
	for _, group := range delegations {
		withUsageOut = append(withUsageOut, withUsage(group, usageByDelegation))
	}
	return withUsageOut, nil
}

// ListDelegationsCreatedBy returns delegations where CreatedBy matches any of the user's groups (tokens).
func (s *Service) GetDelegationsBy(userTokens common.TokenList, limit, offset int) ([]webserver.Delegation, error) {
	// Sanity checks
	if len(userTokens) == 0 {
		return nil, fmt.Errorf("no user tokens found")
	}

	// Pagination
	limit, offset = normalizePagination(limit, offset)

	// Get delegations created by user's groups (tokens)
	delegations, err := s.store.GetDelegationsBy(context.Background(), userTokens, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("load delegations created by user tokens: %w", err)
	}

	// Add usage info to delegations by loading all related approved requests
	requests, err := s.store.ListApprovedRequestsByDelegationIDs(context.Background(), delegationIDs(delegations))
	if err != nil {
		return nil, fmt.Errorf("load approved requests by delegation ids: %w", err)
	}
	usageByDelegation := buildUsageByDelegation(requests, s.quotaResourceIDs)

	//Build return delegations with usage info
	withUsageOut := make([]webserver.Delegation, 0, len(delegations))
	for _, group := range delegations {
		withUsageOut = append(withUsageOut, withUsage(group, usageByDelegation))
	}
	return withUsageOut, nil
}

// CreateDelegation creates a new child delegation after parent and permission checks.
func (s *Service) CreateDelegation(req webserver.CreateDelegationRequest, userEmail string) (webserver.Delegation, error) {
	// Sanity checks
	if req.ParentID == nil || strings.TrimSpace(*req.ParentID) == "" {
		return webserver.Delegation{}, fmt.Errorf("parent delegation ID is required")
	}

	if userEmail == "" {
		return webserver.Delegation{}, fmt.Errorf("forbidden")
	}

	// Get existing parent delegation to check existence and permissions
	parent, err := s.store.GetDelegationByID(context.Background(), *req.ParentID)
	if err != nil {
		return webserver.Delegation{}, fmt.Errorf("load parent delegation: %w", err)
	}
	if parent == nil {
		return webserver.Delegation{}, fmt.Errorf("parent delegation not found")
	}
	if !parent.CanDelegate {
		return webserver.Delegation{}, fmt.Errorf("parent delegation does not have delegation privileges")
	}

	// Create new delegation with generated ID and persist
	newDelegation := webserver.Delegation{
		ID:                 fmt.Sprintf("group_%d", time.Now().UnixMilli()),
		Name:               req.Name,
		ParentID:           req.ParentID,
		CanDelegate:        req.CanDelegate,
		DelegationStrategy: req.DelegationStrategy,
		DelegationScope:    req.DelegationScope,
		Resources:          req.Resources,
		CreatedBy:          userEmail,
		CreatedAt:          time.Now().UTC().Format(time.RFC3339),
		EndDate:            req.EndDate,
	}

	if err := s.store.UpsertDelegation(context.Background(), newDelegation); err != nil {
		return webserver.Delegation{}, fmt.Errorf("persist delegation state: %w", err)
	}
	return newDelegation, nil
}

// UpdateDelegation applies editable delegation changes after authorization checks.
func (s *Service) UpdateDelegation(id string, req webserver.UpdateDelegationRequest, userEmail string) (webserver.Delegation, error) {
	// Sanity checks
	if userEmail == "" {
		return webserver.Delegation{}, fmt.Errorf("forbidden")
	}

	// Get existing delegation to check existence and permissions
	current, err := s.store.GetDelegationByID(context.Background(), id)
	if err != nil {
		return webserver.Delegation{}, fmt.Errorf("load delegation: %w", err)
	}
	if current == nil {
		return webserver.Delegation{}, fmt.Errorf("group not found")
	}

	// Update fields if set in request, keep existing values otherwise. Only allow updating editable fields.
	updated := *current
	if req.Name != nil {
		updated.Name = *req.Name
	}

	if req.DelegationScope != nil {
		updated.DelegationScope = *req.DelegationScope
	}

	if req.Resources != nil {
		updated.Resources = *req.Resources
	}

	if req.EndDate != nil {
		updated.EndDate = req.EndDate
	}

	if req.DelegationStrategy != nil {
		updated.DelegationStrategy = *req.DelegationStrategy
	}

	// Update the actor who made the changes
	updated.CreatedBy = userEmail

	// Persist updated delegation
	if err := s.store.UpsertDelegation(context.Background(), updated); err != nil {
		return webserver.Delegation{}, fmt.Errorf("persist delegation state: %w", err)
	}
	return updated, nil
}

// DeleteDelegation removes a delegation subtree and clears linked request funding.
func (s *Service) DeleteDelegation(id string, userEmail string) error {
	// Sanity checks
	if userEmail == "" {
		return fmt.Errorf("forbidden")
	}

	// Delegation to delete
	targetDelegation, err := s.store.GetDelegationByID(context.Background(), id)
	if err != nil {
		return fmt.Errorf("load delegation: %w", err)
	}
	if targetDelegation == nil {
		return fmt.Errorf("group not found")
	}

	// Create a set of delegation ids to delete
	toDelete := map[string]struct{}{id: {}}
	queue := []string{id}

	// Search down the tree for all delegations to delete
	for len(queue) > 0 {
		parents := append([]string(nil), queue...)
		queue = queue[:0]

		// Get child delegations of all parents in this batch
		children, err := s.store.ListDelegationsByParentIDs(context.Background(), parents, 0, 0)
		if err != nil {
			return fmt.Errorf("load child delegations: %w", err)
		}

		// Add children to delete set and queue for next iteration
		for _, group := range children {
			if _, seen := toDelete[group.ID]; seen {
				continue
			}
			toDelete[group.ID] = struct{}{}
			queue = append(queue, group.ID)
		}
	}

	deletedIDs := make([]string, 0, len(toDelete))
	for groupID := range toDelete {
		deletedIDs = append(deletedIDs, groupID)
	}

	if err := s.store.DeleteDelegations(context.Background(), deletedIDs); err != nil {
		return fmt.Errorf("persist delegation state: %w", err)
	}
	if err := s.store.ClearRequestFundingByDelegationIDs(context.Background(), deletedIDs); err != nil {
		return fmt.Errorf("persist delegation state: %w", err)
	}

	return nil
}
