package webserver

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/tree"
	"github.com/pfisterer/openstack-management-api/internal/usage"
	"go.uber.org/zap"
)

// UsageReader reads the daily consumption rows (usage.Store, reading half).
type UsageReader interface {
	Days(ctx context.Context, nodeIDs []string, from, to time.Time) ([]usage.Day, error)
	DaysUnder(ctx context.Context, budgetID string, from, to time.Time) ([]usage.Day, error)
}

// defaultUsageDays is the period a report covers when none is asked for.
const defaultUsageDays = 30

// maxUsageDays bounds a period: a few years of rows are still cheap to sum,
// an unbounded range is a way to make the server do that for everybody.
const maxUsageDays = 3 * 366

// usagePeriod reads ?from=YYYY-MM-DD&to=YYYY-MM-DD, both days included. The
// default is the last 30 finished days: today is not collected yet.
func usagePeriod(c *gin.Context, now time.Time) (from, to time.Time, err error) {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	to = today.AddDate(0, 0, -1)
	if v := c.Query("to"); v != "" {
		if to, err = time.Parse(time.DateOnly, v); err != nil {
			return from, to, fmt.Errorf("to: want YYYY-MM-DD, got %q", v)
		}
	}
	from = to.AddDate(0, 0, -(defaultUsageDays - 1))
	if v := c.Query("from"); v != "" {
		if from, err = time.Parse(time.DateOnly, v); err != nil {
			return from, to, fmt.Errorf("from: want YYYY-MM-DD, got %q", v)
		}
	}
	if from.After(to) {
		return from, to, fmt.Errorf("from (%s) lies after to (%s)", from.Format(time.DateOnly), to.Format(time.DateOnly))
	}
	if to.Sub(from) > maxUsageDays*24*time.Hour {
		return from, to, fmt.Errorf("a period covers at most %d days", maxUsageDays)
	}
	return from, to, nil
}

// getNodeUsage reports what a project, or everything below a budget, used.
//
//	@Summary		Get node usage
//	@Description	What a project used over a period — or, for a budget, every project that was below it on the day, released ones included — from the daily rows the reconciler collects: per day, per project and in total, with utilisation (used ÷ reserved). A project's usage is visible to everyone who may see the project, a budget's to its managers and those above. Both days are included; the default is the last 30 finished days. No euro values here — those are in the root admins' report.
//	@Tags			usage
//	@Produce		json
//	@Security		Bearer
//	@Param			id		path		string	true	"Node ID"
//	@Param			from	query		string	false	"First day, YYYY-MM-DD"
//	@Param			to		query		string	false	"Last day, YYYY-MM-DD (default yesterday)"
//	@Success		200		{object}	usage.Report	"The report."
//	@Failure		400		{object}	map[string]any	"Bad period."
//	@Failure		401		{object}	map[string]any	"Unauthorized."
//	@Failure		403		{object}	map[string]any	"Forbidden."
//	@Failure		404		{object}	map[string]any	"Not found."
//	@ID				getNodeUsage
//	@Router			/v1/nodes/{id}/usage [get]
func getNodeUsage(cfg APIConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		mapping := usage.MappingFrom(cfg.resources())
		auth, err := mustGetAuthContext(c)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		from, to, err := usagePeriod(c, time.Now().UTC())
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		node, err := cfg.Service.UsageScope(c.Param("id"), auth.EffectiveTokens)
		if err != nil {
			c.JSON(errorToStatus(err), gin.H{"error": err.Error()})
			return
		}
		var rows []usage.Day
		if cfg.Usage != nil {
			end := to.AddDate(0, 0, 1)
			if node.Kind == tree.KindBudget {
				rows, err = cfg.Usage.DaysUnder(c.Request.Context(), node.ID, from, end)
			} else {
				rows, err = cfg.Usage.Days(c.Request.Context(), []string{node.ID}, from, end)
			}
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
		}
		c.JSON(http.StatusOK, usage.BuildReport(rows, from, to, mapping, nil))
	}
}

// RegisterUsageAdminRoutes mounts the root admins' evaluation.
func RegisterUsageAdminRoutes(v1 *gin.RouterGroup, cfg APIConfig, rootAdminTokens common.TokenList, log *zap.SugaredLogger) {
	v1.GET("/admin/usage", requireRootAdmin(rootAdminTokens, log), getUsageReport(cfg))
}

// getUsageReport reports what every project used, for the root admins.
//
//	@Summary		Get usage of all projects
//	@Description	The root admins' evaluation: every project's consumption over a period, with the budget path it had, so it can be summed by budget, faculty or location — released and deleted projects included. With list prices configured (API_USAGE_PRICES), each project and the total carry a euro value. Both days are included; the default is the last 30 finished days. Requires a root admin token.
//	@Tags			usage
//	@Produce		json
//	@Security		Bearer
//	@Param			from	query		string	false	"First day, YYYY-MM-DD"
//	@Param			to		query		string	false	"Last day, YYYY-MM-DD (default yesterday)"
//	@Success		200		{object}	usage.Report	"The report."
//	@Failure		400		{object}	map[string]any	"Bad period."
//	@Failure		401		{object}	map[string]any	"Unauthorized."
//	@Failure		403		{object}	map[string]any	"Forbidden."
//	@ID				getUsageReport
//	@Router			/v1/admin/usage [get]
func getUsageReport(cfg APIConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		mapping := usage.MappingFrom(cfg.resources())
		from, to, err := usagePeriod(c, time.Now().UTC())
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		var rows []usage.Day
		if cfg.Usage != nil {
			if rows, err = cfg.Usage.Days(c.Request.Context(), nil, from, to.AddDate(0, 0, 1)); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
		}
		c.JSON(http.StatusOK, usage.BuildReport(rows, from, to, mapping, cfg.UsagePrices))
	}
}
