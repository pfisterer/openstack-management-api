package webserver

import (
	"net/http"

	"github.com/pfisterer/openstack-management-api/internal/core/roles"

	"github.com/gin-gonic/gin"
)

// PrincipalSearchResponse wraps everything a token field may be filled with:
// groups with their labels, and bare email addresses for individual people.
type PrincipalSearchResponse struct {
	Groups []roles.GroupSummary `json:"groups"`
	Users  []string             `json:"users"`
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
	return func(c *gin.Context) {
		limit, _, ok := pagination(c)
		if !ok {
			return
		}
		groups, users, err := cfg.Identity.SearchPrincipals(c.Query("q"), limit)
		if err != nil {
			cfg.respondInternal(c, err)
			return
		}
		if groups == nil {
			groups = []roles.GroupSummary{}
		}
		if users == nil {
			users = []string{}
		}
		c.JSON(http.StatusOK, PrincipalSearchResponse{Groups: groups, Users: users})
	}
}
