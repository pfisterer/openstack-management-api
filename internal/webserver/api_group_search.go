package webserver

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/openstack-management-api/internal/common"
)

// TokenListResponse wraps the group tokens for list endpoints.
type TokenListResponse struct {
	Tokens common.TokenList `json:"tokens"`
}

// GroupSearchResponse wraps the matching groups, each with its label.
type GroupSearchResponse struct {
	Groups []common.GroupSummary `json:"groups"`
}

// searchGroups searches for groups.
//
//	@Summary		Search groups
//	@Description	Searches for groups whose token, label (display name) or description matches the provided text query.
//	@Tags			groups
//	@Produce		json
//	@Security		Bearer
//	@Param			q		query		string	false	"Search query text"
//	@Param			limit	query		int		false	"Maximum number of entries to return" default(50)
//	@Success		200	{object}		webserver.GroupSearchResponse	"List of matching groups."
//	@Failure		401	{object}	map[string]any	"Unauthorized."
//	@Failure		500	{object}	map[string]any	"Internal server error."
//	@ID				searchGroups
//	@Router			/v1/groups/search [get]
func searchGroups(cfg APIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		limit, _, err := parsePagination(c)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid pagination parameters"})
			return
		}

		searchText := c.Query("q")
		groups, err := svc.SearchGroups(searchText, limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, GroupSearchResponse{Groups: groups})
	}
}
