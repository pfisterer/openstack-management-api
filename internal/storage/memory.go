package storage

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/pfisterer/openstack-management-api/internal/applogic"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/webserver"
	"go.uber.org/zap"
)

// InMemoryResourceStore is a test-friendly store implementing ResourceStore.
type InMemoryResourceStore struct {
	mu          sync.RWMutex
	identities  []webserver.Identity
	delegations []webserver.Delegation
	requests    []webserver.Request
	log         *zap.SugaredLogger
}

// Ensure InMemoryResourceStore implements the ResourceStore interface
var _ applogic.ResourceStore = (*InMemoryResourceStore)(nil)

func NewInMemoryResourceStore(log *zap.SugaredLogger) *InMemoryResourceStore {
	return &InMemoryResourceStore{
		identities:  []webserver.Identity{},
		delegations: []webserver.Delegation{},
		requests:    []webserver.Request{},
		log:         log,
	}
}

func (s *InMemoryResourceStore) IsResourceStateEmpty(_ context.Context) (bool, error) {
	// Lock for reading state
	s.mu.RLock()
	defer s.mu.RUnlock()

	// State is considered empty if there are no identities, delegations, and requests.
	return len(s.identities) == 0 && len(s.delegations) == 0 && len(s.requests) == 0, nil
}

func (s *InMemoryResourceStore) SeedResourceState(_ context.Context, identities []webserver.Identity, delegations []webserver.Delegation, requests []webserver.Request) error {
	// Lock for writing state
	s.mu.Lock()
	defer s.mu.Unlock()

	// Set the in-memory state to the provided data.
	s.identities = identities
	s.delegations = delegations
	s.requests = requests
	return nil
}

func (s *InMemoryResourceStore) GetDelegationByID(_ context.Context, id string) (*webserver.Delegation, error) {
	// Lock for reading state
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Search for the delegation with the given ID.
	for i := range s.delegations {
		if s.delegations[i].ID == id {
			return &s.delegations[i], nil
		}
	}
	return nil, nil
}

func (s *InMemoryResourceStore) ListDelegationsByParentIDs(_ context.Context, parentIDs []string, limit, offset int) ([]webserver.Delegation, error) {
	// Lock for reading state
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Build a set of parentIDs for efficient lookup
	parents := make(map[string]struct{}, len(parentIDs))
	for _, id := range parentIDs {
		parents[id] = struct{}{}
	}

	// Create results slice
	out := make([]webserver.Delegation, 0)

	// Filter delegations based on parentIDs
	for _, delegation := range s.delegations {
		// Skip delegations without a parent ID, as they cannot match any parentIDs filter
		if delegation.ParentID == nil {
			continue
		}

		// Check if the delegation's ParentID is in the provided parentIDs set
		if _, ok := parents[*delegation.ParentID]; ok {
			out = append(out, delegation)
		}
	}

	// Sort the filtered delegations by ID for consistent pagination results
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })

	// Apply pagination to the sorted results
	return paginateInMemory(out, limit, offset), nil
}

func (s *InMemoryResourceStore) GetDelegationsFor(_ context.Context, userTokens common.TokenList, limit, offset int) ([]webserver.Delegation, error) {
	// Create a set of userTokens for efficient lookup
	tokens := make(map[string]struct{}, len(userTokens))
	for _, token := range userTokens {
		tokens[token] = struct{}{}
	}

	// Lock for reading state
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Create the results slice
	out := make([]webserver.Delegation, 0)

	// Iterate over delegations and check if any of the delegation's allowlist tokens match the userTokens.
	for _, delegation := range s.delegations {
		allowlist := delegation.DelegationScope

		for _, token := range allowlist {
			if _, ok := tokens[token]; ok {
				out = append(out, delegation)
				break
			}
		}
	}

	// Sort the results by ID for consistent pagination
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })

	// Apply pagination to the sorted results
	result := paginateInMemory(out, limit, offset)

	// Log and return results
	s.log.Debugw("GetDelegationsFor", "userTokens", userTokens, "totalMatches", len(out), "returned", len(result), "results", result)
	return result, nil
}

// GetDelegationsBy returns delegations where CreatedBy matches any of the userTokens.
func (s *InMemoryResourceStore) GetDelegationsBy(_ context.Context, userTokens common.TokenList, limit, offset int) ([]webserver.Delegation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	//Build a set of userTokens for efficient lookup
	tokenSet := make(map[string]struct{}, len(userTokens))
	for _, t := range userTokens {
		tokenSet[t] = struct{}{}
	}

	// Filter
	out := make([]webserver.Delegation, 0)
	for _, delegation := range s.delegations {
		if delegation.ParentID != nil {
			if _, ok := tokenSet[*delegation.ParentID]; ok {
				out = append(out, delegation)
			}
		}
	}

	// Sort the results by ID for consistent pagination
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })

	// Apply pagination to the sorted results
	result := paginateInMemory(out, limit, offset)

	// Log and return results
	s.log.Debugw("GetDelegationsBy", "userTokens", userTokens, "totalMatches", len(out), "returned", len(result), "results", result)
	return result, nil
}

func (s *InMemoryResourceStore) UpsertDelegation(_ context.Context, delegation webserver.Delegation) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.delegations {
		if s.delegations[i].ID == delegation.ID {
			s.delegations[i] = delegation
			return nil
		}
	}

	s.delegations = append(s.delegations, delegation)
	return nil
}

func (s *InMemoryResourceStore) DeleteDelegations(_ context.Context, delegationIDs []string) error {
	if len(delegationIDs) == 0 {
		return nil
	}
	remove := make(map[string]struct{}, len(delegationIDs))
	for _, id := range delegationIDs {
		remove[id] = struct{}{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	filtered := make([]webserver.Delegation, 0, len(s.delegations))
	for _, delegation := range s.delegations {
		if _, ok := remove[delegation.ID]; !ok {
			filtered = append(filtered, delegation)
		}
	}
	s.delegations = filtered
	return nil
}

func (s *InMemoryResourceStore) GetRequestByID(_ context.Context, id string) (*webserver.Request, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.requests {
		if s.requests[i].ID == id {
			return &s.requests[i], nil
		}
	}
	return nil, nil
}

// ListRequestsBy returns all requests where the requester tokens contain the given userEmail.
//
// Matching behavior: This function performs an OR-match ("any token").
// If the provided userEmail is present in the requester's tokens, the request is included in the result.
// This is NOT an AND-match; a request does not need to match all tokens, only one or more.
func (s *InMemoryResourceStore) ListRequestsBy(_ context.Context, userEmail string, limit, offset int) ([]webserver.Request, error) {
	if userEmail == "" {
		return []webserver.Request{}, fmt.Errorf("userEmail is required")
	}

	// Query logic: Build a set of userTokens and check if any requester's token matches (OR-match).
	tokens := make(map[string]struct{}, 1)
	tokens["user:"+userEmail] = struct{}{}

	// Lock for reading state
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]webserver.Request, 0)
	for _, req := range s.requests {
		for _, requesterToken := range req.RequesterTokens {
			if _, ok := tokens[requesterToken]; ok {
				out = append(out, req)
				break
			}
		}
	}

	// Sort results by ID for consistent pagination
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })

	// Apply pagination to the sorted results
	result := paginateInMemory(out, limit, offset)

	// Log and return results
	s.log.Debugw("ListRequestsBy", "userEmail", userEmail, "totalMatches", len(out), "returned", len(result), "results", result)
	return result, nil
}

func (s *InMemoryResourceStore) GetRequestsSponsorableBy(_ context.Context, delegationScopeTokens []string, limit, offset int) ([]webserver.Request, error) {
	if len(delegationScopeTokens) == 0 {
		return []webserver.Request{}, nil
	}

	// Build a set of delegationScopeTokens for efficient lookup
	tokens := make(map[string]struct{}, len(delegationScopeTokens))
	for _, token := range delegationScopeTokens {
		tokens[token] = struct{}{}
	}

	// Lock for reading state
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Query logic
	out := make([]webserver.Request, 0)

	for _, req := range s.requests {
		// Only consider requests that are in "pending" status, as only those can be sponsored.
		if req.Status != "pending" {
			continue
		}

		// Check if any of the request's requester tokens match any of the delegation scope tokens (OR-match).
		for _, requesterToken := range req.RequesterTokens {
			if _, ok := tokens[requesterToken]; ok {
				out = append(out, req)
				break
			}
		}
	}

	// Sort results by ID for consistent pagination
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })

	// Apply pagination to the sorted results
	result := paginateInMemory(out, limit, offset)

	// Log and return results
	s.log.Debugw("GetRequestsSponsorableBy", "delegationScopeTokens", delegationScopeTokens, "totalMatches", len(out), "returned", len(result), "results", result)
	return result, nil
}

func (s *InMemoryResourceStore) ListApprovedRequestsByDelegationIDs(_ context.Context, delegationIDs []string) ([]webserver.Request, error) {
	if len(delegationIDs) == 0 {
		return []webserver.Request{}, nil
	}

	fundedBy := make(map[string]struct{}, len(delegationIDs))
	for _, id := range delegationIDs {
		fundedBy[id] = struct{}{}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]webserver.Request, 0)
	for _, req := range s.requests {
		if req.Status != "approved" || req.FundedBy == nil {
			continue
		}
		if _, ok := fundedBy[*req.FundedBy]; ok {
			out = append(out, req)
		}
	}

	return out, nil
}

func (s *InMemoryResourceStore) UpsertRequest(_ context.Context, request webserver.Request) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.requests {
		if s.requests[i].ID == request.ID {
			s.requests[i] = request
			return nil
		}
	}

	s.requests = append([]webserver.Request{request}, s.requests...)
	return nil
}

func (s *InMemoryResourceStore) ClearRequestFundingByDelegationIDs(_ context.Context, delegationIDs []string) error {
	if len(delegationIDs) == 0 {
		return nil
	}
	remove := make(map[string]struct{}, len(delegationIDs))
	for _, id := range delegationIDs {
		remove[id] = struct{}{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.requests {
		if s.requests[i].FundedBy == nil {
			continue
		}
		if _, ok := remove[*s.requests[i].FundedBy]; ok {
			s.requests[i].FundedBy = nil
		}
	}
	return nil
}

func paginateInMemory[T any](items []T, limit, offset int) []T {
	if limit <= 0 {
		limit = len(items)
	}
	if offset < 0 {
		offset = 0
	}
	if offset >= len(items) {
		return []T{}
	}
	end := offset + limit
	if end > len(items) {
		end = len(items)
	}
	out := make([]T, end-offset)
	copy(out, items[offset:end])
	return out
}
