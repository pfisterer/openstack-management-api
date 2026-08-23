package common

import (
	"context"
)

// RoleProvider abstracts authorization and token discovery.
// Implementations can use mock logic, Zanzibar, SpiceDB, or other systems.
type RoleProvider interface {
	// GetUserTokens queries the authorization system to discover all tokens/relationships a user has.
	GetUserTokens(ctx context.Context, claims *UserClaims) (TokenList, error)

	// SearchGroups searches for known groups whose token, label or description
	// matches the query.
	SearchGroups(ctx context.Context, query string, limit int) ([]GroupSummary, error)

	// SearchUsers returns email addresses matching query. Matching is on the
	// address only, never on a person's name — the directory must not be
	// browsable by name.
	SearchUsers(ctx context.Context, query string, limit int) ([]string, error)

	// GetGroupUsers returns the email addresses of all users belonging to the given group token
	// (e.g. "group:dept_cs_faculty"). Returns an empty slice when the group has no members.
	GetGroupUsers(ctx context.Context, groupToken string) ([]string, error)
}

// GroupSummary is a group token together with its human-readable label (the
// role provider's display name) and description. The picker searches both the
// token and the label, so the label has to travel with the token.
type GroupSummary struct {
	Token       string `json:"token"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// TokenLookupResult is the token information required by the auth middleware.
type TokenLookupResult struct {
	Found bool
	// Subject is the identity the token was issued for, in the form group
	// resolution expects — an e-mail address. Named Username before, which is
	// what invited filling it with preferred_username and getting a caller that
	// authenticates but belongs to no group.
	Subject  string
	ReadOnly bool
}

// TokenLookupFunc resolves an API token string to token details.
type TokenLookupFunc func(ctx context.Context, tokenString string) (TokenLookupResult, error)

// UserTokenResolverFunc resolves effective authorization tokens for the given claims.
type UserTokenResolverFunc func(ctx context.Context, claims *UserClaims) (TokenList, error)
