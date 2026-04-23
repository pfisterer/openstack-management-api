package webserver

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/openstack-management-api/internal/common"
)

// TokenListResponse wraps the group tokens and optional metadata for search/list endpoints.
type TokenListResponse struct {
	Tokens common.TokenList `json:"tokens"`
}

// searchGroups searches for group tokens.
//
//	@Summary		Search group tokens
//	@Description	Searches for group tokens matching the provided text query.
//	@Tags			groups
//	@Produce		json
//	@Security		Bearer
//	@Param			q		query		string	false	"Search query text"
//	@Param			limit	query		int		false	"Maximum number of entries to return" default(50)
//	@Success		200	{object}		webserver.TokenListResponse	"List of matching group tokens."
//	@Failure		401	{object}	map[string]any	"Unauthorized."
//	@Failure		500	{object}	map[string]any	"Internal server error."
//	@ID				searchGroups
//	@Router			/v1/groups/search [get]
func searchGroups(cfg ProjectAPIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		limit, _, err := parsePagination(c)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid pagination parameters"})
			return
		}

		searchText := c.Query("q")
		groupTokens, err := svc.SearchGroupTokens(searchText, limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		resp := TokenListResponse{
			Tokens: groupTokens,
		}
		c.JSON(http.StatusOK, resp)
	}
}

// listMyGroups lists the group tokens for the current user.
//
//	@Summary		List my groups
//	@Description	Lists the group tokens for the current authenticated user.
//	@Tags			groups
//	@Produce		json
//	@Security		Bearer
//	@Success		200	{object}		webserver.TokenListResponse	"List of group tokens for the user."
//	@Failure		401	{object}	map[string]any	"Unauthorized."
//	@ID				listMyGroups
//	@Router			/v1/groups/mine [get]
func listMyGroups(cfg ProjectAPIConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		auth, err := mustGetAuthContext(c)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		resp := TokenListResponse{
			Tokens: auth.EffectiveTokens,
		}
		c.JSON(http.StatusOK, resp)
	}
}
