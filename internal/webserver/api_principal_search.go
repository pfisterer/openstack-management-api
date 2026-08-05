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

// PrincipalSearchResponse wraps everything a token field may be filled with:
// groups with their labels, and bare email addresses for individual people.
type PrincipalSearchResponse struct {
	Groups []common.GroupSummary `json:"groups"`
	Users  []string              `json:"users"`
}

// searchPrincipals searches for groups and users.
//
//	@Summary		Search groups and users
//	@Description	Returns the principals a token field may be filled with. Groups match on token, label (display name) or description. Users match on their EMAIL ADDRESS ONLY and only once q is non-empty — the directory is deliberately not browsable by person or by name.
//	@Tags			groups
//	@Produce		json
//	@Security		Bearer
//	@Param			q		query		string	false	"Search query text"
//	@Param			limit	query		int		false	"Maximum entries per kind"	default(50)
//	@Success		200	{object}	webserver.PrincipalSearchResponse	"Matching groups and users."
//	@Failure		401	{object}	map[string]any	"Unauthorized."
//	@Failure		500	{object}	map[string]any	"Internal server error."
//	@ID				searchPrincipals
//	@Router			/v1/principals/search [get]
func searchPrincipals(cfg APIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		limit, _, err := parsePagination(c)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid pagination parameters"})
			return
		}

		groups, users, err := svc.SearchPrincipals(c.Query("q"), limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if groups == nil {
			groups = []common.GroupSummary{}
		}
		if users == nil {
			users = []string{}
		}
		c.JSON(http.StatusOK, PrincipalSearchResponse{Groups: groups, Users: users})
	}
}
