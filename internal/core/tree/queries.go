package tree

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
)

// GetNode fetches a single node, enforcing read authorization: the leaf owner, an
// authorized user, a manager of the node or its ancestors, or an eligible
// requester of the budget may read it. Returns (nil, nil) when not found.
func (s *Service) GetNode(id string, userTokens common.TokenList) (*model.Node, error) {
	if len(userTokens) == 0 {
		return nil, common.ErrForbidden
	}
	ctx, cancel := s.newCtx()
	defer cancel()

	node, err := s.store.GetNode(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("load node: %w", err)
	}
	if node == nil {
		return nil, nil
	}

	allowed := isOwner(userTokens, node) ||
		isAuthorizedUser(userTokens, node) ||
		isEligibleRequester(userTokens, node)
	if !allowed {
		manages, err := s.managesNode(ctx, userTokens, node)
		if err != nil {
			return nil, err
		}
		allowed = manages
	}
	if !allowed {
		return nil, common.ErrForbidden
	}

	withUsage, err := s.attachUsage(ctx, []model.Node{*node}, userTokens)
	if err != nil {
		return nil, err
	}
	return &withUsage[0], nil
}

// ListMine returns the leaves the user answers for: the ones they own, and the
// ones whose admin scope names them or one of their groups.
func (s *Service) ListMine(userEmail string, userTokens common.TokenList, limit, offset int) (NodePage, error) {
	if strings.TrimSpace(userEmail) == "" {
		return NodePage{}, fmt.Errorf("missing user email")
	}
	limit, offset = normalizePagination(limit, offset)
	ctx, cancel := s.newCtx()
	defer cancel()

	owner, err := normalizeOwnerToken(userEmail)
	if err != nil {
		return NodePage{}, err
	}
	return s.listPage(ctx, NodeQuery{
		Kinds:       []string{model.KindProject},
		Responsible: &ResponsibleQuery{Owner: owner, AdminAny: userTokens},
	}, limit, offset, append(slices.Clone(userTokens), owner))
}

// listPage runs one query twice — the page itself and the number of rows it was
// cut from — and decorates only the page. Counting is a separate query on
// purpose: it must not be the length of the page, or a full page would always
// claim to be complete.
func (s *Service) listPage(ctx context.Context, q NodeQuery, limit, offset int, viewer common.TokenList) (NodePage, error) {
	nodes, err := s.store.ListNodes(ctx, q, limit, offset)
	if err != nil {
		return NodePage{}, fmt.Errorf("load nodes: %w", err)
	}
	total, err := s.store.CountNodes(ctx, q)
	if err != nil {
		return NodePage{}, fmt.Errorf("count nodes: %w", err)
	}
	decorated, err := s.attachUsage(ctx, nodes, viewer)
	if err != nil {
		return NodePage{}, err
	}
	return newNodePage(decorated, total, limit, offset), nil
}

// ListMyBudgets returns the budgets whose AdminScope directly contains one of the
// caller's tokens — "budgets delegated to me". Children of these are navigated
// via ListChildren.
func (s *Service) ListMyBudgets(userTokens common.TokenList, limit, offset int) (NodePage, error) {
	if len(userTokens) == 0 {
		return NodePage{}, fmt.Errorf("no user tokens found")
	}
	limit, offset = normalizePagination(limit, offset)
	ctx, cancel := s.newCtx()
	defer cancel()

	page, err := s.listPage(ctx, NodeQuery{
		Kinds:         []string{model.KindBudget},
		AdminScopeAny: userTokens,
	}, limit, offset, userTokens)
	if err != nil {
		return NodePage{}, err
	}

	// Only here, and not in listPage: this is the one listing whose caller has
	// to work out which entries are the TOP-most ones it manages, and the answer
	// needs the nodes in between — which are not in the list, because they are
	// exactly the ones the caller does not manage. Every other listing knows
	// where its nodes belong from ParentID alone.
	page.Items, err = s.attachAncestorIDs(ctx, page.Items)
	if err != nil {
		return NodePage{}, err
	}
	return page, nil
}

// ListEligibleForMe returns the approved budgets the caller may submit requests to.
func (s *Service) ListEligibleForMe(userTokens common.TokenList, limit, offset int) (NodePage, error) {
	if len(userTokens) == 0 {
		return NodePage{}, fmt.Errorf("no user tokens found")
	}
	limit, offset = normalizePagination(limit, offset)
	ctx, cancel := s.newCtx()
	defer cancel()

	return s.listPage(ctx, NodeQuery{
		Kinds:       []string{model.KindBudget},
		Statuses:    []string{model.StatusApproved},
		EligibleAny: userTokens,
	}, limit, offset, userTokens)
}

// ListEligibleForOwner returns the approved budgets the given owner tokens may
// request under. Root-admin only — used by the promote flow so the admin can see
// which budgets make sense as a promotion target for a specific owner.
func (s *Service) ListEligibleForOwner(callerTokens common.TokenList, ownerTokens common.TokenList, limit, offset int) (NodePage, error) {
	if !common.NewTokenSet(s.rootAdminTokens).ContainsAny(callerTokens) {
		return NodePage{}, common.ErrForbidden
	}
	if len(ownerTokens) == 0 {
		return NodePage{}, fmt.Errorf("owner_tokens must not be empty")
	}
	return s.ListEligibleForMe(common.CanonicalTokens(ownerTokens), limit, offset)
}

// ListToManage returns the nodes awaiting a decision by the caller: pending and
// change_pending nodes (budgets AND leaves) plus imported leaves, hanging under
// the budgets the caller directly administers.
//
// includeSubtree widens that to the whole subtree below those budgets. Off by
// default, because a request under a delegated sub-budget is that manager's job:
// for a root admin the wide list is the entire organization, which drowns the
// handful of requests actually addressed to them. On it answers the other
// question — "is anything stuck anywhere below me?".
func (s *Service) ListToManage(userTokens common.TokenList, includeSubtree bool, limit, offset int) (NodePage, error) {
	if len(userTokens) == 0 {
		return NodePage{}, fmt.Errorf("no user tokens found")
	}
	limit, offset = normalizePagination(limit, offset)
	ctx, cancel := s.newCtx()
	defer cancel()

	administered, err := s.store.ListNodes(ctx, NodeQuery{
		Kinds:         []string{model.KindBudget},
		AdminScopeAny: userTokens,
	}, 0, 0)
	if err != nil {
		return NodePage{}, fmt.Errorf("load administered budgets: %w", err)
	}
	if len(administered) == 0 {
		return newNodePage(nil, 0, limit, offset), nil
	}

	// The parents whose children the caller decides on.
	var parentIDs []string
	if includeSubtree {
		parentMap, err := s.buildSubtreeParentMap(ctx, administered)
		if err != nil {
			return NodePage{}, fmt.Errorf("collect administered subtrees: %w", err)
		}
		for id := range parentMap {
			parentIDs = append(parentIDs, id)
		}
	} else {
		parentIDs, err = s.undelegatedBudgetIDs(ctx, administered, userTokens)
		if err != nil {
			return NodePage{}, err
		}
	}

	query := NodeQuery{
		ParentIDs: parentIDs,
		Statuses:  []string{model.StatusPending, model.StatusChangePending, model.StatusImported},
	}
	waiting, err := s.store.ListNodes(ctx, query, limit, offset)
	if err != nil {
		return NodePage{}, fmt.Errorf("load requests to manage: %w", err)
	}
	total, err := s.store.CountNodes(ctx, query)
	if err != nil {
		return NodePage{}, fmt.Errorf("count requests to manage: %w", err)
	}
	// The name of the funding budget travels with the request, so the inbox can
	// say where something arrived without a lookup per entry.
	named, err := s.attachParentNames(ctx, waiting, userTokens)
	if err != nil {
		return NodePage{}, err
	}
	return newNodePage(named, total, limit, offset), nil
}

// SearchNodes finds nodes anywhere below the budgets the caller administers.
//
// This exists because the tree is paginated: the UI used to load every node and
// filter in the browser, which is exactly what a tree with thousands of student
// projects cannot do. Matching happens here, over the whole managed subtree, and
// the result is a flat list — a hit deep in the tree is easier to read as "this
// node, under that budget" than as a tree unfolded down to it.
//
// Matched fields mirror what the row shows plus what people search by: name,
// purpose, ID, owner, creator, the OpenStack project, and the tokens on it.
func (s *Service) SearchNodes(userTokens common.TokenList, query string, limit, offset int) (NodePage, error) {
	if len(userTokens) == 0 {
		return NodePage{}, common.ErrForbidden
	}
	needle := strings.ToLower(strings.TrimSpace(query))
	if needle == "" {
		return NodePage{}, fmt.Errorf("q must not be empty")
	}
	limit, offset = normalizePagination(limit, offset)
	ctx, cancel := s.newCtx()
	defer cancel()

	administered, err := s.store.ListNodes(ctx, NodeQuery{
		Kinds:         []string{model.KindBudget},
		AdminScopeAny: userTokens,
	}, 0, 0)
	if err != nil {
		return NodePage{}, fmt.Errorf("load administered budgets: %w", err)
	}
	if len(administered) == 0 {
		return newNodePage(nil, 0, limit, offset), nil
	}

	// Every budget below the administered ones, so a project three levels down
	// is searchable too.
	parentMap, err := s.buildSubtreeParentMap(ctx, administered)
	if err != nil {
		return NodePage{}, fmt.Errorf("collect administered subtrees: %w", err)
	}
	budgetIDs := make([]string, 0, len(parentMap))
	for id := range parentMap {
		budgetIDs = append(budgetIDs, id)
	}

	candidates, err := s.store.ListNodes(ctx, NodeQuery{ParentIDs: budgetIDs}, 0, 0)
	if err != nil {
		return NodePage{}, fmt.Errorf("load subtree: %w", err)
	}
	// The administered budgets themselves are searchable as well; they are
	// nobody's child within this set unless their parent is also administered.
	seen := make(map[string]bool, len(candidates))
	for _, n := range candidates {
		seen[n.ID] = true
	}
	for _, n := range administered {
		if !seen[n.ID] {
			candidates = append(candidates, n)
			seen[n.ID] = true
		}
	}

	matches := make([]model.Node, 0, 16)
	for _, n := range candidates {
		if nodeMatches(n, needle) {
			matches = append(matches, n)
		}
	}
	slices.SortFunc(matches, func(a, b model.Node) int { return strings.Compare(a.ID, b.ID) })

	total := len(matches)
	// Only the page itself is decorated (usage rollup, child count, parent name):
	// a search over a large tree would otherwise roll up usage for every match.
	decorated, err := s.attachUsage(ctx, paginateInMemory(matches, limit, offset), userTokens)
	if err != nil {
		return NodePage{}, err
	}
	return newNodePage(decorated, total, limit, offset), nil
}

// nodeMatches reports whether a node contains the (already lower-cased) needle
// in any of its searchable fields.
func nodeMatches(n model.Node, needle string) bool {
	fields := []string{
		n.Name, n.Reason, n.ID, n.Owner, n.CreatedBy,
		n.ExternalName, n.ExternalID, n.Status,
	}
	fields = append(fields, n.AdminScope...)
	fields = append(fields, n.EligibleRequesters...)
	for _, u := range n.AuthorizedUsers {
		fields = append(fields, u.Token)
	}
	for _, f := range fields {
		if f != "" && strings.Contains(strings.ToLower(f), needle) {
			return true
		}
	}
	return false
}

// UsageScope returns the node whose consumption the caller asks for, if they
// may see it: a project to everyone who may see the project, a budget's
// subtree to its managers and those above — a requester sees a budget, not what
// the others in it use.
func (s *Service) UsageScope(id string, userTokens common.TokenList) (*model.Node, error) {
	node, err := s.GetNode(id, userTokens)
	if err != nil {
		return nil, err
	}
	if node == nil {
		return nil, fmt.Errorf("node %w", common.ErrNotFound)
	}
	if node.Kind == model.KindBudget {
		ctx, cancel := s.newCtx()
		defer cancel()
		manages, err := s.managesNode(ctx, userTokens, node)
		if err != nil {
			return nil, err
		}
		if !manages {
			return nil, common.ErrForbidden
		}
	}
	return node, nil
}

// normalizePagination clamps incoming pagination values to safe limits.
func normalizePagination(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = common.DefaultPageLimit
	}
	if limit > common.MaxPageLimit {
		limit = common.MaxPageLimit
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}
