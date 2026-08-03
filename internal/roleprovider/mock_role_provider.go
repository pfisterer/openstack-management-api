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
	identities, _ := mockdata.DefaultMockTreeState()
	for _, ident := range identities {
		if strings.EqualFold(ident.Email, userEmail) {
			return ident.Tokens, nil
		}
	}
	return common.TokenList{"user:" + userEmail}, nil
}

// GetGroupUsers returns the emails of all mock identities that carry the given group token.
func (m *MockRoleProvider) GetGroupUsers(_ context.Context, groupToken string) ([]string, error) {
	identities, _ := mockdata.DefaultMockTreeState()
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

// mockGroupLabels gives the mock groups a human-readable label, mirroring the
// display names the role-provider-service seeds, so label search behaves the
// same in mock mode as against the real provider.
var mockGroupLabels = map[string]string{
	mockdata.RootGroup:      "University Root",
	mockdata.DeptCSAdmin:    "Computer Science Dept",
	mockdata.DeptCSFaculty:  "CS Faculty Pool",
	mockdata.DeptBioGroup:   "Biology Dept",
	mockdata.CSStudentGroup: "CS Students",
}

// SearchGroups returns mock groups from mockdata identities, matching the query
// against the token and the label, like the real provider does against group ID,
// display name and description.
func (m *MockRoleProvider) SearchGroups(_ context.Context, query string, limit int) ([]common.GroupSummary, error) {
	identities, _ := mockdata.DefaultMockTreeState()
	groupSet := map[string]struct{}{}
	for _, ident := range identities {
		for _, token := range ident.Tokens {
			if strings.HasPrefix(token, groupPrefix) {
				groupSet[token] = struct{}{}
			}
		}
	}
	needle := strings.ToLower(strings.TrimSpace(query))
	out := make([]common.GroupSummary, 0, len(groupSet))
	for token := range groupSet {
		label := mockGroupLabels[token]
		if needle != "" &&
			!strings.Contains(strings.ToLower(token), needle) &&
			!strings.Contains(strings.ToLower(label), needle) {
			continue
		}
		out = append(out, common.GroupSummary{Token: token, Label: label})
	}
	// Sort the output list
	if len(out) > 1 {
		sort.Slice(out, func(i, j int) bool { return out[i].Token < out[j].Token })
	}

	// Apply limit if specified
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
