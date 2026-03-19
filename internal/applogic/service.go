package applogic

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/mockdata"
	"github.com/pfisterer/openstack-management-api/internal/webserver"
	"go.uber.org/zap"
)

type Service struct {
	store            ResourceStore
	roles            common.RoleProvider
	quotaResourceIDs []string

	overridesWriteMu sync.Mutex
	groupOverrides   atomic.Value // stores immutable map[string]string snapshots

	log *zap.SugaredLogger
}

const (
	defaultListLimit = 50
	maxListLimit     = 100
)

// Ensure Service implements the ResourceAPIService interface at compile time.
var _ webserver.ResourceAPIService = (*Service)(nil)

// NewService constructs the resource service with default override state.
func NewService(store ResourceStore, roles common.RoleProvider, quotaResourceIDs []string, log *zap.SugaredLogger) *Service {
	if store == nil {
		panic("resources.NewService requires a non-nil store")
	}
	if roles == nil {
		panic("resources.NewService requires a non-nil role provider")
	}

	if len(quotaResourceIDs) == 0 {
		panic("resources.NewService requires at least one configured resource type")
	}

	svc := &Service{
		store:            store,
		roles:            roles,
		quotaResourceIDs: quotaResourceIDs,
		log:              log,
	}

	// Seed the first snapshot so readers can load without special cases.
	svc.groupOverrides.Store(map[string]string{})
	return svc
}

// InitializeState seeds storage with mock data when requested and when storage is empty.
func (s *Service) InitializeState(ctx context.Context, addMockData bool) error {

	// Cheeck if store is empty before potentially adding mock data, to avoid overwriting any existing state.
	empty, err := s.store.IsResourceStateEmpty(ctx)
	if err != nil {
		return fmt.Errorf("resource service: check state emptiness: %w", err)
	}

	if !empty || !addMockData {
		return nil
	}

	//Add mock data for initial application state and testing.
	identities, delegations, requests := mockdata.DefaultMockResourceState(time.Now().UTC())
	if err := s.store.SeedResourceState(ctx, identities, delegations, requests); err != nil {
		return fmt.Errorf("resource service: seed mock state: %w", err)
	}

	return nil
}

// ResolveEffectiveUserTokens applies a temporary actor-specific group override.
// It keeps authentication and base role resolution in webserver auth context.
func (s *Service) ResolveEffectiveUserTokens(actorEmail string, originalTokens common.TokenList) common.TokenList {
	canonicalActor := canonicalActorEmail(actorEmail)
	if canonicalActor == "" {
		s.log.Warnf("ResolveEffectiveUserTokens called with empty actorEmail, returning original tokens")
		return webserver.ApplyRoleSwitchOverride(originalTokens, nil)
	}

	overrideGroupToken := s.lookupOverrideGroupToken(canonicalActor)
	return webserver.ApplyRoleSwitchOverride(originalTokens, overrideGroupToken)
}

// SetUserGroupSwitchForActor stores a temporary effective group for one actor.
func (s *Service) SetUserGroupSwitchForActor(actorEmail, groupToken string) error {
	// Validate inputs before setting the override.
	normalizedActor := canonicalActorEmail(actorEmail)
	if normalizedActor == "" {
		return fmt.Errorf("actor email must not be empty")
	}

	// Normalize and validate the group token before setting the override.
	normalizedGroup := webserver.NormalizeGroupToken(groupToken)
	if normalizedGroup == "" {
		return fmt.Errorf("group_token must not be empty")
	}

	// Set the override in a thread-safe manner.
	s.overridesWriteMu.Lock()
	defer s.overridesWriteMu.Unlock()

	current := s.currentGroupOverrides()
	next := cloneGroupOverrides(current, 1)
	next[normalizedActor] = normalizedGroup
	s.groupOverrides.Store(next)
	return nil
}

// ClearUserGroupSwitchForActor removes the temporary group override for one actor.
func (s *Service) ClearUserGroupSwitchForActor(actorEmail string) {
	// Validate inputs before clearing the override.
	normalizedActor := canonicalActorEmail(actorEmail)
	if normalizedActor == "" {
		return
	}

	// Get write lock to clear the override in a thread-safe manner.
	s.overridesWriteMu.Lock()
	defer s.overridesWriteMu.Unlock()

	// Clear the override in a thread-safe manner.
	current := s.currentGroupOverrides()
	next := cloneGroupOverrides(current, 0)
	delete(next, normalizedActor)
	s.groupOverrides.Store(next)
}

// GetUserGroupSwitchForActor retrieves the temporary group override for one actor.
func (s *Service) GetUserGroupSwitchForActor(actorEmail string) *string {
	return s.lookupOverrideGroupToken(canonicalActorEmail(actorEmail))
}

// SearchGroupTokens returns matching group tokens via the role provider.
func (s *Service) SearchGroupTokens(query string, limit int) (common.TokenList, error) {
	return s.roles.SearchGroupTokens(context.Background(), query, limit)
}

// currentGroupOverrides returns the latest immutable override snapshot.
func (s *Service) currentGroupOverrides() map[string]string {
	return s.groupOverrides.Load().(map[string]string)
}

// cloneGroupOverrides creates a writable copy for copy-on-write updates.
func cloneGroupOverrides(current map[string]string, extraCapacity int) map[string]string {
	next := make(map[string]string, len(current)+extraCapacity)
	maps.Copy(next, current)
	return next
}

// ptr returns a pointer to the provided value for inline literals.
func ptr[T any](v T) *T {
	return &v
}

// canonicalActorEmail normalizes actor identifiers used as override map keys.
func canonicalActorEmail(actorEmail string) string {
	return strings.ToLower(strings.TrimSpace(actorEmail))
}

// lookupOverrideGroupToken returns the configured override token for one actor.
func (s *Service) lookupOverrideGroupToken(canonicalActor string) *string {
	if canonicalActor == "" {
		return nil
	}

	if override := s.currentGroupOverrides()[canonicalActor]; override != "" {
		return ptr(override)
	}

	return nil
}

// normalizePagination clamps incoming pagination values to safe limits.
func normalizePagination(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// normalizeGroupRoleOrEmpty validates and canonicalizes group role input.
func normalizeGroupRoleOrEmpty(role string) string {
	r := strings.ToLower(strings.TrimSpace(role))
	switch r {
	case "admin":
		return "admin"
	case "member", "operator":
		return "member"
	case "viewer", "reader":
		return "viewer"
	default:
		return ""
	}
}

// normalizeOpenstackRoleOrEmpty validates OpenStack role input.
func normalizeOpenstackRoleOrEmpty(role string) string {
	r := strings.ToLower(strings.TrimSpace(role))
	if r == "" {
		return ""
	}
	return r
}

// normalizeAuthorizedUsers validates and normalizes authorization entries.
func normalizeAuthorizedUsers(users []webserver.AuthorizedUser) ([]webserver.AuthorizedUser, error) {
	out := make([]webserver.AuthorizedUser, 0, len(users))
	for i, user := range users {
		token := strings.TrimSpace(user.Token)
		if token == "" {
			return nil, fmt.Errorf("invalid authorized_users: entry %d has empty token", i)
		}

		groupRole := normalizeGroupRoleOrEmpty(user.GroupRole)
		openstackRole := normalizeOpenstackRoleOrEmpty(user.OpenstackRole)

		if groupRole == "" {
			return nil, fmt.Errorf("invalid authorized_users: entry %d has invalid or missing group_role", i)
		}

		if openstackRole == "" {
			return nil, fmt.Errorf("invalid authorized_users: entry %d has invalid or missing openstack_role", i)
		}

		out = append(out, webserver.AuthorizedUser{
			Token:         token,
			GroupRole:     groupRole,
			OpenstackRole: openstackRole,
		})
	}
	return out, nil
}

// quotaAdd sums configured resource types from two quota objects.
func quotaAdd(a webserver.ResourceQuota, b webserver.ResourceQuota, resourceIDs []string) webserver.ResourceQuota {
	out := make(webserver.ResourceQuota, len(a)+len(resourceIDs))
	maps.Copy(out, a)
	for _, resourceID := range resourceIDs {
		out[resourceID] = a[resourceID] + b[resourceID]
	}
	return out
}

// quotaFits checks whether requested quota stays within configured resource limits.
func quotaFits(need webserver.ResourceQuota, limit webserver.ResourceQuota, resourceIDs []string) bool {
	for _, resourceID := range resourceIDs {
		if need[resourceID] > limit[resourceID] {
			return false
		}
	}

	return true
}

// buildUsageByDelegation aggregates approved request usage by funding delegation.
func buildUsageByDelegation(requests []webserver.Request, resourceIDs []string) map[string]webserver.ResourceQuota {
	usageByDelegation := make(map[string]webserver.ResourceQuota)
	for _, req := range requests {
		if req.Status == "approved" && req.FundedBy != nil {
			current := usageByDelegation[*req.FundedBy]
			usageByDelegation[*req.FundedBy] = quotaAdd(current, req.Resources, resourceIDs)
		}
	}
	return usageByDelegation
}

// delegationIDs extracts delegation IDs for batched store lookups.
func delegationIDs(groups []webserver.Delegation) []string {
	ids := make([]string, 0, len(groups))
	for _, group := range groups {
		ids = append(ids, group.ID)
	}
	return ids
}

// withUsage attaches computed usage data to a delegation response.
func withUsage(group webserver.Delegation, usageByDelegation map[string]webserver.ResourceQuota) webserver.Delegation {
	usage := usageByDelegation[group.ID]
	out := group
	out.Resources.Usage = &usage
	return out
}
