package tree

import (
	"context"
	"fmt"
	"maps"
	"net/mail"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/identity"
	"go.uber.org/zap"
)

// Service implements the tree domain: one authorization rule (ancestor walk), one
// capacity mechanism (subtree usage rollup), one lifecycle for budgets and leaves.
// The embedded identity.Service contributes the model-agnostic role-switch and
// impersonation operations, so one object satisfies the whole HTTP service interface.
type Service struct {
	*identity.Service

	store           Store
	roles           common.RoleProvider
	resourceIDs     []string
	rootAdminTokens common.TokenList
	requestTimeout  time.Duration
	// maxAuthorizedUsers bounds how many participants one node may list.
	maxAuthorizedUsers int

	// approvalMu serializes the capacity check-then-write critical sections
	// (create with direct/auto approval, approve, reparent, promote) so concurrent
	// approvals cannot both pass the capacity check and over-allocate a budget.
	// Process-level lock — correct for the single-replica deployment; a
	// multi-replica setup would need DB-level row locking instead.
	approvalMu sync.Mutex

	log *zap.SugaredLogger
}

// NewService constructs the tree service.
// rootAdminTokens is synchronized into the root node's AdminScope on every
// Bootstrap — the configuration is the source of truth for that one scope.
func NewService(store Store, roles common.RoleProvider, resourceIDs []string, rootAdminTokens common.TokenList, requestTimeout time.Duration, maxAuthorizedUsers int, log *zap.SugaredLogger) *Service {
	if store == nil {
		panic("tree.NewService requires a non-nil store")
	}
	if len(resourceIDs) == 0 {
		panic("tree.NewService requires at least one configured resource type")
	}
	if requestTimeout <= 0 {
		requestTimeout = 30 * time.Second
	}
	if maxAuthorizedUsers <= 0 {
		maxAuthorizedUsers = common.DefaultMaxAuthorizedUsers
	}
	return &Service{
		Service:            identity.NewService(store, roles, requestTimeout, log),
		store:              store,
		roles:              roles,
		resourceIDs:        resourceIDs,
		rootAdminTokens:    rootAdminTokens,
		requestTimeout:     requestTimeout,
		maxAuthorizedUsers: maxAuthorizedUsers,
		log:                log,
	}
}

// ── Bootstrap ─────────────────────────────────────────────────────────────────

// Bootstrap prepares storage: optionally seeds mock data into an empty store,
// then ensures the structural nodes exist. Safe to run on every startup.
func (s *Service) Bootstrap(ctx context.Context, mockIdentities []common.Identity, mockNodes []Node) error {
	if len(mockNodes) > 0 {
		empty, err := s.store.IsEmpty(ctx)
		if err != nil {
			return fmt.Errorf("tree service: check state emptiness: %w", err)
		}
		if empty {
			if err := s.store.Seed(ctx, mockIdentities, mockNodes); err != nil {
				return fmt.Errorf("tree service: seed mock state: %w", err)
			}
			s.log.Infow("seeded mock tree state", "nodes", len(mockNodes))
		}
	}
	return s.ensureBootstrapNodes(ctx)
}

// ensureBootstrapNodes guarantees the two structural nodes:
//
//   - "root": the single parentless budget with an unlimited limit. Its AdminScope
//     is SYNCHRONIZED from ROOT_ADMIN_TOKENS on every startup, so admin changes in
//     the configuration take effect on the next deployment (the old model only set
//     the scope on first creation, letting it drift). Consequence: the root scope
//     is not editable via the API — the config owns it.
//   - "unassigned": the collection point for reconciler-imported leaves, directly
//     under root. Its all-zero limit makes approving anything under it impossible;
//     imported leaves must be promoted into a real budget.
func (s *Service) ensureBootstrapNodes(ctx context.Context) error {
	if len(s.rootAdminTokens) == 0 {
		s.log.Warn("no ROOT_ADMIN_TOKENS configured — the root node will have an empty admin scope and no one can manage top-level budgets")
	}

	now := time.Now().UTC().Format(time.RFC3339)

	root, err := s.store.GetNode(ctx, RootNodeID)
	if err != nil {
		return fmt.Errorf("check root node: %w", err)
	}
	if root == nil {
		limit := make(common.ProjectQuota, len(s.resourceIDs))
		for _, id := range s.resourceIDs {
			limit[id] = common.UnlimitedQuota
		}
		rootNode := Node{
			ID:         RootNodeID,
			Kind:       KindBudget,
			ParentID:   nil,
			Status:     StatusApproved,
			Name:       "Organization Root",
			Limit:      limit,
			AdminScope: append(common.TokenList{}, s.rootAdminTokens...),
			CreatedBy:  "System",
			CreatedAt:  now,
		}
		if err := s.store.UpsertNode(ctx, rootNode); err != nil {
			return fmt.Errorf("bootstrap root node: %w", err)
		}
		s.log.Infow("bootstrapped root node", "admin_scope", rootNode.AdminScope)
	} else if !tokenListsEqual(root.AdminScope, s.rootAdminTokens) {
		updated := *root
		updated.AdminScope = append(common.TokenList{}, s.rootAdminTokens...)
		if err := s.store.UpsertNode(ctx, updated); err != nil {
			return fmt.Errorf("sync root admin scope: %w", err)
		}
		s.log.Infow("synchronized root admin scope from configuration",
			"previous", root.AdminScope, "current", updated.AdminScope)
	}

	unassigned, err := s.store.GetNode(ctx, UnassignedNodeID)
	if err != nil {
		return fmt.Errorf("check unassigned node: %w", err)
	}
	if unassigned == nil {
		zeroLimit := make(common.ProjectQuota, len(s.resourceIDs))
		for _, id := range s.resourceIDs {
			zeroLimit[id] = 0
		}
		parent := RootNodeID
		unassignedNode := Node{
			ID:       UnassignedNodeID,
			Kind:     KindBudget,
			ParentID: &parent,
			Status:   StatusApproved,
			Name:     "Unassigned OpenStack Imports",
			Reason:   "Collection point for OpenStack projects discovered by the reconciler. Limit 0: nothing can be approved here — promote imports into a real budget.",
			Limit:    zeroLimit,
			// No own admin scope needed: root admins manage it via the ancestor rule.
			CreatedBy: "System",
			CreatedAt: now,
		}
		if err := s.store.UpsertNode(ctx, unassignedNode); err != nil {
			return fmt.Errorf("bootstrap unassigned node: %w", err)
		}
		s.log.Info("bootstrapped unassigned node for reconciler imports")
	}

	return nil
}

// ── Authorization ─────────────────────────────────────────────────────────────
// One rule: managing authority flows down the ancestor chain via AdminScope.

// nodeChain returns the chain of nodes starting at startID (inclusive) walking up
// to the root. Guards against cycles.
func (s *Service) nodeChain(ctx context.Context, startID string) ([]Node, error) {
	var chain []Node
	seen := map[string]struct{}{}
	id := &startID
	for id != nil {
		if _, dup := seen[*id]; dup {
			return nil, fmt.Errorf("cycle detected in node chain at %q", *id)
		}
		seen[*id] = struct{}{}
		node, err := s.store.GetNode(ctx, *id)
		if err != nil {
			return nil, fmt.Errorf("load node for chain walk: %w", err)
		}
		if node == nil {
			break
		}
		chain = append(chain, *node)
		id = node.ParentID
	}
	return chain, nil
}

// managesNode reports whether the caller holds a token in the AdminScope of the
// node itself or ANY of its ancestors (inclusive walk). Root admins manage every
// node simply because "root" is an ancestor of everything.
func (s *Service) managesNode(ctx context.Context, userTokens common.TokenList, node *Node) (bool, error) {
	callerSet := common.NewTokenSet(userTokens)
	if callerSet.ContainsAny(node.AdminScope) {
		return true, nil
	}
	return s.managesChainFrom(ctx, callerSet, node.ParentID)
}

// managesParentChain reports whether the caller holds a token in the AdminScope of
// the node's parent or any higher ancestor (EXCLUSIVE walk — the node's own scope
// does not count). This is the approval authority: a budget's own managers must
// not approve their own budget's requests for more capacity.
func (s *Service) managesParentChain(ctx context.Context, userTokens common.TokenList, node *Node) (bool, error) {
	return s.managesChainFrom(ctx, common.NewTokenSet(userTokens), node.ParentID)
}

func (s *Service) managesChainFrom(ctx context.Context, callerSet common.TokenSet, startID *string) (bool, error) {
	if startID == nil {
		return false, nil
	}
	chain, err := s.nodeChain(ctx, *startID)
	if err != nil {
		return false, err
	}
	for _, n := range chain {
		if callerSet.ContainsAny(n.AdminScope) {
			return true, nil
		}
	}
	return false, nil
}

// isOwner reports whether the caller holds the leaf's owner token.
func isOwner(userTokens common.TokenList, node *Node) bool {
	return node.Owner != "" && common.NewTokenSet(userTokens).Contains(node.Owner)
}

// isEligibleRequester reports whether the caller may request children under node.
func isEligibleRequester(userTokens common.TokenList, node *Node) bool {
	return common.NewTokenSet(node.EligibleRequesters).ContainsAny(userTokens)
}

// isAuthorizedUser reports whether the caller appears in the leaf's authorized users.
func isAuthorizedUser(userTokens common.TokenList, node *Node) bool {
	set := common.NewTokenSet(userTokens)
	for _, au := range node.AuthorizedUsers {
		if set.Contains(au.Token) {
			return true
		}
	}
	return false
}

// ── Usage rollup ──────────────────────────────────────────────────────────────

// buildSubtreeParentMap performs a BFS over BUDGET nodes from the given roots,
// building a map of every subtree budget ID → its parent ID (roots included).
func (s *Service) buildSubtreeParentMap(ctx context.Context, roots []Node) (map[string]*string, error) {
	parentMap := make(map[string]*string, len(roots))
	queue := make([]string, 0, len(roots))
	for _, n := range roots {
		if _, seen := parentMap[n.ID]; seen {
			continue
		}
		parentMap[n.ID] = n.ParentID
		queue = append(queue, n.ID)
	}

	for len(queue) > 0 {
		children, err := s.store.ListNodes(ctx, NodeQuery{ParentIDs: queue, Kinds: []string{KindBudget}}, 0, 0)
		if err != nil {
			return nil, fmt.Errorf("load child budgets: %w", err)
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

// undelegatedBudgetIDs returns the roots plus every budget below them that has
// not been handed to anyone else — descending stops at a budget with an admin
// scope of its own that does not include the caller.
//
// This is the set of budgets the caller is personally responsible for. It is not
// just the roots, because a budget without an admin scope (the imports node, a
// purely structural grouping) has no manager and its children still belong to
// whoever manages the level above.
func (s *Service) undelegatedBudgetIDs(ctx context.Context, roots []Node, userTokens common.TokenList) ([]string, error) {
	seen := make(map[string]bool, len(roots))
	ids := make([]string, 0, len(roots))
	queue := make([]string, 0, len(roots))
	for _, r := range roots {
		if !seen[r.ID] {
			seen[r.ID] = true
			ids = append(ids, r.ID)
			queue = append(queue, r.ID)
		}
	}

	for len(queue) > 0 {
		children, err := s.store.ListNodes(ctx, NodeQuery{ParentIDs: queue, Kinds: []string{KindBudget}}, 0, 0)
		if err != nil {
			return nil, fmt.Errorf("load child budgets: %w", err)
		}
		queue = queue[:0]
		for _, child := range children {
			if seen[child.ID] {
				continue
			}
			if len(child.AdminScope) > 0 && !common.NewTokenSet(child.AdminScope).ContainsAny(userTokens) {
				continue // delegated — its requests are that manager's job
			}
			seen[child.ID] = true
			ids = append(ids, child.ID)
			queue = append(queue, child.ID)
		}
	}
	return ids, nil
}

// loadSubtreeUsage computes resource consumption for each budget in roots, rolling
// up the limits of all ACTIVE descendant leaves through the budget hierarchy.
//
// Three phases: (1) BFS discovers the budget subtree, (2) one batched load fetches
// all active leaves across the subtree, (3) each leaf's limit is attributed upward
// from its parent through every tracked ancestor.
func (s *Service) loadSubtreeUsage(ctx context.Context, roots []Node) (UsagePerNode, error) {
	if len(roots) == 0 {
		return make(UsagePerNode), nil
	}

	parentMap, err := s.buildSubtreeParentMap(ctx, roots)
	if err != nil {
		return nil, err
	}

	budgetIDs := make([]string, 0, len(parentMap))
	for id := range parentMap {
		budgetIDs = append(budgetIDs, id)
	}
	leaves, err := s.store.ListNodes(ctx, NodeQuery{
		ParentIDs: budgetIDs,
		Kinds:     []string{KindProject},
		Statuses:  ActiveStatuses,
	}, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("load active leaves for usage rollup: %w", err)
	}

	return buildRolledUpUsage(leaves, parentMap, s.resourceIDs), nil
}

// buildRolledUpUsage attributes each leaf's limit and ID to its parent budget and
// every tracked ancestor, so a budget's usage reflects total consumption across
// its entire subtree — not just its direct children.
func buildRolledUpUsage(leaves []Node, parentMap map[string]*string, resourceIDs []string) UsagePerNode {
	result := make(UsagePerNode)

	for _, leaf := range leaves {
		if leaf.ParentID == nil {
			continue
		}
		current := *leaf.ParentID
		for {
			if _, tracked := parentMap[current]; !tracked {
				break // outside the tracked subtree — stop climbing
			}
			if result[current] == nil {
				result[current] = make(UsageByStatus)
			}
			entry := result[current][leaf.Status]
			entry.Limit = quotaAdd(entry.Limit, leaf.Limit, resourceIDs)
			entry.NodeIDs = append(entry.NodeIDs, leaf.ID)
			result[current][leaf.Status] = entry

			parent := parentMap[current]
			if parent == nil {
				break
			}
			current = *parent
		}
	}
	return result
}

// attachUsage computes per-status subtree usage for the budget nodes in the list
// and returns a new slice with usage attached (leaves are passed through).
// attachChildCounts fills Node.ChildCount for every budget in the set. Leaves
// never have children, so they are skipped rather than queried.
func (s *Service) attachChildCounts(ctx context.Context, nodes []Node) ([]Node, error) {
	parentIDs := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n.Kind == KindBudget {
			parentIDs = append(parentIDs, n.ID)
		}
	}
	if len(parentIDs) == 0 {
		return nodes, nil
	}
	counts, err := s.store.CountChildren(ctx, parentIDs)
	if err != nil {
		return nil, fmt.Errorf("count children: %w", err)
	}
	for i := range nodes {
		if nodes[i].Kind == KindBudget {
			nodes[i].ChildCount = counts[nodes[i].ID]
		}
	}
	return nodes, nil
}

// attachParentNames fills Node.ParentName for every node that has a parent,
// loading all distinct parents in one query. Without it a client that shows
// "paid from <budget>" has to fetch every parent separately — one request per
// node. Only the display name is exposed; no other parent field is copied.
func (s *Service) attachParentNames(ctx context.Context, nodes []Node) ([]Node, error) {
	parentIDs := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n.ParentID != nil && !slices.Contains(parentIDs, *n.ParentID) {
			parentIDs = append(parentIDs, *n.ParentID)
		}
	}
	if len(parentIDs) == 0 {
		return nodes, nil
	}
	parents, err := s.store.ListNodes(ctx, NodeQuery{IDs: parentIDs}, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("load parents: %w", err)
	}
	names := make(map[string]string, len(parents))
	for _, p := range parents {
		names[p.ID] = p.Name
	}
	for i := range nodes {
		if nodes[i].ParentID != nil {
			nodes[i].ParentName = names[*nodes[i].ParentID]
		}
	}
	return nodes, nil
}

// attachUsage adds the derived fields every view needs on top of the stored
// node: the subtree usage rollup, the direct child count and the parent's name.
func (s *Service) attachUsage(ctx context.Context, nodes []Node) ([]Node, error) {
	budgets := make([]Node, 0, len(nodes))
	for _, n := range nodes {
		if n.Kind == KindBudget {
			budgets = append(budgets, n)
		}
	}
	usage, err := s.loadSubtreeUsage(ctx, budgets)
	if err != nil {
		return nil, fmt.Errorf("compute subtree usage: %w", err)
	}
	out := make([]Node, 0, len(nodes))
	for _, n := range nodes {
		if n.Kind == KindBudget {
			n.Usage = usage[n.ID]
		}
		out = append(out, n)
	}
	out, err = s.attachChildCounts(ctx, out)
	if err != nil {
		return nil, err
	}
	return s.attachParentNames(ctx, out)
}

// ── Capacity & limit validation ───────────────────────────────────────────────

// checkCapacity verifies that adding addLimit to the current committed usage of
// each budget in ancestors stays within its limit. subtractLimit is removed from
// the committed total first — used when a change_pending node's current resources
// are already in the active set and must not be double-counted.
func (s *Service) checkCapacity(ctx context.Context, ancestors []Node, addLimit, subtractLimit common.ProjectQuota) error {
	if len(ancestors) == 0 {
		return nil
	}
	subtreeUsage, err := s.loadSubtreeUsage(ctx, ancestors)
	if err != nil {
		return fmt.Errorf("compute subtree usage for capacity check: %w", err)
	}
	for _, ancestor := range ancestors {
		committed := subtreeUsage[ancestor.ID].Total(s.resourceIDs)
		for _, resourceID := range s.resourceIDs {
			committed[resourceID] -= subtractLimit[resourceID]
		}
		needed := quotaAdd(committed, addLimit, s.resourceIDs)
		if !quotaFits(needed, ancestor.Limit, s.resourceIDs) {
			return fmt.Errorf("budget %q (%s) capacity exceeded", ancestor.ID, ancestor.Name)
		}
	}
	return nil
}

// validateLeafLimit rejects a leaf limit that is negative, set on an unmanaged
// resource, or empty. A leaf limit is a concrete allocation with no "unlimited"
// sentinel: a negative value would lower the tracked usage and map to Nova's
// unlimited, letting a request grab unbounded resources.
func (s *Service) validateLeafLimit(q common.ProjectQuota) error {
	if err := s.validateKnownResources(q); err != nil {
		return err
	}
	for key, val := range q {
		if val < 0 {
			return fmt.Errorf("limit for %q must not be negative (got %d)", key, val)
		}
	}
	return nil
}

// validateBudgetLimit rejects unknown resources and negative values other than the
// UnlimitedQuota sentinel. Missing keys count as 0 — an intentionally hard cap.
func (s *Service) validateBudgetLimit(q common.ProjectQuota) error {
	if err := s.validateKnownResources(q); err != nil {
		return err
	}
	for key, val := range q {
		if val < 0 && val != common.UnlimitedQuota {
			return fmt.Errorf("limit for %q must be >= 0 or %d for unlimited (got %d)", key, common.UnlimitedQuota, val)
		}
	}
	return nil
}

func (s *Service) validateKnownResources(q common.ProjectQuota) error {
	known := make(map[string]struct{}, len(s.resourceIDs))
	for _, id := range s.resourceIDs {
		known[id] = struct{}{}
	}
	for key := range q {
		if _, ok := known[key]; !ok {
			return fmt.Errorf("unknown resource %q", key)
		}
	}
	return nil
}

// validateChildBudgetLimit enforces the static invariant that a child budget's
// limit cannot exceed its direct parent's limit for any resource (and cannot be
// unlimited under a limited parent). Enforced on every edge — create, approve,
// direct limit edit and reparent — so it holds inductively across the tree.
func (s *Service) validateChildBudgetLimit(parent *Node, childLimit common.ProjectQuota) error {
	for _, id := range s.resourceIDs {
		parentCap := parent.Limit[id]
		childCap := childLimit[id]
		if parentCap == common.UnlimitedQuota {
			continue
		}
		if childCap == common.UnlimitedQuota {
			return fmt.Errorf("child limit for %q cannot be unlimited when parent limit is %d", id, parentCap)
		}
		if childCap > parentCap {
			return fmt.Errorf("child limit for %q (%d) exceeds parent limit (%d)", id, childCap, parentCap)
		}
	}
	return nil
}

// validateAutoApprove checks the per-requester limit of an auto-approve policy.
func (s *Service) validateAutoApprove(a *AutoApprove) error {
	if a == nil {
		return nil
	}
	if len(a.PerRequesterLimit) == 0 {
		return fmt.Errorf("auto_approve requires a non-empty per_requester_limit")
	}
	return s.validateLeafLimit(a.PerRequesterLimit)
}

// ownerActiveUsage sums the owner's committed (active) leaf limits directly under
// the given budget, so auto-approval enforces a cumulative per-requester cap.
// Matching is by the single Owner token — group memberships do not blur the count.
func (s *Service) ownerActiveUsage(ctx context.Context, budgetID string, ownerToken string) (common.ProjectQuota, error) {
	leaves, err := s.store.ListNodes(ctx, NodeQuery{
		ParentIDs: []string{budgetID},
		Kinds:     []string{KindProject},
		Statuses:  ActiveStatuses,
		Owner:     ownerToken,
	}, 0, 0)
	if err != nil {
		return nil, err
	}
	usage := make(common.ProjectQuota, len(s.resourceIDs))
	for _, leaf := range leaves {
		for _, id := range s.resourceIDs {
			usage[id] += leaf.Limit[id]
		}
	}
	return usage, nil
}

// ── Small helpers ─────────────────────────────────────────────────────────────

// newCtx returns a context with the configured request deadline.
func (s *Service) newCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), s.requestTimeout)
}

// newHistoryEntry creates a HistoryEntry with the current timestamp.
func newHistoryEntry(event, actor, statusTo string) HistoryEntry {
	return HistoryEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Event:     event,
		Actor:     actor,
		StatusTo:  statusTo,
	}
}

// quotaAdd sums configured resource types from two quota objects.
func quotaAdd(a, b common.ProjectQuota, resourceIDs []string) common.ProjectQuota {
	out := make(common.ProjectQuota, len(a)+len(resourceIDs))
	maps.Copy(out, a)
	for _, resourceID := range resourceIDs {
		out[resourceID] = a[resourceID] + b[resourceID]
	}
	return out
}

// quotaFits checks whether the needed quota stays within the given limits.
// A limit of common.UnlimitedQuota (-1) means no cap for that resource.
func quotaFits(need, limit common.ProjectQuota, resourceIDs []string) bool {
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

// quotaNeverGrows reports whether no configured resource is higher in `to` than
// in `from` — i.e. the change gives capacity back or leaves it alone.
//
// The UnlimitedQuota sentinel (-1) is the LARGEST value, not the smallest, so a
// plain numeric comparison inverts both unlimited cases: unlimited → concrete is
// a REDUCTION, concrete → unlimited an increase. quotaFits already reads an
// unlimited *limit* as "no cap", which covers the first case; only an unlimited
// *new* value has to be ruled out separately.
func quotaNeverGrows(from, to common.ProjectQuota, resourceIDs []string) bool {
	for _, resourceID := range resourceIDs {
		if to[resourceID] == common.UnlimitedQuota && from[resourceID] != common.UnlimitedQuota {
			return false
		}
	}
	return quotaFits(to, from, resourceIDs)
}

// terminationDateNeverGrows reports whether `proposed` does not extend the node's
// life beyond `current`. An earlier date counts as a reduction: it hands the
// capacity back sooner and can only shrink the claim on the parent budget. A nil
// current date means "runs until somebody says otherwise", so naming any concrete
// day shortens it. A date neither side can parse is treated as an extension —
// when in doubt a manager decides.
func terminationDateNeverGrows(current *string, proposed string) bool {
	proposedAt, ok := parseTerminationDate(proposed)
	if !ok {
		return false
	}
	if current == nil {
		return true
	}
	currentAt, ok := parseTerminationDate(*current)
	if !ok {
		return false
	}
	return !proposedAt.After(currentAt)
}

// parseTerminationDate accepts the two shapes that reach the API: the RFC3339
// timestamp the model stores and the bare calendar day a date picker sends.
func parseTerminationDate(value string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339, time.DateOnly} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

// tokenListsEqual compares two token lists as sets.
func tokenListsEqual(a, b common.TokenList) bool {
	if len(a) != len(b) {
		return false
	}
	setA := common.NewTokenSet(a)
	for _, t := range b {
		if !setA.Contains(t) {
			return false
		}
	}
	return true
}

// normalizeOwnerToken accepts "email" or "user:email" and returns "user:email".
func normalizeOwnerToken(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	v = strings.TrimPrefix(v, "user:")
	if v == "" || strings.Contains(v, ":") {
		return "", fmt.Errorf("owner must be a plain email or user: token")
	}
	return "user:" + strings.ToLower(v), nil
}

// normalizeAuthorizedUsers validates and normalizes authorization entries.
//
// Every entry here has consequences in OpenStack: the reconciler creates a
// Keystone group for each group token, resolves its members through the role
// provider and CREATES an account for each of them. An unchecked token therefore
// let any requester materialize identities for people who never used the system,
// and a typo left an empty group behind on every run. So a token must name
// something that exists, and the list has a ceiling.
func (s *Service) normalizeAuthorizedUsers(ctx context.Context, users []common.AuthorizedUser) ([]common.AuthorizedUser, error) {
	if len(users) > s.maxAuthorizedUsers {
		return nil, fmt.Errorf("at most %d authorized users — use a group token to grant access to a whole course or department", s.maxAuthorizedUsers)
	}

	out := make([]common.AuthorizedUser, 0, len(users))
	seenGroups := make(map[string]bool, len(users))

	for i, user := range users {
		token := strings.TrimSpace(user.Token)
		if token == "" {
			return nil, fmt.Errorf("invalid authorized_users: entry %d has empty token", i)
		}

		role := strings.ToLower(strings.TrimSpace(user.OpenstackRole))
		if !slices.Contains(common.OpenstackRoles, role) {
			return nil, fmt.Errorf("invalid authorized_users: entry %d has role %q, expected one of %v", i, user.OpenstackRole, common.OpenstackRoles)
		}

		switch {
		case strings.HasPrefix(token, common.UserPrefix):
			email := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(token, common.UserPrefix)))
			if _, err := mail.ParseAddress(email); err != nil {
				return nil, fmt.Errorf("invalid authorized_users: entry %d is not a valid email address", i)
			}
			token = common.UserPrefix + email

		case strings.HasPrefix(token, common.GroupPrefix):
			if !seenGroups[token] {
				exists, err := s.groupTokenExists(ctx, token)
				if err != nil {
					// Fail closed. The group directory is also what resolves the
					// caller's own tokens, so if it is unreachable nothing else
					// works either — better an honest error than a node whose
					// members were never checked.
					return nil, fmt.Errorf("cannot verify group %q right now: %w", token, err)
				}
				if !exists {
					return nil, fmt.Errorf("invalid authorized_users: entry %d references unknown group %q", i, token)
				}
				seenGroups[token] = true
			}

		default:
			return nil, fmt.Errorf("invalid authorized_users: entry %d must be a %s or %s token", i, common.UserPrefix, common.GroupPrefix)
		}

		out = append(out, common.AuthorizedUser{Token: token, OpenstackRole: role})
	}
	return out, nil
}

// groupTokenExists asks the role provider whether a group token is real. The
// search matches substrings, so the exact token still has to be picked out of
// the results.
func (s *Service) groupTokenExists(ctx context.Context, token string) (bool, error) {
	if s.roles == nil {
		return false, fmt.Errorf("no role provider configured")
	}
	groups, err := s.roles.SearchGroups(ctx, token, common.DefaultPageLimit)
	if err != nil {
		return false, err
	}
	for _, g := range groups {
		if strings.EqualFold(g.Token, token) {
			return true, nil
		}
	}
	return false, nil
}

// ptr returns a pointer to the provided value for inline literals.
func ptr[T any](v T) *T {
	return &v
}
