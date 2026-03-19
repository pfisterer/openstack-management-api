package applogic

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/go-set"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/webserver"
)

// ListRequestsBy returns requests created by the user/group (matching their tokens).
// This function fetches all requests where the user's tokens match the requester tokens.
func (s *Service) ListRequestsBy(userEmail string, limit, offset int) ([]webserver.Request, error) {
	// Validate required parameters
	if userEmail == "" {
		return nil, fmt.Errorf("missing user email")
	}

	// Apply pagination defaults and limits
	limit, offset = normalizePagination(limit, offset)

	// Retrieve requests where the user is the requester (via token matching)
	requests, err := s.store.ListRequestsBy(context.Background(), userEmail, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list requests by requester tokens: %w", err)
	}

	return requests, nil
}

// ListRequestsManagedBy returns requests that the user/group could sponsor via their delegations.
func (s *Service) ListRequestsManagedBy(userEmail string, userTokens common.TokenList, limit, offset int) ([]webserver.Request, error) {
	// Validate required parameters
	if userEmail == "" {
		return nil, fmt.Errorf("missing user email")
	}
	if len(userTokens) == 0 {
		return nil, fmt.Errorf("no user tokens found")
	}

	// Apply pagination defaults and limits
	limit, offset = normalizePagination(limit, offset)

	// Load all delegations where the user has direct membership
	allDelegations, err := s.store.GetDelegationsFor(context.Background(), userTokens, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("load delegations by direct membership tokens: %w", err)
	}

	// If user has no delegations, return empty list
	if len(allDelegations) == 0 {
		return []webserver.Request{}, nil
	}

	// Create a set of tokens stored in delegation.DelegationScope
	delegationScopeTokens := set.New[string](20)

	for _, delegation := range allDelegations {
		for _, token := range delegation.DelegationScope {
			delegationScopeTokens.Insert(token)
		}
	}
	// If no scope tokens found, return empty list
	if delegationScopeTokens.Size() == 0 {
		return []webserver.Request{}, nil
	}

	// Fetch all requests that could potentially be sponsored by any of the user's delegations
	requests, err := s.store.GetRequestsSponsorableBy(context.Background(), delegationScopeTokens.Slice(), limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list requests by requester tokens: %w", err)
	}

	return requests, nil
}

// CreateRequest validates and stores a new request, with optional auto-approval.
//
// This function creates a new request with the provided details, validates the authorized users,
// and checks for automatic approval based on per-user allowance delegations. If an auto-approval
// delegation is found that can cover the requested resources, the request is immediately approved.
func (s *Service) CreateRequest(req webserver.CreateRequestRequest, actor string, userEmail string, userTokens common.TokenList) (webserver.Request, error) {
	// Validate required parameters
	if userEmail == "" {
		return webserver.Request{}, fmt.Errorf("no current user")
	}
	if len(userTokens) == 0 {
		return webserver.Request{}, fmt.Errorf("no current user tokens")
	}

	// Normalize and validate authorized users
	normalizedAuthorizedUsers, err := normalizeAuthorizedUsers(req.AuthorizedUsers)
	if err != nil {
		return webserver.Request{}, err
	}

	// Create initial request with pending status
	newReq := webserver.Request{
		ID:              fmt.Sprintf("req_%d", time.Now().UnixMilli()),
		Status:          "pending",
		RequesterTokens: append(common.TokenList{}, userTokens...),
		Resources:       req.Resources,
		Reason:          req.Reason,
		FundedBy:        nil,
		Pending:         nil,
		TerminationDate: req.TerminationDate,
		AuthorizedUsers: normalizedAuthorizedUsers,
		History: []webserver.HistoryEntry{{
			Timestamp:       time.Now().UTC().Format(time.RFC3339),
			Event:           "created",
			Actor:           actor,
			StatusFrom:      nil,
			StatusTo:        "pending",
			QuotaFrom:       nil,
			QuotaTo:         &req.Resources,
			TerminationDate: &req.TerminationDate,
		}},
	}

	// Load all delegations where the user is eligible to get sponsored from (based on their tokens)
	// Check for auto-approval based on per-user allowance delegations
	scopeGroups, err := s.store.GetDelegationsFor(context.Background(), userTokens, 0, 0)
	if err != nil {
		return webserver.Request{}, fmt.Errorf("load delegation scope: %w", err)
	}

	// Auto-approval logic: Check if any of the user's delegations with delegation strategy "allowance" can cover the requested quota.
	var autoFunder *webserver.Delegation
	for _, group := range scopeGroups {
		// Only consider per-user allowance delegations for auto-approval
		if group.DelegationStrategy != webserver.DelegationStrategyAllowance {
			continue
		}
		// Check if the requested quota fits within the delegation's limit
		if quotaFits(req.Resources, group.Resources.Limit, s.quotaResourceIDs) {
			groupCopy := group
			autoFunder = &groupCopy
			break
		}
	}

	// If auto-approval is possible, update the request immediately
	if autoFunder != nil {
		newReq.Status = "approved"
		newReq.FundedBy = &autoFunder.ID
		newReq.History = append(newReq.History, webserver.HistoryEntry{
			Timestamp:  time.Now().UTC().Format(time.RFC3339),
			Event:      "approved",
			Actor:      "system:auto-approval",
			Group:      &autoFunder.ID,
			StatusFrom: ptr("pending"),
			StatusTo:   "approved",
			Reason:     ptr("Auto-approved (per-user allowance)"),
		})
	}

	// Persist the request to storage
	if err := s.store.UpsertRequest(context.Background(), newReq); err != nil {
		return webserver.Request{}, fmt.Errorf("persist request state: %w", err)
	}
	return newReq, nil
}

// UpdateRequest records requested changes and transitions the request to change_pending.
//
// This function loads the current request, applies the requested changes (quota, termination date,
// or authorized users), and transitions the request status to "change_pending" to await approval.
// The changes are stored in the PendingChanges field and will be applied when the request is approved.
func (s *Service) UpdateRequest(id string, req webserver.UpdateRequestRequest, actor string) (webserver.Request, error) {
	// Load the current request
	current, err := s.store.GetRequestByID(context.Background(), id)
	if err != nil {
		return webserver.Request{}, fmt.Errorf("load request: %w", err)
	}
	if current == nil {
		return webserver.Request{}, fmt.Errorf("invalid ID")
	}

	// Determine the final quota value (requested or pending)
	quotaTo := current.Resources
	if req.Resources != nil {
		quotaTo = *req.Resources
	}

	// Build pending changes structure
	pending := &webserver.PendingChanges{Quota: &quotaTo}

	// Add termination date change if specified and different from current
	if req.TerminationDate != nil && *req.TerminationDate != current.TerminationDate {
		pending.TerminationDate = req.TerminationDate
	}

	// Add authorized users change if specified
	if req.AuthorizedUsers != nil {
		normalized, err := normalizeAuthorizedUsers(*req.AuthorizedUsers)
		if err != nil {
			return webserver.Request{}, err
		}
		pending.AuthorizedUsers = &normalized
	}

	// Create history entry for the change request
	historyEntry := webserver.HistoryEntry{
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
		Event:      "change_requested",
		Actor:      actor,
		StatusFrom: &current.Status,
		StatusTo:   "change_pending",
		QuotaFrom:  &current.Resources,
		QuotaTo:    &quotaTo,
	}
	if pending.TerminationDate != nil {
		historyEntry.TerminationDateFrom = &current.TerminationDate
		historyEntry.TerminationDateTo = pending.TerminationDate
	}

	// Update request with pending changes
	updated := *current
	updated.Pending = pending
	updated.Status = "change_pending"
	updated.History = append(append([]webserver.HistoryEntry{}, current.History...), historyEntry)

	// Persist the updated request
	if err := s.store.UpsertRequest(context.Background(), updated); err != nil {
		return webserver.Request{}, fmt.Errorf("persist request state: %w", err)
	}
	return updated, nil
}

// ApproveRequest finalizes a request from eligible delegation context and pending changes.
//
// This function validates that the requesting user is authorized to approve the request by:
// 1. Verifying the user is a member of the specified delegation
// 2. Verifying the requester is within the delegation's scope (can be sponsored)
// 3. Applying the requested quota changes or pending changes
// 4. Updating the request status to "approved" and clearing pending changes
func (s *Service) ApproveRequest(id string, req webserver.ApproveRequestRequest, actor string, userEmail string, userTokens common.TokenList) (webserver.Request, error) {
	// Validate user authorization
	if userEmail == "" {
		return webserver.Request{}, fmt.Errorf("forbidden")
	}
	if len(userTokens) == 0 {
		return webserver.Request{}, fmt.Errorf("forbidden")
	}

	// Load the current request
	current, err := s.store.GetRequestByID(context.Background(), id)
	if err != nil {
		return webserver.Request{}, fmt.Errorf("load request: %w", err)
	}
	if current == nil {
		return webserver.Request{}, fmt.Errorf("request not found")
	}

	// Load the delegation being used for approval
	group, err := s.store.GetDelegationByID(context.Background(), req.DelegationID)
	if err != nil {
		return webserver.Request{}, fmt.Errorf("load delegation: %w", err)
	}
	if group == nil {
		return webserver.Request{}, fmt.Errorf("delegation not found")
	}

	// Verify the user is a member of the delegation's scope
	isMember := false
	for _, token := range userTokens {
		if slices.Contains(group.DelegationScope, token) {
			isMember = true
		}
	}
	if !isMember {
		return webserver.Request{}, fmt.Errorf("forbidden")
	}

	// Verify the requester is within the delegation's scope (can be sponsored)
	canSponsor := false
	for _, token := range current.RequesterTokens {
		if slices.Contains(group.DelegationScope, token) {
			canSponsor = true
		}
	}
	if !canSponsor {
		return webserver.Request{}, fmt.Errorf("forbidden")
	}

	// Determine final quota value (requested override or pending changes)
	finalQuota := current.Resources
	if req.ModifiedQuota != nil {
		finalQuota = *req.ModifiedQuota
	} else if current.Pending != nil && current.Pending.Quota != nil {
		finalQuota = *current.Pending.Quota
	}

	// Determine final termination date (from pending changes if applicable)
	finalTerminationDate := current.TerminationDate
	if current.Pending != nil && current.Pending.TerminationDate != nil {
		finalTerminationDate = *current.Pending.TerminationDate
	}

	// Determine final authorized users (apply pending changes if applicable)
	finalAuthorizedUsers := append([]webserver.AuthorizedUser{}, current.AuthorizedUsers...)
	if current.Pending != nil && current.Pending.AuthorizedUsers != nil {
		finalAuthorizedUsers = append(finalAuthorizedUsers[:0], (*current.Pending.AuthorizedUsers)...)
	}
	normalizedFinalAuthorizedUsers, err := normalizeAuthorizedUsers(finalAuthorizedUsers)
	if err != nil {
		return webserver.Request{}, err
	}
	finalAuthorizedUsers = normalizedFinalAuthorizedUsers

	// Create history entry for the approval
	historyEntry := webserver.HistoryEntry{
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
		Event:      "approved",
		Actor:      actor,
		Group:      &req.DelegationID,
		StatusFrom: &current.Status,
		StatusTo:   "approved",
	}
	// Add quota change to history if applicable
	if req.ModifiedQuota != nil || (current.Pending != nil && current.Pending.Quota != nil) {
		historyEntry.QuotaFrom = &current.Resources
		historyEntry.QuotaTo = &finalQuota
	}
	// Add termination date change to history if applicable
	if current.Pending != nil && current.Pending.TerminationDate != nil && *current.Pending.TerminationDate != current.TerminationDate {
		historyEntry.TerminationDateFrom = &current.TerminationDate
		historyEntry.TerminationDateTo = &finalTerminationDate
	}
	// Add reason for approval if pending authorization changes were applied
	if current.Pending != nil && current.Pending.AuthorizedUsers != nil {
		historyEntry.Reason = ptr("Approved pending authorization changes")
	}

	// Update request with approved values
	updated := *current
	updated.Status = "approved"
	updated.FundedBy = &req.DelegationID
	updated.Resources = finalQuota
	updated.TerminationDate = finalTerminationDate
	updated.AuthorizedUsers = finalAuthorizedUsers
	updated.Pending = nil
	updated.History = append(append([]webserver.HistoryEntry{}, current.History...), historyEntry)

	// Persist the updated request
	if err := s.store.UpsertRequest(context.Background(), updated); err != nil {
		return webserver.Request{}, fmt.Errorf("persist request state: %w", err)
	}
	return updated, nil
}

// RejectRequest transitions a request into a rejected state and appends audit history.
//
// This function transitions a request to a rejected state, clearing any pending changes.
// For change_pending requests, it transitions to change_rejected to distinguish between
// original request rejection and pending changes rejection.
func (s *Service) RejectRequest(id string, req webserver.RejectRequestRequest, actor string) (webserver.Request, error) {
	// Load the current request
	current, err := s.store.GetRequestByID(context.Background(), id)
	if err != nil {
		return webserver.Request{}, fmt.Errorf("load request: %w", err)
	}
	if current == nil {
		return webserver.Request{}, fmt.Errorf("request not found")
	}

	// Determine the appropriate rejection status
	statusTo := "rejected"
	if current.Status == "change_pending" {
		statusTo = "change_rejected"
	}

	// Create history entry for the rejection
	history := webserver.HistoryEntry{
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
		Event:      "rejected",
		Actor:      actor,
		StatusFrom: &current.Status,
		StatusTo:   statusTo,
	}
	// Add rejection reason if provided
	if req.Reason != nil && strings.TrimSpace(*req.Reason) != "" {
		history.Reason = req.Reason
	}

	// Update request with rejected status
	updated := *current
	updated.Status = statusTo
	updated.Pending = nil
	updated.History = append(append([]webserver.HistoryEntry{}, current.History...), history)

	// Persist the updated request
	if err := s.store.UpsertRequest(context.Background(), updated); err != nil {
		return webserver.Request{}, fmt.Errorf("persist request state: %w", err)
	}
	return updated, nil
}

// ReleaseRequest marks approved requests as released to return allocated capacity.
//
// This function transitions an approved request to released status, effectively returning
// the allocated capacity back to the pool. This is typically used when the resource
// allocation is no longer needed or has been terminated.
func (s *Service) ReleaseRequest(id string, actor string) (webserver.Request, error) {
	// Load the current request
	current, err := s.store.GetRequestByID(context.Background(), id)
	if err != nil {
		return webserver.Request{}, fmt.Errorf("load request: %w", err)
	}
	if current == nil {
		return webserver.Request{}, fmt.Errorf("request not found")
	}

	// Validate that the request can be released (must be approved)
	if current.Status != "approved" {
		return webserver.Request{}, fmt.Errorf("cannot release")
	}

	// Create history entry for the release
	history := webserver.HistoryEntry{
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
		Event:      "released",
		Actor:      actor,
		StatusFrom: ptr("approved"),
		StatusTo:   "released",
		QuotaFrom:  &current.Resources,
		QuotaTo:    nil,
	}

	// Update request with released status
	updated := *current
	updated.Status = "released"
	updated.History = append(append([]webserver.HistoryEntry{}, current.History...), history)

	// Persist the updated request
	if err := s.store.UpsertRequest(context.Background(), updated); err != nil {
		return webserver.Request{}, fmt.Errorf("persist request state: %w", err)
	}
	return updated, nil
}
