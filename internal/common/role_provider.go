package common

import (
	"context"
)

// RoleProvider abstracts authorization and token discovery.
// Implementations can use mock logic, Zanzibar, SpiceDB, or other systems.
type RoleProvider interface {
	// GetUserTokens queries the authorization system to discover all tokens/relationships a user has.
	GetUserTokens(ctx context.Context, claims *UserClaims) (TokenList, error)

	// SearchGroupTokens searches for known group tokens matching the query.
	SearchGroupTokens(ctx context.Context, query string, limit int) (TokenList, error)

	// GetGroupUsers returns the email addresses of all users belonging to the given group token
	// (e.g. "group:dept_cs_faculty"). Returns an empty slice when the group has no members.
	GetGroupUsers(ctx context.Context, groupToken string) ([]string, error)
}

// TokenLookupResult is the token information required by the auth middleware.
type TokenLookupResult struct {
	Found    bool
	Username string
	ReadOnly bool
}

// TokenLookupFunc resolves an API token string to token details.
type TokenLookupFunc func(ctx context.Context, tokenString string) (TokenLookupResult, error)

// UserTokenResolverFunc resolves effective authorization tokens for the given claims.
type UserTokenResolverFunc func(ctx context.Context, claims *UserClaims) (TokenList, error)
