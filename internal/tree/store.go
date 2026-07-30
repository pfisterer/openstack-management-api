package tree

import (
	"context"
	"slices"
	"sort"
	"strings"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

// NodeQuery describes a filtered node listing. Zero-value fields are not applied;
// list fields match ANY of their values (OR semantics within a field, AND across
// fields).
type NodeQuery struct {
	// ParentIDs restricts to nodes whose parent is one of these IDs.
	ParentIDs []string
	// Kinds restricts to the given node kinds (KindBudget / KindProject).
	Kinds []string
	// Statuses restricts to the given lifecycle statuses.
	Statuses []string
	// Owner restricts to leaves with this exact owner token ("user:<email>").
	Owner string
	// AdminScopeAny restricts to nodes whose AdminScope contains any of these tokens.
	AdminScopeAny common.TokenList
	// EligibleAny restricts to nodes whose EligibleRequesters contains any of these tokens.
	EligibleAny common.TokenList
}

// Store is the persistence interface of the tree model.
type Store interface {
	// IsEmpty reports whether no nodes exist yet (drives mock seeding).
	IsEmpty(ctx context.Context) (bool, error)
	// Seed replaces the full state with the given identities and nodes.
	Seed(ctx context.Context, identities []common.Identity, nodes []Node) error

	// ListIdentities returns the seeded identities (dev/mock data, used by the
	// role-switch identity picker).
	ListIdentities(ctx context.Context) ([]common.Identity, error)
	// ListParticipants returns the distinct user emails appearing as owner or
	// authorized user on any leaf (used by the role-switch identity picker to
	// surface pattern-covered members).
	ListParticipants(ctx context.Context) ([]string, error)

	GetNode(ctx context.Context, id string) (*Node, error)
	// ListNodes returns nodes matching the query, ordered by ID, paginated.
	// limit <= 0 means no limit.
	ListNodes(ctx context.Context, q NodeQuery, limit, offset int) ([]Node, error)
	UpsertNode(ctx context.Context, n Node) error
	DeleteNodes(ctx context.Context, ids []string) error
}

// ParticipantEmails returns the distinct, sorted set of user emails appearing as
// owner or authorized user across the given nodes. Group tokens are ignored.
// Deduplication is case-insensitive; the first-seen spelling is preserved.
func ParticipantEmails(nodes []Node) []string {
	seen := map[string]string{} // lower-cased email -> first-seen spelling
	addToken := func(token string) {
		const prefix = "user:"
		if !strings.HasPrefix(token, prefix) {
			return
		}
		email := strings.TrimSpace(token[len(prefix):])
		if email == "" {
			return
		}
		if key := strings.ToLower(email); key != "" {
			if _, ok := seen[key]; !ok {
				seen[key] = email
			}
		}
	}
	for _, n := range nodes {
		if n.Owner != "" {
			addToken(n.Owner)
		}
		for _, u := range n.AuthorizedUsers {
			addToken(u.Token)
		}
	}
	out := make([]string, 0, len(seen))
	for _, orig := range seen {
		out = append(out, orig)
	}
	sort.Strings(out)
	return out
}

// matchesQuery reports whether a node satisfies the query. Shared by the
// in-memory store; the Postgres store translates the same semantics to SQL.
func matchesQuery(n Node, q NodeQuery) bool {
	if len(q.ParentIDs) > 0 {
		if n.ParentID == nil || !containsString(q.ParentIDs, *n.ParentID) {
			return false
		}
	}
	if len(q.Kinds) > 0 && !containsString(q.Kinds, n.Kind) {
		return false
	}
	if len(q.Statuses) > 0 && !containsString(q.Statuses, n.Status) {
		return false
	}
	if q.Owner != "" && n.Owner != q.Owner {
		return false
	}
	if len(q.AdminScopeAny) > 0 && !tokenListContainsAny(n.AdminScope, q.AdminScopeAny) {
		return false
	}
	if len(q.EligibleAny) > 0 && !tokenListContainsAny(n.EligibleRequesters, q.EligibleAny) {
		return false
	}
	return true
}

func containsString(haystack []string, needle string) bool {
	return slices.Contains(haystack, needle)
}

func tokenListContainsAny(list common.TokenList, any common.TokenList) bool {
	set := common.NewTokenSet(list)
	return set.ContainsAny(any)
}
