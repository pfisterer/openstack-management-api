// Package roles is what the tree needs to know about people and groups: the
// interface it asks, not where the answers come from.
package roles

import (
	"context"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

// Provider abstracts authorization and token discovery: which groups a person
// belongs to, and finding groups and people to name on a node.
// Implementations live in package roleprovider (the role-provider-service over
// HTTP, and a mock); the tree depends only on this interface.
type Provider interface {
	// GetUserTokens queries the authorization system to discover all tokens/relationships a user has.
	GetUserTokens(ctx context.Context, claims *common.UserClaims) (common.TokenList, error)

	// SearchGroups searches for known groups whose token, label or description
	// matches the query.
	SearchGroups(ctx context.Context, query string, limit int) ([]GroupSummary, error)

	// SearchUsers returns email addresses matching query. Matching is on the
	// address only, never on a person's name — the directory must not be
	// browsable by name.
	SearchUsers(ctx context.Context, query string, limit int) ([]string, error)

	// GetGroupUsers returns the email addresses of all users belonging to the given group token
	// (e.g. "group:dept_cs_faculty"), or holding the relation a token names
	// ("group:wwi23seb#dozent"). Returns an empty slice when there are none.
	GetGroupUsers(ctx context.Context, groupToken string) ([]string, error)

	// ListRelations returns the relations a group can carry, "member" first.
	ListRelations(ctx context.Context) ([]string, error)
}

// GroupSummary is a group token together with its human-readable label (the
// role provider's display name) and description. The picker searches both the
// token and the label, so the label has to travel with the token.
type GroupSummary struct {
	Token       string `json:"token"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}
