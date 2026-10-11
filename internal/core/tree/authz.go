package tree

import (
	"context"
	"fmt"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

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
