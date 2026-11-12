package webserver

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/cloud-self-service-golib/apierr"
	"github.com/pfisterer/cloud-self-service-golib/ginweb"
	"github.com/pfisterer/cloud-self-service-golib/msg"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/identity"
	"go.uber.org/zap"
)

// RegisterReconcilerRoutes mounts the admin reconciler endpoints under /v1/admin/reconcile.
// All routes are guarded by a middleware that rejects callers whose token set does not
// contain at least one of the rootAdminTokens.
// When rec is nil the routes are still registered but return 503 Service Unavailable,
// ensuring the CORS middleware on the parent group handles preflight and actual requests.
func RegisterReconcilerRoutes(v1 *gin.RouterGroup, rec ReconcilerAPI, rootAdminTokens common.TokenList, log *zap.SugaredLogger) {
	admin := v1.Group("/admin/reconcile")
	admin.Use(requireRootAdmin(rootAdminTokens, log), reconcilerAvailable(rec))
	{
		// GET /v1/admin/reconcile/status — returns outcome of the last reconciliation run.
		admin.GET("/status", getReconcilerStatus(rec))

		// POST /v1/admin/reconcile/trigger — queues an immediate reconciliation run.
		admin.POST("/trigger", triggerReconcile(rec, log))
	}
}

// requireRootAdmin returns a middleware that aborts with 403 if the caller does not hold at least one token from rootAdminTokens, compared in canonical spelling like every token (common.CanonicalTokenSet).
func requireRootAdmin(rootAdminTokens common.TokenList, log *zap.SugaredLogger) gin.HandlerFunc {
	allowed := common.CanonicalTokenSet(rootAdminTokens)
	return func(c *gin.Context) {
		// Gate on the EFFECTIVE tokens, not the real caller's, so acting on behalf of
		// somebody is faithful: while a root admin acts for a non-root user, admin
		// actions are denied (they must switch back first). Actions therefore always
		// match the identity on screen.
		auth, err := identity.FromContext(c)
		if err != nil {
			log.Warnw("Reconciler admin: failed to resolve auth context", "error", err)
			ginweb.AbortError(c, identity.ErrUnauthenticated)
			return
		}
		if allowed.ContainsAny(auth.EffectiveTokens) {
			c.Next()
			return
		}
		log.Warnw("Reconciler admin: access denied (not a root admin)", "effective_tokens", auth.EffectiveTokens, "actor", auth.ActorEmail)
		ginweb.AbortError(c, apierr.New(http.StatusForbidden, "forbidden", msg.T("Nur für Root-Admins.", "For root admins only.")))
	}
}

// reconcilerAvailable answers 503 while there is no reconciler to talk to: none configured, or one still connecting to OpenStack — which is a different thing, and the API says which.
func reconcilerAvailable(rec ReconcilerAPI) gin.HandlerFunc {
	return func(c *gin.Context) {
		switch {
		case rec == nil:
			ginweb.AbortError(c, apierr.New(http.StatusServiceUnavailable, "reconciler_disabled", msg.T("Der Abgleich mit OpenStack ist ausgeschaltet.", "The reconciliation with OpenStack is switched off.")))
		case !rec.Ready():
			ginweb.AbortError(c, apierr.New(http.StatusServiceUnavailable, "reconciler_connecting", msg.T("Der Abgleich mit OpenStack ist eingeschaltet, die Verbindung zu OpenStack steht aber noch nicht.", "The reconciliation with OpenStack is switched on, but the connection to OpenStack is not up yet.")))
		default:
			c.Next()
		}
	}
}

// getReconcilerStatus returns the outcome of the last reconciliation run.
//
//	@Summary		Get reconcile status
//	@Description	Returns the outcome of the last reconciliation run. Requires root admin token.
//	@Tags			admin
//	@Produce		json
//	@Security		Bearer
//	@Success		200	{object}	reconciler.Status	"Last reconciliation status."
//	@Failure		401	{object}	map[string]any			"Unauthorized."
//	@Failure		403	{object}	map[string]any			"Forbidden."
//	@ID				getAdminReconcileStatus
//	@Router			/v1/admin/reconcile/status [get]
func getReconcilerStatus(rec ReconcilerAPI) gin.HandlerFunc {
	return func(c *gin.Context) {
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
		log.Info("Manual reconciliation triggered via API")
		rec.Trigger()
		c.JSON(http.StatusAccepted, gin.H{"message": "reconciliation triggered"})
	}
}
