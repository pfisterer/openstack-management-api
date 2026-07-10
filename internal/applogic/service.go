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
	store            ProjectStore
	roles            common.RoleProvider
	quotaResourceIDs []string
	rootAdminTokens  common.TokenSet
	rootAdminList    common.TokenList // original order, for the root-delegation admin_scope
	requestTimeout   time.Duration

	overridesWriteMu sync.Mutex
	groupOverrides   atomic.Value // stores immutable map[string]string snapshots

	// approvalMu serializes the capacity check-then-write critical section of the
	// approval paths (CreateProject auto-approve, ApproveProject,
	// MarkProjectForPromotion) so concurrent approvals cannot both pass the
	// capacity check and over-allocate a pool. Process-level lock — correct for
	// the single-replica deployment; a multi-replica setup would need DB-level
	// row locking (SELECT ... FOR UPDATE) instead.
	approvalMu sync.Mutex

	log *zap.SugaredLogger
}

const (
	defaultListLimit = common.DefaultPageLimit
	maxListLimit     = common.MaxPageLimit
)

// Ensure Service implements the ProjectAPIService interface at compile time.
var _ webserver.ProjectAPIService = (*Service)(nil)

// NewService constructs the resource service with default override state.
// rootAdminTokens are the group tokens (e.g. "group:root_uni") whose holders have
// system-wide admin access and can see all openstack_only records in ListProjectsManagedBy.
func NewService(store ProjectStore, roles common.RoleProvider, quotaResourceIDs []string, rootAdminTokens common.TokenList, requestTimeout time.Duration, log *zap.SugaredLogger) *Service {
	if store == nil {
		panic("resources.NewService requires a non-nil store")
	}
	if roles == nil {
		panic("resources.NewService requires a non-nil role provider")
	}

	if len(quotaResourceIDs) == 0 {
		panic("resources.NewService requires at least one configured resource type")
	}

	if requestTimeout <= 0 {
		requestTimeout = 30 * time.Second
	}
	svc := &Service{
		store:            store,
		roles:            roles,
		quotaResourceIDs: quotaResourceIDs,
		rootAdminTokens:  common.NewTokenSet(rootAdminTokens),
		rootAdminList:    rootAdminTokens,
		requestTimeout:   requestTimeout,
		log:              log,
	}

	// Seed the first snapshot so readers can load without special cases.
	svc.groupOverrides.Store(map[string]string{})
	return svc
}

// ── Authorization helpers ─────────────────────────────────────────────────────
// Shared by the delegation/project mutation methods so authorization is enforced
// consistently (mirrors the read-side chain walk in GetProjectByID).

// callerInAdminScopeChain reports whether any of the caller's tokens is in the
// AdminScope of the delegation identified by startID or any of its ancestors.
func (s *Service) callerInAdminScopeChain(ctx context.Context, callerSet common.TokenSet, startID *string) (bool, error) {
	for delegID := startID; delegID != nil; {
		deleg, err := s.store.GetDelegationByID(ctx, *delegID)
		if err != nil {
			return false, fmt.Errorf("load delegation for auth check: %w", err)
		}
		if deleg == nil {
			return false, nil
		}
		if callerSet.ContainsAny(deleg.AdminScope) {
			return true, nil
		}
		delegID = deleg.ParentID
	}
	return false, nil
}

// canManageDelegation reports whether the caller may create-under / edit / delete
// the given delegation: root admins, or holders of a token in the delegation's own
// AdminScope or that of any ancestor (the owning parent chain).
func (s *Service) canManageDelegation(ctx context.Context, userTokens common.TokenList, deleg *common.Delegation) (bool, error) {
	if s.rootAdminTokens.ContainsAny(userTokens) {
		return true, nil
	}
	return s.callerInAdminScopeChain(ctx, common.NewTokenSet(userTokens), &deleg.ID)
}

// canManageProjectFunding reports whether the caller administers the delegation
// funding the project (or any ancestor), or is a root admin.
func (s *Service) canManageProjectFunding(ctx context.Context, userTokens common.TokenList, p *common.Project) (bool, error) {
	if s.rootAdminTokens.ContainsAny(userTokens) {
		return true, nil
	}
	if p.FundedBy == nil {
		return false, nil
	}
	return s.callerInAdminScopeChain(ctx, common.NewTokenSet(userTokens), p.FundedBy)
}

// isProjectRequester reports whether the caller holds one of the project's requester tokens.
func isProjectRequester(userTokens common.TokenList, p *common.Project) bool {
	return common.NewTokenSet(userTokens).ContainsAny(p.RequesterTokens)
}

// ── Quota / lifecycle validation ──────────────────────────────────────────────

// validateProjectQuota rejects a project quota that is negative or set on an
// unmanaged resource. Unlike a delegation LIMIT, a project quota is a concrete
// allocation with no "unlimited" (-1) sentinel: previously a negative value
// passed quotaFits, auto-approved, lowered the tracked pool usage, and (as -1)
// mapped to Nova's unlimited — letting a request grab unbounded resources.
func (s *Service) validateProjectQuota(q common.ProjectQuota) error {
	known := make(map[string]struct{}, len(s.quotaResourceIDs))
	for _, id := range s.quotaResourceIDs {
		known[id] = struct{}{}
	}
	for key, val := range q {
		if _, ok := known[key]; !ok {
			return fmt.Errorf("unknown quota resource %q", key)
		}
		if val < 0 {
			return fmt.Errorf("quota for %q must not be negative (got %d)", key, val)
		}
	}
	return nil
}

// isTerminalProjectStatus reports whether a project is in a terminal state that
// must not transition further (guards against reviving released/rejected work).
func isTerminalProjectStatus(status string) bool {
	switch status {
	case common.ProjectStatusReleased, common.ProjectStatusRejected, common.ProjectStatusChangeRejected:
		return true
	}
	return false
}

// userAllowanceUsage sums the caller's already-committed (active) quota funded by
// the given allowance delegation, so auto-approval can enforce a cumulative
// per-user cap rather than only checking each request in isolation.
func (s *Service) userAllowanceUsage(ctx context.Context, delegationID string, userTokens common.TokenList) (common.ProjectQuota, error) {
	projects, err := s.store.GetProjectsByFundedByIDs(ctx, []string{delegationID}, common.ActiveProjectStatuses, 0, 0)
	if err != nil {
		return nil, err
	}
	callerSet := common.NewTokenSet(userTokens)
	usage := make(common.ProjectQuota, len(s.quotaResourceIDs))
	for _, p := range projects {
		if !callerSet.ContainsAny(p.RequesterTokens) {
			continue
		}
		for _, id := range s.quotaResourceIDs {
			usage[id] += p.Quota[id]
		}
	}
	return usage, nil
}

// InitializeState seeds storage with mock data when requested and when storage is empty.
func (s *Service) InitializeState(ctx context.Context, addMockData bool) error {

	// Check if store is empty before potentially adding mock data, to avoid overwriting any existing state.
	empty, err := s.store.IsProjectStateEmpty(ctx)
	if err != nil {
		return fmt.Errorf("resource service: check state emptiness: %w", err)
	}

	if addMockData {
		// Mock seed (dev/testing) into an empty store; it carries its own root
		// delegation, so the real-mode bootstrap below is not needed here.
		if empty {
			identities, delegations, requests, eligibilityRules := mockdata.DefaultMockResourceState()
			if err := s.store.SeedProjectState(ctx, identities, delegations, requests, eligibilityRules); err != nil {
				return fmt.Errorf("resource service: seed mock state: %w", err)
			}
		}
		return nil
	}

	// Real mode: ensure the configured root admins have an unlimited top-level pool
	// to delegate from (the equivalent of the mock's System-created root).
	return s.ensureRootDelegation(ctx)
}

// rootDelegationID is the stable ID of the bootstrapped top-level pool.
const rootDelegationID = "root"

// ensureRootDelegation creates a single unlimited top-level pool owned by the
// configured root admins if none exists. Idempotent — safe on every startup, and
// self-healing if the root is deleted. Without it root admins would have full
// privileges but no budget to delegate from (CreateDelegation always requires a
// parent, so a root can't be created through the API).
func (s *Service) ensureRootDelegation(ctx context.Context) error {
	if len(s.rootAdminList) == 0 {
		return nil // no root admins configured → no implicit root pool
	}

	existing, err := s.store.GetDelegationByID(ctx, rootDelegationID)
	if err != nil {
		return fmt.Errorf("check root delegation: %w", err)
	}
	if existing != nil {
		return nil
	}

	limit := make(common.ProjectQuota, len(s.quotaResourceIDs))
	for _, id := range s.quotaResourceIDs {
		limit[id] = common.UnlimitedQuota
	}

	root := common.Delegation{
		ID:                 rootDelegationID,
		Name:               "Organization Root",
		ParentID:           nil,
		CanDelegate:        true,
		DelegationStrategy: common.DelegationStrategyPool,
		AdminScope:         append(common.TokenList{}, s.rootAdminList...),
		Quota:              common.ProjectResources{Limit: limit},
		CreatedBy:          "System",
		CreatedAt:          time.Now().UTC().Format(time.RFC3339),
	}
	if err := s.store.UpsertDelegation(ctx, root); err != nil {
		return fmt.Errorf("bootstrap root delegation: %w", err)
	}
	s.log.Infow("bootstrapped unlimited root delegation for root admins", "admin_scope", root.AdminScope)
	return nil
}

// userTokenPrefix marks an impersonation override: the actor fully assumes the
// identity with this email (see ResolveEffectiveUserTokens).
const userTokenPrefix = "user:"

// ResolveEffectiveUserTokens applies a temporary actor-specific role-switch
// override. Two modes, distinguished by the stored override value:
//   - group override ("group:…"): swap group tokens, preserve everything else —
//     the actor keeps their own user/root identity and acts within the group.
//   - identity override ("user:…"): replace the ENTIRE token set with the target
//     identity's tokens, so the actor fully impersonates that user, including
//     dropping their own root-admin grant.
//
// It keeps authentication and base role resolution in webserver auth context.
func (s *Service) ResolveEffectiveUserTokens(actorEmail string, originalTokens common.TokenList) common.TokenList {
	canonicalActor := canonicalActorEmail(actorEmail)
	if canonicalActor == "" {
		s.log.Warnf("ResolveEffectiveUserTokens called with empty actorEmail, returning original tokens")
		return originalTokens
	}

	override := s.currentGroupOverrides()[canonicalActor]
	if override == "" {
		return originalTokens
	}

	if email, ok := strings.CutPrefix(override, userTokenPrefix); ok {
		ctx, cancel := s.newCtx()
		defer cancel()
		tokens, err := s.roles.GetUserTokens(ctx, &common.UserClaims{Email: email})
		if err != nil || len(tokens) == 0 {
			s.log.Warnf("ResolveEffectiveUserTokens: impersonation of %q failed (err=%v, tokens=%d); using original tokens", email, err, len(tokens))
			return originalTokens
		}
		return tokens
	}

	return webserver.ApplyRoleSwitchOverride(originalTokens, ptr(override))
}

// ResolveEffectiveEmail returns the email the actor is acting AS. It equals the
// actor's own email unless an identity impersonation override is active, in which
// case it returns the impersonated identity's email (so email-scoped views follow
// the assumed user). Group overrides do not change the acting email.
func (s *Service) ResolveEffectiveEmail(actorEmail string) string {
	canonical := canonicalActorEmail(actorEmail)
	if canonical == "" {
		return actorEmail
	}
	if override := s.currentGroupOverrides()[canonical]; override != "" {
		if email, ok := strings.CutPrefix(override, userTokenPrefix); ok {
			return email
		}
	}
	return actorEmail
}

// ListAssumableIdentities returns the identities a root admin may impersonate via
// role switch. Used to populate the UI picker and to validate impersonation targets.
func (s *Service) ListAssumableIdentities() ([]common.Identity, error) {
	ctx, cancel := s.newCtx()
	defer cancel()
	return s.store.ListIdentities(ctx)
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

// SetUserImpersonationForActor makes the actor fully assume the given identity
// (see ResolveEffectiveUserTokens, identity mode). The target must be one of the
// known assumable identities. Stored in the same per-actor override slot, so it
// replaces any active group override and vice versa.
func (s *Service) SetUserImpersonationForActor(actorEmail, targetEmail string) error {
	normalizedActor := canonicalActorEmail(actorEmail)
	if normalizedActor == "" {
		return fmt.Errorf("actor email must not be empty")
	}
	target := canonicalActorEmail(targetEmail)
	if target == "" {
		return fmt.Errorf("impersonate_user must not be empty")
	}

	// Only a known identity may be assumed.
	ctx, cancel := s.newCtx()
	defer cancel()
	identities, err := s.store.ListIdentities(ctx)
	if err != nil {
		return fmt.Errorf("list identities: %w", err)
	}
	matched := ""
	for _, ident := range identities {
		if strings.EqualFold(ident.Email, target) {
			matched = ident.Email
			break
		}
	}
	if matched == "" {
		return fmt.Errorf("unknown identity: %s", targetEmail)
	}

	s.overridesWriteMu.Lock()
	defer s.overridesWriteMu.Unlock()

	current := s.currentGroupOverrides()
	next := cloneGroupOverrides(current, 1)
	next[normalizedActor] = userTokenPrefix + matched
	s.groupOverrides.Store(next)
	return nil
}

// ClearUserGroupSwitchForActor removes the temporary group override for one actor.
func (s *Service) ClearUserGroupSwitchForActor(actorEmail string) {
	normalizedActor := canonicalActorEmail(actorEmail)
	if normalizedActor == "" {
		return
	}

	s.overridesWriteMu.Lock()
	defer s.overridesWriteMu.Unlock()

	current := s.currentGroupOverrides()
	next := cloneGroupOverrides(current, 0)
	delete(next, normalizedActor)
	s.groupOverrides.Store(next)
}

// GetUserGroupSwitchForActor retrieves the temporary group override for one actor.
func (s *Service) GetUserGroupSwitchForActor(actorEmail string) *string {
	actor := canonicalActorEmail(actorEmail)
	if actor == "" {
		return nil
	}
	if override := s.currentGroupOverrides()[actor]; override != "" {
		return ptr(override)
	}
	return nil
}

// newCtx returns a context with the configured request deadline.
func (s *Service) newCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), s.requestTimeout)
}

// SearchGroupTokens returns matching group tokens via the role provider.
func (s *Service) SearchGroupTokens(query string, limit int) (common.TokenList, error) {
	ctx, cancel := s.newCtx()
	defer cancel()
	return s.roles.SearchGroupTokens(ctx, query, limit)
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

// normalizeOpenstackRoleOrEmpty validates OpenStack role input.
func normalizeOpenstackRoleOrEmpty(role string) string {
	r := strings.ToLower(strings.TrimSpace(role))
	if r == "" {
		return ""
	}
	return r
}

// normalizeAuthorizedUsers validates and normalizes authorization entries.
func normalizeAuthorizedUsers(users []common.AuthorizedUser) ([]common.AuthorizedUser, error) {
	out := make([]common.AuthorizedUser, 0, len(users))
	for i, user := range users {
		token := strings.TrimSpace(user.Token)
		if token == "" {
			return nil, fmt.Errorf("invalid authorized_users: entry %d has empty token", i)
		}

		openstackRole := normalizeOpenstackRoleOrEmpty(user.OpenstackRole)

		if openstackRole == "" {
			return nil, fmt.Errorf("invalid authorized_users: entry %d has invalid or missing openstack_role", i)
		}

		out = append(out, common.AuthorizedUser{
			Token:         token,
			OpenstackRole: openstackRole,
		})
	}
	return out, nil
}

// buildSubtreeParentMap performs a BFS from roots, building a map of every
// descendant ID → its parent ID. The roots themselves are included.
// This map is used both for usage rollup (upward walks) and bulk deletion.
func (s *Service) buildSubtreeParentMap(ctx context.Context, roots []common.Delegation) (map[string]*string, error) {
	parentMap := make(map[string]*string, len(roots))
	for _, d := range roots {
		parentMap[d.ID] = d.ParentID
	}

	queue := delegationIDs(roots)
	for len(queue) > 0 {
		children, err := s.store.ListDelegationsByParentIDs(ctx, queue, 0, 0)
		if err != nil {
			return nil, fmt.Errorf("load child delegations: %w", err)
		}
		queue = queue[:0]
		for _, child := range children {
			if _, seen := parentMap[child.ID]; seen {
				continue
			}
			parentMap[child.ID] = child.ParentID
			queue = append(queue, child.ID)
		}
	}
	return parentMap, nil
}

// attachUsage computes per-status usage for each delegation (rolling up descendants)
// and returns a new slice with usage attached.
func (s *Service) attachUsage(ctx context.Context, delegations []common.Delegation) ([]common.Delegation, error) {
	usageByDelegation, err := s.loadSubtreeUsage(ctx, delegations)
	if err != nil {
		return nil, fmt.Errorf("compute subtree usage: %w", err)
	}
	out := make([]common.Delegation, 0, len(delegations))
	for _, d := range delegations {
		out = append(out, withUsage(d, usageByDelegation))
	}
	return out, nil
}

// collectPoolAncestors returns all pool-strategy delegations in the ancestor chain of
// startDelegation, including startDelegation itself if it is a pool. The slice is ordered
// from closest to furthest ancestor so callers can report violations at the right level.
func (s *Service) collectPoolAncestors(ctx context.Context, startDelegation *common.Delegation) ([]common.Delegation, error) {
	if startDelegation == nil {
		return nil, nil
	}
	var pools []common.Delegation
	if startDelegation.DelegationStrategy == common.DelegationStrategyPool {
		pools = append(pools, *startDelegation)
	}
	ancestorID := startDelegation.ParentID
	for ancestorID != nil {
		ancestor, err := s.store.GetDelegationByID(ctx, *ancestorID)
		if err != nil {
			return nil, fmt.Errorf("load ancestor delegation: %w", err)
		}
		if ancestor == nil {
			break
		}
		if ancestor.DelegationStrategy == common.DelegationStrategyPool {
			pools = append(pools, *ancestor)
		}
		ancestorID = ancestor.ParentID
	}
	return pools, nil
}

// checkPoolCapacity verifies that adding addQuota to the current committed usage of each
// pool delegation in poolAncestors stays within its limit.
//
// subtractQuota is subtracted from the committed total before checking — used when a
// change_pending project is being re-approved and its existing resources are already
// counted in the active set and must not be double-counted.
//
// Returns nil if all checks pass, or an error identifying the first exceeded delegation.
func (s *Service) checkPoolCapacity(ctx context.Context, poolAncestors []common.Delegation, addQuota common.ProjectQuota, subtractQuota common.ProjectQuota) error {
	if len(poolAncestors) == 0 {
		return nil
	}
	subtreeUsage, err := s.loadSubtreeUsage(ctx, poolAncestors)
	if err != nil {
		return fmt.Errorf("compute subtree usage for capacity check: %w", err)
	}
	for _, ancestor := range poolAncestors {
		committed := subtreeUsage[ancestor.ID].TotalQuota(s.quotaResourceIDs)
		for _, resourceID := range s.quotaResourceIDs {
			committed[resourceID] -= subtractQuota[resourceID]
		}
		needed := quotaAdd(committed, addQuota, s.quotaResourceIDs)
		if !quotaFits(needed, ancestor.Quota.Limit, s.quotaResourceIDs) {
			return fmt.Errorf("delegation %q capacity exceeded", ancestor.ID)
		}
	}
	return nil
}

// quotaAdd sums configured resource types from two quota objects.
func quotaAdd(a common.ProjectQuota, b common.ProjectQuota, resourceIDs []string) common.ProjectQuota {
	out := make(common.ProjectQuota, len(a)+len(resourceIDs))
	maps.Copy(out, a)
	for _, resourceID := range resourceIDs {
		out[resourceID] = a[resourceID] + b[resourceID]
	}
	return out
}

// quotaFits checks whether requested quota stays within configured resource limits.
// A limit of common.UnlimitedQuota (-1) means no cap for that resource.
func quotaFits(need common.ProjectQuota, limit common.ProjectQuota, resourceIDs []string) bool {
	for _, resourceID := range resourceIDs {
		if limit[resourceID] == common.UnlimitedQuota {
			continue
		}
		if need[resourceID] > limit[resourceID] {
			return false
		}
	}
	return true
}

// loadSubtreeUsage computes resource consumption for each delegation in rootDelegations,
// correctly accounting for resources consumed by child delegations further down the tree.
//
// The delegation graph is a DAG where each node may have child delegations that consume
// resources from their parent's pool. A naive implementation that only sums direct projects
// would undercount a parent's usage — this function solves that by rolling up consumption
// from the full subtree rooted at each delegation.
//
// It operates in three phases:
//  1. BFS to discover the full subtree (via buildSubtreeParentMap)
//  2. A single batched load of all active projects across the subtree
//  3. Delegation to buildRolledUpUsage for a per-project upward attribution walk
func (s *Service) loadSubtreeUsage(ctx context.Context, rootDelegations []common.Delegation) (common.UsagePerDelegation, error) {
	if len(rootDelegations) == 0 {
		return make(common.UsagePerDelegation), nil
	}

	// Phase 1: BFS — discover the complete subtree.
	parentMap, err := s.buildSubtreeParentMap(ctx, rootDelegations)
	if err != nil {
		return nil, err
	}

	// Phase 2: load all active projects across the entire subtree in one batched query.
	allIDs := make([]string, 0, len(parentMap))
	for id := range parentMap {
		allIDs = append(allIDs, id)
	}
	projects, err := s.store.GetProjectsByFundedByIDs(ctx, allIDs, common.ActiveProjectStatuses, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("load active projects for usage rollup: %w", err)
	}

	// Phase 3: attribute each project's quota upward through the delegation hierarchy.
	return buildRolledUpUsage(projects, parentMap, s.quotaResourceIDs), nil
}

// buildRolledUpUsage attributes each project's quota and ID to its funding delegation and
// every tracked ancestor, so that a parent delegation's usage reflects total consumption
// across its entire subtree — not just projects it directly funded.
//
// Example with a three-level hierarchy:
//
//	group:root_uni
//	└── dept_cs_admin  (pool, 30 cores)
//	    └── dept_cs_students  (allowance, 2 cores per user)
//
// A student project funded by dept_cs_students (2 cores) is counted in:
//   - dept_cs_students: 2 cores consumed directly
//   - dept_cs_admin:    2 cores consumed by its subtree
//   - group:root_uni:   2 cores consumed by its subtree
//
// This means dept_cs_admin correctly shows 2/30 cores used even though it did not directly
// fund the project. Without rollup it would show 0.
//
// parentMap is produced by buildSubtreeParentMap and encodes both the upward navigation path
// (each ID maps to its parent ID) and the subtree boundary (IDs absent from the map are
// outside the tracked scope and the walk stops there).
func buildRolledUpUsage(projects []common.Project, parentMap map[string]*string, resourceIDs []string) common.UsagePerDelegation {
	result := make(common.UsagePerDelegation)

	for _, proj := range projects {
		if proj.FundedBy == nil {
			continue
		}

		// Start at the direct funder and climb through every tracked ancestor.
		// Each delegation on the path receives a copy of this project's quota and ID,
		// accumulating the full subtree view at every level.
		current := *proj.FundedBy
		for {
			if _, tracked := parentMap[current]; !tracked {
				break // stepped outside the tracked subtree — stop climbing
			}
			if result[current] == nil {
				result[current] = make(common.UsageByStatus)
			}

			// Accumulate quota and record the project ID under its status bucket.
			// The read-modify-write pattern is required because StatusUsage is a value
			// type (not a pointer): map entries in Go are not directly addressable.
			entry := result[current][proj.Status]
			entry.Quota = quotaAdd(entry.Quota, proj.Quota, resourceIDs)
			entry.ProjectIDs = append(entry.ProjectIDs, proj.ID)
			result[current][proj.Status] = entry

			parent := parentMap[current]
			if parent == nil {
				break // reached the root of the tracked subtree — stop climbing
			}
			current = *parent
		}
	}
	return result
}

// delegationIDs extracts delegation IDs for batched store lookups.
func delegationIDs(groups []common.Delegation) []string {
	ids := make([]string, 0, len(groups))
	for _, group := range groups {
		ids = append(ids, group.ID)
	}
	return ids
}

// withUsage attaches computed per-status usage data to a delegation response.
func withUsage(group common.Delegation, usageByDelegation common.UsagePerDelegation) common.Delegation {
	out := group
	out.Quota.UsageByStatus = usageByDelegation[group.ID]
	return out
}
