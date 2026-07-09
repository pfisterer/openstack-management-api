package webserver

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"go.uber.org/zap"
)

// RegisterReconcilerRoutes mounts the admin reconciler endpoints under /v1/admin/reconcile.
// All routes are guarded by a middleware that rejects callers whose token set does not
// contain at least one of the rootAdminTokens.
// When rec is nil the routes are still registered but return 503 Service Unavailable,
// ensuring the CORS middleware on the parent group handles preflight and actual requests.
func RegisterReconcilerRoutes(v1 *gin.RouterGroup, rec ReconcilerAPI, rootAdminTokens common.TokenList, log *zap.SugaredLogger) {
	admin := v1.Group("/admin/reconcile")
	admin.Use(requireRootAdmin(rootAdminTokens, log))
	{
		// GET /v1/admin/reconcile/status — returns outcome of the last reconciliation run.
		admin.GET("/status", getReconcilerStatus(rec))

		// POST /v1/admin/reconcile/trigger — queues an immediate reconciliation run.
		admin.POST("/trigger", triggerReconcile(rec, log))
	}
}

// requireRootAdmin returns a middleware that aborts with 403 if the caller does not hold
// at least one token from rootAdminTokens.
func requireRootAdmin(rootAdminTokens common.TokenList, log *zap.SugaredLogger) gin.HandlerFunc {
	allowed := make(map[string]struct{}, len(rootAdminTokens))
	for _, t := range rootAdminTokens {
		allowed[t] = struct{}{}
	}
	return func(c *gin.Context) {
		_, userTokens, err := ResolveOriginalAuthContext(c)
		if err != nil {
			log.Warnw("Reconciler admin: failed to resolve auth context", "error", err)
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		for _, t := range userTokens {
			if _, ok := allowed[t]; ok {
				c.Next()
				return
			}
		}
		log.Warnw("Reconciler admin: access denied (not a root admin)", "tokens", userTokens)
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "forbidden"})
	}
}

// getReconcilerStatus returns the outcome of the last reconciliation run.
//
//	@Summary		Get reconcile status
//	@Description	Returns the outcome of the last reconciliation run. Requires root admin token.
//	@Tags			admin
//	@Produce		json
//	@Security		Bearer
//	@Success		200	{object}	map[string]any	"Last reconciliation status (reconciler.Status)."
//	@Failure		401	{object}	map[string]any			"Unauthorized."
//	@Failure		403	{object}	map[string]any			"Forbidden."
//	@ID				getAdminReconcileStatus
//	@Router			/v1/admin/reconcile/status [get]
func getReconcilerStatus(rec ReconcilerAPI) gin.HandlerFunc {
	return func(c *gin.Context) {
		if rec == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "reconciler is disabled"})
			return
		}
		c.JSON(http.StatusOK, rec.GetStatus())
	}
}

// triggerReconcile queues an immediate reconciliation run.
//
//	@Summary		Trigger reconcile
//	@Description	Queues an immediate reconciliation run. Requires root admin token.
//	@Tags			admin
//	@Produce		json
//	@Security		Bearer
//	@Success		202	{object}	map[string]any	"Reconciliation triggered."
//	@Failure		401	{object}	map[string]any				"Unauthorized."
//	@Failure		403	{object}	map[string]any				"Forbidden."
//	@ID				triggerAdminReconcile
//	@Router			/v1/admin/reconcile/trigger [post]
func triggerReconcile(rec ReconcilerAPI, log *zap.SugaredLogger) gin.HandlerFunc {
	return func(c *gin.Context) {
		if rec == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "reconciler is disabled"})
			return
		}
		log.Info("Manual reconciliation triggered via API")
		rec.Trigger()
		c.JSON(http.StatusAccepted, gin.H{"message": "reconciliation triggered"})
	}
}
