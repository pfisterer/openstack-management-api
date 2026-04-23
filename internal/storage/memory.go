package storage

import (
	"context"
	"sort"
	"sync"

	"github.com/pfisterer/openstack-management-api/internal/applogic"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"go.uber.org/zap"
)

// Ensure InMemoryProjectStore implements the ProjectStore interface
var _ applogic.ProjectStore = (*InMemoryProjectStore)(nil)

// InMemoryProjectStore is a test-friendly store implementing ProjectStore.
type InMemoryProjectStore struct {
	mu               sync.RWMutex
	identities       []common.Identity
	delegations      []common.Delegation
	projects         []common.Project
	eligibilityRules map[string]common.TokenEligibilityRule // keyed by OwnerToken
	log              *zap.SugaredLogger
}

func NewInMemoryProjectStore(log *zap.SugaredLogger) *InMemoryProjectStore {
	return &InMemoryProjectStore{
		identities:       []common.Identity{},
		delegations:      []common.Delegation{},
		projects:         []common.Project{},
		eligibilityRules: make(map[string]common.TokenEligibilityRule),
		log:              log,
	}
}

func (s *InMemoryProjectStore) IsProjectStateEmpty(_ context.Context) (bool, error) {
	// Lock for reading state
	s.mu.RLock()
	defer s.mu.RUnlock()

	// State is considered empty if there are no identities, delegations, and projects.
	return len(s.identities) == 0 && len(s.delegations) == 0 && len(s.projects) == 0, nil
}

func (s *InMemoryProjectStore) SeedProjectState(_ context.Context, identities []common.Identity, delegations []common.Delegation, projects []common.Project, eligibilityRules []common.TokenEligibilityRule) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.identities = identities
	s.delegations = delegations
	s.projects = projects

	s.eligibilityRules = make(map[string]common.TokenEligibilityRule, len(eligibilityRules))
	for _, rule := range eligibilityRules {
		s.eligibilityRules[rule.OwnerToken] = rule
	}
	return nil
}

func (s *InMemoryProjectStore) GetDelegationByID(_ context.Context, id string) (*common.Delegation, error) {
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

// filterDelegations applies predicate to all delegations under a read lock,
// sorts results ascending by ID, and returns the paginated slice.
func (s *InMemoryProjectStore) filterDelegations(predicate func(common.Delegation) bool, limit, offset int) []common.Delegation {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []common.Delegation
	for _, d := range s.delegations {
		if predicate(d) {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return paginateInMemory(out, limit, offset)
}

func (s *InMemoryProjectStore) ListDelegationsByParentIDs(_ context.Context, parentIDs []string, limit, offset int) ([]common.Delegation, error) {
	parents := make(map[string]struct{}, len(parentIDs))
	for _, id := range parentIDs {
		parents[id] = struct{}{}
	}
	return s.filterDelegations(func(d common.Delegation) bool {
		if d.ParentID == nil {
			return false
		}
		_, ok := parents[*d.ParentID]
		return ok
	}, limit, offset), nil
}

func (s *InMemoryProjectStore) GetDelegationsByParentTokens(_ context.Context, userTokens common.TokenList, limit, offset int) ([]common.Delegation, error) {
	tokens := make(map[string]struct{}, len(userTokens))
	for _, token := range userTokens {
		tokens[token] = struct{}{}
	}
	result := s.filterDelegations(func(d common.Delegation) bool {
		if d.ParentID == nil {
			return false
		}
		_, ok := tokens[*d.ParentID]
		return ok
	}, limit, offset)
	s.log.Debugw("GetDelegationsByParentTokens", "userTokens", userTokens, "returned", len(result))
	return result, nil
}

func (s *InMemoryProjectStore) GetDelegationsByAdminScope(_ context.Context, userTokens common.TokenList, limit, offset int) ([]common.Delegation, error) {
	tokenSet := make(map[string]struct{}, len(userTokens))
	for _, t := range userTokens {
		tokenSet[t] = struct{}{}
	}
	result := s.filterDelegations(func(d common.Delegation) bool {
		for _, token := range d.AdminScope {
			if _, ok := tokenSet[token]; ok {
				return true
			}
		}
		return false
	}, limit, offset)
	s.log.Debugw("GetDelegationsByAdminScope", "userTokens", userTokens, "returned", len(result))
	return result, nil
}

func (s *InMemoryProjectStore) UpsertDelegation(_ context.Context, delegation common.Delegation) error {
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

func (s *InMemoryProjectStore) DeleteDelegations(_ context.Context, delegationIDs []string) error {
	if len(delegationIDs) == 0 {
		return nil
	}
	remove := make(map[string]struct{}, len(delegationIDs))
	for _, id := range delegationIDs {
		remove[id] = struct{}{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	filtered := make([]common.Delegation, 0, len(s.delegations))
	for _, delegation := range s.delegations {
		if _, ok := remove[delegation.ID]; !ok {
			filtered = append(filtered, delegation)
		}
	}
	s.delegations = filtered
	return nil
}

func (s *InMemoryProjectStore) GetProjectByID(_ context.Context, id string) (*common.Project, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.projects {
		if s.projects[i].ID == id {
			return &s.projects[i], nil
		}
	}
	return nil, nil
}

// ListProjectsBy returns all projects where the requester tokens contain the given userEmail.
//
// Matching behavior: This function performs an OR-match ("any token").
// If the provided userEmail is present in the requester's tokens, the project is included in the result.
// This is NOT an AND-match; a project does not need to match all tokens, only one or more.
func (s *InMemoryProjectStore) ListProjectsBy(_ context.Context, userEmail string, limit, offset int) ([]common.Project, error) {

	// Query logic: Build a set of userTokens and check if any requester's token matches (OR-match).
	tokens := make(map[string]struct{}, 1)
	tokens["user:"+userEmail] = struct{}{}

	// Lock for reading state
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]common.Project, 0)
	for _, proj := range s.projects {
		for _, requesterToken := range proj.RequesterTokens {
			if _, ok := tokens[requesterToken]; ok {
				out = append(out, proj)
				break
			}
		}
	}

	// Sort results by ID for consistent pagination
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })

	// Apply pagination to the sorted results
	result := paginateInMemory(out, limit, offset)

	// Log and return results
	s.log.Debugw("ListProjectsBy", "userEmail", userEmail, "totalMatches", len(out), "returned", len(result), "results", result)
	return result, nil
}

func (s *InMemoryProjectStore) GetProjectsByFundedByIDs(_ context.Context, delegationIDs []string, statuses []string, limit, offset int) ([]common.Project, error) {
	if len(delegationIDs) == 0 {
		return []common.Project{}, nil
	}

	fundedBy := make(map[string]struct{}, len(delegationIDs))
	for _, id := range delegationIDs {
		fundedBy[id] = struct{}{}
	}
	allowedStatus := make(map[string]struct{}, len(statuses))
	for _, s := range statuses {
		allowedStatus[s] = struct{}{}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]common.Project, 0)
	for _, req := range s.projects {
		if req.FundedBy == nil {
			continue
		}
		if _, ok := allowedStatus[req.Status]; !ok {
			continue
		}
		if _, ok := fundedBy[*req.FundedBy]; ok {
			out = append(out, req)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return paginateInMemory(out, limit, offset), nil
}

func (s *InMemoryProjectStore) ListProjectsByStatus(_ context.Context, statuses []string, limit, offset int) ([]common.Project, error) {
	allowedStatus := make(map[string]struct{}, len(statuses))
	for _, st := range statuses {
		allowedStatus[st] = struct{}{}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]common.Project, 0)
	for _, proj := range s.projects {
		if _, ok := allowedStatus[proj.Status]; ok {
			out = append(out, proj)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return paginateInMemory(out, limit, offset), nil
}

func (s *InMemoryProjectStore) DeleteProject(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, proj := range s.projects {
		if proj.ID == id {
			s.projects = append(s.projects[:i], s.projects[i+1:]...)
			return nil
		}
	}
	return nil // not found is not an error
}

func (s *InMemoryProjectStore) UpsertProject(_ context.Context, project common.Project) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.projects {
		if s.projects[i].ID == project.ID {
			s.projects[i] = project
			return nil
		}
	}

	s.projects = append([]common.Project{project}, s.projects...)
	return nil
}

func (s *InMemoryProjectStore) ClearProjectFundingByDelegationIDs(_ context.Context, delegationIDs []string) error {
	if len(delegationIDs) == 0 {
		return nil
	}
	remove := make(map[string]struct{}, len(delegationIDs))
	for _, id := range delegationIDs {
		remove[id] = struct{}{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.projects {
		if s.projects[i].FundedBy == nil {
			continue
		}
		if _, ok := remove[*s.projects[i].FundedBy]; ok {
			s.projects[i].FundedBy = nil
		}
	}
	return nil
}

func (s *InMemoryProjectStore) GetEligibilityRulesByOwnerTokens(_ context.Context, ownerTokens []string) ([]common.TokenEligibilityRule, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]common.TokenEligibilityRule, 0, len(ownerTokens))
	for _, token := range ownerTokens {
		if rule, ok := s.eligibilityRules[token]; ok {
			out = append(out, rule)
		}
	}
	return out, nil
}

func (s *InMemoryProjectStore) GetEligibilityRulesByRequesterTokens(_ context.Context, requesterTokens []string) ([]common.TokenEligibilityRule, error) {
	needle := make(map[string]struct{}, len(requesterTokens))
	for _, t := range requesterTokens {
		needle[t] = struct{}{}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]common.TokenEligibilityRule, 0)
	for _, rule := range s.eligibilityRules {
		for _, t := range rule.EligibleRequesters {
			if _, ok := needle[t]; ok {
				out = append(out, rule)
				break
			}
		}
	}
	return out, nil
}

func (s *InMemoryProjectStore) UpsertEligibilityRule(_ context.Context, rule common.TokenEligibilityRule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eligibilityRules[rule.OwnerToken] = rule
	return nil
}

func (s *InMemoryProjectStore) DeleteEligibilityRule(_ context.Context, ownerToken string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.eligibilityRules, ownerToken)
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
