package tree

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

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
	if err := s.ensureBootstrapNodes(ctx); err != nil {
		return err
	}
	return s.canonicalizeStoredTokens(ctx)
}

// canonicalizeStoredTokens rewrites tokens stored before every token was kept
// in one spelling (common.CanonicalToken) — so a lookup in canonical spelling
// finds them. A no-op once the data is clean, so it runs on every start instead
// of being tracked as a one-off; the tree is hundreds of nodes, not millions.
func (s *Service) canonicalizeStoredTokens(ctx context.Context) error {
	nodes, err := s.store.ListNodes(ctx, NodeQuery{}, 0, 0)
	if err != nil {
		return fmt.Errorf("load nodes to canonicalize tokens: %w", err)
	}
	rewritten := 0
	for _, n := range nodes {
		if !canonicalizeNodeTokens(&n) {
			continue
		}
		wrote, err := s.store.UpdateNode(ctx, n.ID, func(stored *Node) error {
			if !canonicalizeNodeTokens(stored) {
				return ErrSkipUpdate
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("canonicalize tokens of %s: %w", n.ID, err)
		}
		if wrote {
			rewritten++
		}
	}
	if rewritten > 0 {
		s.log.Infow("rewrote stored tokens into canonical spelling", "nodes", rewritten)
	}
	return nil
}

// canonicalizeNodeTokens puts every token on the node into canonical spelling
// and reports whether anything changed.
func canonicalizeNodeTokens(n *Node) bool {
	changed := false
	list := func(tokens common.TokenList) common.TokenList {
		out := common.CanonicalTokens(tokens)
		if !slices.Equal(out, tokens) {
			changed = true
			return out
		}
		return tokens
	}
	// A new slice rather than an edit in place: the entries may be shared with
	// whatever handed the node over.
	users := func(entries []common.AuthorizedUser) []common.AuthorizedUser {
		out := slices.Clone(entries)
		for i := range out {
			out[i].Token = common.CanonicalToken(out[i].Token)
		}
		if !slices.Equal(out, entries) {
			changed = true
			return out
		}
		return entries
	}
	if c := common.CanonicalToken(n.Owner); c != n.Owner {
		n.Owner = c
		changed = true
	}
	n.AdminScope = list(n.AdminScope)
	n.EligibleRequesters = list(n.EligibleRequesters)
	n.AuthorizedUsers = users(n.AuthorizedUsers)
	if n.Pending != nil && n.Pending.AuthorizedUsers != nil {
		if pending := users(*n.Pending.AuthorizedUsers); changed {
			copied := *n.Pending
			copied.AuthorizedUsers = &pending
			n.Pending = &copied
		}
	}
	return changed
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
		// The root holds everything there is: no cap on any quantity, and every
		// availability granted. UnlimitedQuota would be rejected on an
		// availability (it is 0 or 1), and it would mean nothing anyway — the
		// root is where the full catalogue is visible and delegated from.
		limit := make(common.ProjectQuota, len(s.resources()))
		for _, r := range s.resources() {
			if r.IsBool() {
				if !r.Withdrawn {
					limit[r.ID] = 1
				}
				continue
			}
			limit[r.ID] = common.UnlimitedQuota
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
	} else {
		updated := *root
		changed := false

		if !tokenListsEqual(root.AdminScope, s.rootAdminTokens) {
			updated.AdminScope = append(common.TokenList{}, s.rootAdminTokens...)
			changed = true
			s.log.Infow("synchronized root admin scope from configuration",
				"previous", root.AdminScope, "current", updated.AdminScope)
		}

		if added := s.adoptNewCatalogueResources(&updated); len(added) > 0 {
			changed = true
			s.log.Infow("root adopted resources new to the catalogue", "resources", added)
		}

		if changed {
			if err := s.store.UpsertNode(ctx, updated); err != nil {
				return fmt.Errorf("sync root node: %w", err)
			}
		}
	}

	unassigned, err := s.store.GetNode(ctx, UnassignedNodeID)
	if err != nil {
		return fmt.Errorf("check unassigned node: %w", err)
	}
	if unassigned == nil {
		// Zero for quantities and withheld for availabilities — same value, and
		// both say the same thing here: nothing can be approved under this node.
		zeroLimit := make(common.ProjectQuota, len(s.resources()))
		for _, r := range s.resources() {
			zeroLimit[r.ID] = 0
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

// nodeChain returns the chain of nodes starting at startID (inclusive) walking up
// to the root. Guards against cycles.
// adoptNewCatalogueResources gives the root whatever the catalogue has gained
// since the root was created, and returns what it added.
//
// Without it a resource added to a running deployment is unreachable. The root
// is where delegation starts and every edge is bounded by its parent, so a
// resource the root does not hold cannot be granted to anyone — the catalogue
// would list it, the portal would show it at the root, and every attempt to pass
// it down would be refused. The root's limit is only written when the root is
// CREATED, which on any existing tree happened long ago.
//
// It only ADDS keys. An operator who capped the root at a hundred cores meant it,
// and this is not the place to overrule them.
func (s *Service) adoptNewCatalogueResources(root *Node) []string {
	if root.Limit == nil {
		root.Limit = common.ProjectQuota{}
	}
	var added []string
	for _, r := range s.resources() {
		if _, present := root.Limit[r.ID]; present || r.Withdrawn {
			continue
		}
		if r.IsBool() {
			root.Limit[r.ID] = 1
		} else {
			root.Limit[r.ID] = common.UnlimitedQuota
		}
		added = append(added, r.ID)
	}
	return added
}
