package roleprovider

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/roleprovider/api"
	"go.uber.org/zap"
)

// HttpRoleProvider implements common.RoleProvider by calling the role-provider-service REST API.
type HttpRoleProvider struct {
	client *roleclient.ClientWithResponses
	log    *zap.SugaredLogger
}

func NewHttpRoleProvider(baseURL, apiToken string, timeoutSeconds int, log *zap.SugaredLogger) (*HttpRoleProvider, error) {
	httpClient := &http.Client{Timeout: time.Duration(timeoutSeconds) * time.Second}

	authEditor := func(ctx context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer "+apiToken)
		return nil
	}

	client, err := roleclient.NewClientWithResponses(
		baseURL,
		roleclient.WithHTTPClient(httpClient),
		roleclient.WithRequestEditorFn(authEditor),
	)
	if err != nil {
		return nil, fmt.Errorf("NewHttpRoleProvider: %w", err)
	}

	return &HttpRoleProvider{client: client, log: log}, nil
}

// GetUserTokens calls GET /v1/users/{email}/tokens.
func (h *HttpRoleProvider) GetUserTokens(ctx context.Context, claims *common.UserClaims) (common.TokenList, error) {
	if claims == nil {
		return common.TokenList{}, nil
	}
	email := claims.ResolveEmail()
	if email == "" {
		return common.TokenList{}, nil
	}

	resp, err := h.client.GetUserTokensWithResponse(ctx, email)
	if err != nil {
		h.log.Warnw("HttpRoleProvider.GetUserTokens failed", "email", email, zap.Error(err))
		return common.TokenList{UserPrefix + email}, nil
	}
	if resp.JSON200 == nil {
		h.log.Warnw("HttpRoleProvider.GetUserTokens: unexpected status", "email", email, "status", resp.StatusCode())
		return common.TokenList{UserPrefix + email}, nil
	}

	return common.TokenList(*resp.JSON200), nil
}

// SearchGroupTokens calls GET /v1/groups?q=...&limit=... and returns matching group tokens.
func (h *HttpRoleProvider) SearchGroupTokens(ctx context.Context, query string, limit int) (common.TokenList, error) {
	params := &roleclient.ListGroupsParams{Q: &query, Limit: &limit}

	resp, err := h.client.ListGroupsWithResponse(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("HttpRoleProvider.SearchGroupTokens: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, fmt.Errorf("HttpRoleProvider.SearchGroupTokens: unexpected status %d", resp.StatusCode())
	}

	out := make(common.TokenList, 0, len(*resp.JSON200))
	for _, g := range *resp.JSON200 {
		if g.Token != nil {
			out = append(out, *g.Token)
		}
	}
	return out, nil
}

// GetGroupUsers calls GET /v1/groups/{token}/members?recursive=true and returns user emails.
func (h *HttpRoleProvider) GetGroupUsers(ctx context.Context, groupToken string) ([]string, error) {
	recursive := true
	params := &roleclient.ListGroupMembersParams{Recursive: &recursive}

	resp, err := h.client.ListGroupMembersWithResponse(ctx, groupToken, params)
	if err != nil {
		return nil, fmt.Errorf("HttpRoleProvider.GetGroupUsers: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, fmt.Errorf("HttpRoleProvider.GetGroupUsers: unexpected status %d", resp.StatusCode())
	}

	var emails []string
	for _, m := range *resp.JSON200 {
		if len(m) > len(UserPrefix) && m[:len(UserPrefix)] == UserPrefix {
			emails = append(emails, m[len(UserPrefix):])
		}
	}
	return emails, nil
}

const UserPrefix = "user:"
