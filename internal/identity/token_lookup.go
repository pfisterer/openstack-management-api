package identity

import (
	"context"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

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
type UserTokenResolverFunc func(ctx context.Context, claims *common.UserClaims) (common.TokenList, error)
