package tree

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

// ChildFilter narrows and orders the children of one budget — the table under
// a budget in the management view, where a budget for all students of a
// location holds hundreds of projects.
type ChildFilter struct {
	// Kind restricts to KindBudget or KindProject; empty means both.
	Kind string
	// Query matches the same fields as SearchNodes (name, purpose, id, owner,
	// OpenStack project, tokens), case-insensitively.
	Query string
	// Statuses restricts to these lifecycle statuses.
	Statuses []string
	// Group, with GroupMode, restricts by a group (or any token):
	//   GroupAccess: the token is the owner, an admin, or a member of the project;
	//   GroupOwner:  the owner holds the token — is in that group, or holds that
	//                relation in it — as the role provider resolves it.
	Group     string
	GroupMode string
	// Sort is one of the ChildSort* keys; empty keeps the store's order.
	Sort string
	Desc bool
}

// Group filter modes.
const (
	GroupAccess = "access"
	GroupOwner  = "owner"
)

// Sort keys for ListChildren.
const (
	ChildSortName    = "name"
	ChildSortOwner   = "owner"
	ChildSortStatus  = "status"
	ChildSortEnd     = "termination_date"
	ChildSortCreated = "created_at"
)

func (f ChildFilter) needsMemory() bool {
	return f.Query != "" || f.Group != "" || f.Sort != ""
}

// Validate refuses values that would otherwise be silently ignored.
func (f ChildFilter) Validate() error {
	switch f.Kind {
	case "", KindBudget, KindProject:
	default:
		return fmt.Errorf("kind must be %q or %q", KindBudget, KindProject)
	}
	switch f.Sort {
	case "", ChildSortName, ChildSortOwner, ChildSortStatus, ChildSortEnd, ChildSortCreated:
	default:
		return fmt.Errorf("unknown sort %q", f.Sort)
	}
	if f.Group != "" {
		switch f.GroupMode {
		case "", GroupAccess, GroupOwner:
		default:
			return fmt.Errorf("group_mode must be %q or %q", GroupAccess, GroupOwner)
		}
	}
	return nil
}

// ListChildren returns the direct children of a budget. Management view: only
// managers of the budget (or its ancestors) may list children.
//
// Without a text, group or sort filter the store pages directly. With one, the
// budget's direct children are loaded and filtered here — the set SearchNodes
// already handles for a whole subtree, so one budget's is well within reach —
// and only the page shown is decorated.
func (s *Service) ListChildren(parentID string, f ChildFilter, userTokens common.TokenList, limit, offset int) (NodePage, error) {
	if len(userTokens) == 0 {
		return NodePage{}, common.ErrForbidden
	}
	if err := f.Validate(); err != nil {
		return NodePage{}, err
	}
	limit, offset = normalizePagination(limit, offset)
	ctx, cancel := s.newCtx()
	defer cancel()

	parent, err := s.store.GetNode(ctx, parentID)
	if err != nil {
		return NodePage{}, fmt.Errorf("load parent node: %w", err)
	}
	if parent == nil {
		return NodePage{}, fmt.Errorf("node %w", common.ErrNotFound)
	}
	if manages, err := s.managesNode(ctx, userTokens, parent); err != nil {
		return NodePage{}, err
	} else if !manages {
		return NodePage{}, common.ErrForbidden
	}

	q := NodeQuery{ParentIDs: []string{parentID}, Statuses: f.Statuses}
	if f.Kind != "" {
		q.Kinds = []string{f.Kind}
	}
	if !f.needsMemory() {
		return s.listPage(ctx, q, limit, offset)
	}

	children, err := s.store.ListNodes(ctx, q, 0, 0)
	if err != nil {
		return NodePage{}, fmt.Errorf("load children: %w", err)
	}
	if needle := strings.ToLower(strings.TrimSpace(f.Query)); needle != "" {
		children = slices.DeleteFunc(children, func(n Node) bool { return !nodeMatches(n, needle) })
	}
	if f.Group != "" {
		children, err = s.filterByGroup(ctx, children, common.CanonicalToken(f.Group), f.GroupMode)
		if err != nil {
			return NodePage{}, err
		}
	}
	sortChildren(children, f.Sort, f.Desc)

	decorated, err := s.attachUsage(ctx, paginateInMemory(children, limit, offset))
	if err != nil {
		return NodePage{}, err
	}
	return newNodePage(decorated, len(children), limit, offset), nil
}

func (s *Service) filterByGroup(ctx context.Context, nodes []Node, token, mode string) ([]Node, error) {
	if mode != GroupOwner {
		return slices.DeleteFunc(nodes, func(n Node) bool { return !grantsAccess(n, token) }), nil
	}

	owners := map[string]bool{}
	for _, n := range nodes {
		if n.Owner != "" {
			owners[n.OwnerEmail()] = true
		}
	}
	holds, err := s.ownersHolding(ctx, owners, token)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(nodes, func(n Node) bool { return !holds[n.OwnerEmail()] }), nil
}

// grantsAccess reports whether the token is named on the node: as its owner,
// an admin, or a member.
func grantsAccess(n Node, token string) bool {
	if n.Owner == token || slices.Contains(n.AdminScope, token) {
		return true
	}
	for _, u := range n.AuthorizedUsers {
		if u.Token == token {
			return true
		}
	}
	return false
}

// ownersHolding asks the role provider, per owner, whether they hold the
// token. Per owner rather than by expanding the group: a group may be defined
// by a pattern ("everyone @student…"), which has no member list, and asking
// for a person's tokens answers patterns and relations alike.
func (s *Service) ownersHolding(ctx context.Context, owners map[string]bool, token string) (map[string]bool, error) {
	if s.roles == nil {
		return nil, fmt.Errorf("no role provider configured")
	}
	type result struct {
		email string
		holds bool
		err   error
	}
	emails := make(chan string)
	results := make(chan result)
	const workers = 8
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for email := range emails {
				tokens, err := s.ownerTokens.get(ctx, s.roles, email)
				results <- result{email, slices.Contains(tokens, token), err}
			}
		}()
	}
	go func() {
		for email := range owners {
			emails <- email
		}
		close(emails)
		wg.Wait()
		close(results)
	}()

	holds := map[string]bool{}
	var firstErr error
	for r := range results {
		if r.err != nil && firstErr == nil {
			firstErr = r.err
		}
		holds[r.email] = r.holds
	}
	if firstErr != nil {
		return nil, fmt.Errorf("resolve owners' groups: %w", firstErr)
	}
	return holds, nil
}

// tokenCache keeps a person's tokens for a short while: filtering a table by
// group asks for every owner in it, and a person who types a few letters into
// the filter would otherwise ask for all of them again with each.
type tokenCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]tokenCacheEntry
}

type tokenCacheEntry struct {
	tokens common.TokenList
	at     time.Time
}

func newTokenCache(ttl time.Duration) *tokenCache {
	return &tokenCache{ttl: ttl, entries: map[string]tokenCacheEntry{}}
}

func (c *tokenCache) get(ctx context.Context, roles common.RoleProvider, email string) (common.TokenList, error) {
	c.mu.Lock()
	e, ok := c.entries[email]
	c.mu.Unlock()
	if ok && time.Since(e.at) < c.ttl {
		return e.tokens, nil
	}
	tokens, err := roles.GetUserTokens(ctx, &common.UserClaims{Email: email})
	if err != nil {
		return nil, err
	}
	tokens = common.CanonicalTokens(tokens)
	c.mu.Lock()
	c.entries[email] = tokenCacheEntry{tokens: tokens, at: time.Now()}
	c.mu.Unlock()
	return tokens, nil
}

// sortChildren orders by the key, ties broken by id so a page boundary never
// moves between two requests.
func sortChildren(nodes []Node, key string, desc bool) {
	if key == "" {
		return
	}
	value := func(n Node) string {
		switch key {
		case ChildSortName:
			return strings.ToLower(cmp.Or(n.Name, n.Reason))
		case ChildSortOwner:
			return n.Owner
		case ChildSortStatus:
			return n.Status
		case ChildSortEnd:
			if n.TerminationDate != nil {
				return *n.TerminationDate
			}
			return "~" // no end sorts last
		case ChildSortCreated:
			return n.CreatedAt
		}
		return ""
	}
	slices.SortStableFunc(nodes, func(a, b Node) int {
		c := strings.Compare(value(a), value(b))
		if desc {
			c = -c
		}
		if c == 0 {
			c = strings.Compare(a.ID, b.ID)
		}
		return c
	})
}
