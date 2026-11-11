package webserver

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/cloud-self-service-golib/calendar"
	"github.com/pfisterer/cloud-self-service-golib/ginweb"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/usage"
	"go.uber.org/zap"
)

// UsageReader reads the daily consumption rows (usage.Store, reading half).
type UsageReader interface {
	Days(ctx context.Context, nodeIDs []string, from, to time.Time) ([]usage.Day, error)
	DaysUnder(ctx context.Context, budgetID string, from, to time.Time) ([]usage.Day, error)
}

// usagePeriods is the rule of the usage reports (golib calendar, the same as the AI service's): by default the last 30 finished days in Europe/Berlin, only to = the 30 days ending there, at most three years (a few years of rows are still cheap to sum, an unbounded range is a way to make the server do that for everybody).
var usagePeriods = calendar.Report(3 * 366)

// usagePeriod reads ?from=YYYY-MM-DD&to=YYYY-MM-DD, both days included (usagePeriods), as the store's day labels: from at midnight UTC of its day and the end at midnight UTC of the day after to. A period the rule refuses is answered with 400 and ok is false.
func usagePeriod(c *gin.Context) (from, to, end time.Time, ok bool) {
	p, ok := ginweb.Period(c, usagePeriods, time.Now())
	if !ok {
		return time.Time{}, time.Time{}, time.Time{}, false
	}
	return p.From.AsUTC(), p.To.AsUTC(), p.To.AddDays(1).AsUTC(), true
}

// getNodeUsage reports what a project, or everything below a budget, used.
//
//	@Summary		Get node usage
//	@Description	What a project used over a period — or, for a budget, every project that was below it on the day, released ones included — from the daily rows the reconciler collects: per day, per project and in total, with utilisation (used ÷ reserved). A project's usage is visible to everyone who may see the project, a budget's to its managers and those above. Both days are included; the default is the last 30 finished days (calendar days in Europe/Berlin), only to gives the 30 days ending there, at most 1098 days. No euro values here — those are in the root admins' report.
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
		auth, ok := caller(c)
		if !ok {
			return
		}
		from, to, end, ok := usagePeriod(c)
		if !ok {
			return
		}
		node, err := cfg.Service.UsageScope(c.Param("id"), auth.EffectiveTokens)
		if err != nil {
			cfg.respondError(c, err)
			return
		}
		var rows []usage.Day
		if cfg.Usage != nil {
			if node.Kind == model.KindBudget {
				rows, err = cfg.Usage.DaysUnder(c.Request.Context(), node.ID, from, end)
			} else {
				rows, err = cfg.Usage.Days(c.Request.Context(), []string{node.ID}, from, end)
			}
			if err != nil {
				cfg.respondInternal(c, err)
				return
			}
		}
		// A project's members see what it used, not who pays for it. Taken off
		// the rows rather than the report, so the days are not split by
		// attributes the caller cannot see.
		visible, err := cfg.Service.AttributesVisible(node, auth.EffectiveTokens)
		if err != nil {
			cfg.respondInternal(c, err)
			return
		}
		if !visible {
			for i := range rows {
				rows[i].Attributes = nil
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
//	@Description	The root admins' evaluation: every project's consumption over a period, with the budget path it had, so it can be summed by budget, faculty or location — released and deleted projects included. With list prices configured (API_USAGE_PRICES), each project and the total carry a euro value. Both days are included; the default is the last 30 finished days (calendar days in Europe/Berlin), only to gives the 30 days ending there, at most 1098 days. Requires a root admin token.
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
		from, to, end, ok := usagePeriod(c)
		if !ok {
			return
		}
		var rows []usage.Day
		if cfg.Usage != nil {
			var err error
			if rows, err = cfg.Usage.Days(c.Request.Context(), nil, from, end); err != nil {
				cfg.respondInternal(c, err)
				return
			}
		}
		c.JSON(http.StatusOK, usage.BuildReport(rows, from, to, mapping, cfg.UsagePrices))
	}
}
