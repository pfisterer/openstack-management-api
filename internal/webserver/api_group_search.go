package webserver

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// searchGroups searches for group tokens.
//
//	@Summary		Search group tokens
//	@Description	Searches for group tokens matching the provided text query.
//	@Tags			groups
//	@Produce		json
//	@Security		Bearer
//	@Param			q		query		string	false	"Search query text"
//	@Param			limit	query		int		false	"Maximum number of entries to return" default(50)
//	@Success		200	{array}		string	"List of matching group tokens."
//	@Failure		401	{object}	map[string]any	"Unauthorized."
//	@Failure		500	{object}	map[string]any	"Internal server error."
//	@ID				searchGroups
//	@Router			/v1/groups/search [get]
func searchGroups(cfg ResourceAPIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		searchText := c.Query("q")
		limit := 50
		if rawLimit := strings.TrimSpace(c.Query("limit")); rawLimit != "" {
			parsedLimit, err := strconv.Atoi(rawLimit)
			if err != nil || parsedLimit <= 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid limit parameter"})
				return
			}
			if parsedLimit > maxPageLimit {
				parsedLimit = maxPageLimit
			}
			limit = parsedLimit
		}

		groupTokens, err := svc.SearchGroupTokens(searchText, limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, groupTokens)
	}
}
