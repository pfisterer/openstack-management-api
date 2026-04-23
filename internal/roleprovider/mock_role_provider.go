package roleprovider

import (
	"context"
	"sort"
	"strings"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/mockdata"
)

type MockRoleProvider struct{}

// Ensure MockRoleProvider implements the RoleProvider interface
var _ common.RoleProvider = (*MockRoleProvider)(nil)

func NewMockRoleProvider() *MockRoleProvider {
	return &MockRoleProvider{}
}

// GetUserTokens returns mock tokens for a user based on mockdata identities.
func (m *MockRoleProvider) GetUserTokens(ctx context.Context, claims *common.UserClaims) (common.TokenList, error) {
	_ = ctx
	if claims == nil {
		return common.TokenList{}, nil
	}
	userEmail := claims.ResolveEmail()
	if userEmail == "" {
		return common.TokenList{}, nil
	}
	// Use mockdata identities for token lookup
	identities, _, _, _ := mockdata.DefaultMockResourceState()
	for _, ident := range identities {
		if strings.EqualFold(ident.Email, userEmail) {
			return ident.Tokens, nil
		}
	}
	return common.TokenList{"user:" + userEmail}, nil
}

// GetGroupUsers returns the emails of all mock identities that carry the given group token.
func (m *MockRoleProvider) GetGroupUsers(_ context.Context, groupToken string) ([]string, error) {
	identities, _, _, _ := mockdata.DefaultMockResourceState()
	var emails []string
	for _, ident := range identities {
		for _, token := range ident.Tokens {
			if token == groupToken {
				emails = append(emails, ident.Email)
				break
			}
		}
	}
	return emails, nil
}

// SearchGroupTokens returns mock group tokens from mockdata identities.
func (m *MockRoleProvider) SearchGroupTokens(_ context.Context, query string, limit int) (common.TokenList, error) {
	identities, _, _, _ := mockdata.DefaultMockResourceState()
	groupSet := map[string]struct{}{}
	for _, ident := range identities {
		for _, token := range ident.Tokens {
			if strings.HasPrefix(token, "group:") {
				groupSet[token] = struct{}{}
			}
		}
	}
	needle := strings.ToLower(strings.TrimSpace(query))
	out := make(common.TokenList, 0, len(groupSet))
	for token := range groupSet {
		if needle == "" || strings.Contains(token, needle) {
			out = append(out, token)
		}
	}
	// Sort the output list
	if len(out) > 1 {
		sort.Strings(out)
	}

	// Apply limit if specified
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
