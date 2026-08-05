package webserver

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-contrib/cors"
	ginzap "github.com/gin-contrib/zap"
	"github.com/gin-gonic/gin"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/helper"
	"github.com/pfisterer/openstack-management-api/internal/reconciler"
	"github.com/pfisterer/openstack-management-api/internal/tree"
	"go.uber.org/zap"
)

const (
	defaultPageLimit = common.DefaultPageLimit
	maxPageLimit     = common.MaxPageLimit
)

// ReconcilerAPI is the minimal interface the admin endpoint needs from the reconciler.
type ReconcilerAPI interface {
	Trigger()
	GetStatus() reconciler.Status
}

// SetupConfig defines required dependencies for constructing the HTTP router.
type SetupConfig struct {
	DevMode      bool
	Log          *zap.SugaredLogger
	StaticConfig StaticConfig
	API          APIConfig
	// Reconciler is optional; when nil the /v1/admin/reconcile endpoints are omitted.
	Reconciler ReconcilerAPI
	// RootAdminTokens is the set of tokens whose holders may access the reconciler admin endpoints.
	// Requests that carry none of these tokens receive 403 Forbidden.
	RootAdminTokens common.TokenList
	AuthMiddleware  gin.HandlerFunc
}

// ConfigResponse contains system-wide resource configuration for the frontend.
type ConfigResponse struct {
	Resources      []common.ManagedProject `json:"resources"`
	OpenstackRoles []string                `json:"openstackRoles"`
	DummyDevUsers  []string                `json:"dummyDevUsers,omitempty"`
	// ProvisioningEnabled reports whether the reconciler is running. Only then
	// does an approved project eventually get an OpenStack project — without it
	// the UI would show every project as "waiting for OpenStack" forever.
	ProvisioningEnabled bool `json:"provisioningEnabled"`
}

// APIService provides the business operations consumed by the HTTP handlers.
// It is implemented by tree.Service (which embeds identity.Service for the
// role-switch operations).
type APIService interface {
	// Principal search (groups + users) for token fields
	SearchPrincipals(query string, limit int) ([]common.GroupSummary, []string, error)

	// Node reads / views
	GetNode(id string, userTokens common.TokenList) (*tree.Node, error)
	ListChildren(parentID string, userTokens common.TokenList, limit, offset int) (tree.NodePage, error)
	ListMine(userEmail string, limit, offset int) (tree.NodePage, error)
	ListMyBudgets(userTokens common.TokenList, limit, offset int) (tree.NodePage, error)
	ListEligibleForMe(userTokens common.TokenList, limit, offset int) (tree.NodePage, error)
	ListEligibleForOwner(callerTokens common.TokenList, ownerTokens common.TokenList, limit, offset int) (tree.NodePage, error)
	ListToManage(userTokens common.TokenList, includeSubtree bool, limit, offset int) (tree.NodePage, error)
	SearchNodes(userTokens common.TokenList, query string, limit, offset int) (tree.NodePage, error)

	// Node lifecycle
	CreateNode(req tree.CreateNodeRequest, actor string, userEmail string, userTokens common.TokenList) (tree.Node, error)
	UpdateNode(id string, req tree.UpdateNodeRequest, actor string, userTokens common.TokenList) (tree.Node, error)
	RequestChange(id string, req tree.ChangeNodeRequest, actor string, userTokens common.TokenList) (tree.Node, error)
	ApproveNode(id string, req tree.ApproveNodeRequest, actor string, userTokens common.TokenList) (tree.Node, error)
	RejectNode(id string, req tree.RejectNodeRequest, actor string, userTokens common.TokenList) (tree.Node, error)
	ReleaseNode(id string, actor string, userTokens common.TokenList) (tree.Node, error)
	ReparentNode(id string, req tree.ReparentNodeRequest, actor string, userTokens common.TokenList) (tree.Node, error)
	TransferOwner(id string, req tree.TransferOwnerRequest, actor string, userTokens common.TokenList) (tree.Node, error)
	PromoteNode(id string, req tree.PromoteNodeRequest, actor string, userTokens common.TokenList) (tree.Node, error)
	DeleteNode(id string, actor string, userTokens common.TokenList) error

	// Role switch related operations
	GetUserGroupSwitchForActor(actorEmail string) *string
	SetUserGroupSwitchForActor(actorEmail, groupToken string) error
	SetUserImpersonationForActor(actorEmail, targetEmail string) error
	ClearUserGroupSwitchForActor(actorEmail string)
	ResolveEffectiveUserTokens(actorEmail string, originalTokens common.TokenList) common.TokenList
	ResolveEffectiveEmail(actorEmail string) string
}

// APIConfig configures API route registration.
type APIConfig struct {
	RoleSwitchGroups   common.TokenList
	ProjectDefinitions []common.ManagedProject
	Service            APIService
	DummyDevUsers      []string
	// ProvisioningEnabled mirrors "the reconciler is configured and running".
	ProvisioningEnabled bool
}

// SetupGinWebserver configures and returns the application router.
func SetupGinWebserver(cfg SetupConfig) *gin.Engine {
	// Determine which Gin mode to run in based on the DevMode flag.
	ginMode := gin.ReleaseMode
	if cfg.DevMode {
		ginMode = gin.TestMode
	}
	gin.SetMode(ginMode)
	cfg.Log.Debugf("Running Gin web server in '%s' mode.", ginMode)

	// Setup Gin router and middleware.
	router := gin.New()

	if cfg.DevMode {
		cfg.Log.Debugf("Disabling caching in development mode.")
		router.Use(disableCachingMiddleware())
	}

	// Pipe Gin internals through Zap logger outputs.
	ginLogWriter := &helper.ZapWriter{SugarLogger: cfg.Log, Level: cfg.Log.Level()}
	gin.DefaultWriter = ginLogWriter
	gin.DefaultErrorWriter = ginLogWriter
	router.Use(ginzap.RecoveryWithZap(cfg.Log.Desugar(), true))

	// Setup static file serving routes
	staticGroup := router.Group("/")
	staticGroup.Use(cors.Default())
	// The generated API client (client/*.gen.mjs) is imported by the SPA at runtime.
	// It MUST never be served stale: when the API grows an operation, a browser holding
	// a cached older SDK silently lacks the new method and the UI feature no-ops (this
	// masked the role-switch impersonation picker). In DevMode the whole router already
	// gets this; in production only the static assets need it (API responses set their
	// own no-store where relevant), so force revalidation on this group unconditionally.
	if !cfg.DevMode {
		staticGroup.Use(disableCachingMiddleware())
	}
	RegisterStaticRoutes(staticGroup, cfg.StaticConfig)

	// Setup API v1 routes
	apiV1Group := router.Group("/v1")

	// Enable CORS with origin reflection for API routes to allow cross-origin requests from any domain
	enableCorsOriginReflectionConfig(apiV1Group)

	// Apply authentication middleware to API routes if provided
	if cfg.AuthMiddleware != nil {
		apiV1Group.Use(cfg.AuthMiddleware)
	}

	// Register API routes with the provided tree service and role switch groups configuration
	RegisterApiRoutes(apiV1Group, cfg.API, cfg.Log)

	// Always register reconciler admin endpoints so CORS headers are present even
	// when the reconciler is disabled. Handlers return 503 when Reconciler is nil.
	RegisterReconcilerRoutes(apiV1Group, cfg.Reconciler, cfg.RootAdminTokens, cfg.Log)

	return router
}

// RegisterApiRoutes wires all resource-management API endpoints.
func RegisterApiRoutes(v1 *gin.RouterGroup, cfg APIConfig, log *zap.SugaredLogger) *gin.RouterGroup {
	v1.Use(EffectiveAuthMiddleware(cfg.Service))

	v1.GET("/config", getConfig(cfg))

	roleSwitch := v1.Group("/role-switch")
	{
		roleSwitch.GET("", getRoleSwitch(cfg))
		roleSwitch.PUT("", setRoleSwitch(cfg))
		roleSwitch.DELETE("", clearRoleSwitch(cfg))
	}

	v1.GET("/principals/search", searchPrincipals(cfg))

	nodes := v1.Group("/nodes")
	{
		nodes.GET("/mine", listMyNodes(cfg))
		nodes.GET("/to-manage", listNodesToManage(cfg))
		nodes.GET("/my-budgets", listMyBudgets(cfg))
		nodes.GET("/eligible-for-me", listEligibleBudgets(cfg))
		nodes.GET("/eligible-for-owner", listEligibleBudgetsForOwner(cfg))
		nodes.GET("/search", searchNodes(cfg))
		nodes.GET("/:id", getNode(cfg))
		nodes.GET("/:id/children", listNodeChildren(cfg))
		nodes.POST("", createNode(cfg))
		nodes.PUT("/:id", updateNode(cfg))
		nodes.DELETE("/:id", deleteNode(cfg))
		nodes.POST("/:id/request-change", requestNodeChange(cfg))
		nodes.POST("/:id/approve", approveNode(cfg))
		nodes.POST("/:id/reject", rejectNode(cfg))
		nodes.POST("/:id/release", releaseNode(cfg))
		nodes.POST("/:id/reparent", reparentNode(cfg))
		nodes.POST("/:id/transfer-owner", transferNodeOwner(cfg))
		nodes.POST("/:id/promote", promoteNode(cfg))
	}

	return v1
}

// getConfig returns the system-wide resource configuration.
//
//	@Summary		Get resource configuration
//	@Description	Retrieves system-wide configuration including resource types and OpenStack roles.
//	@Tags			config
//	@Produce		json
//	@Security		Bearer
//	@Success		200	{object}	ConfigResponse	"Resource configuration."
//	@ID				getConfig
//	@Router			/v1/config [get]
func getConfig(cfg APIConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		resources := make([]common.ManagedProject, 0, len(cfg.ProjectDefinitions))
		for _, r := range cfg.ProjectDefinitions {
			if r.ShowOnUI {
				resources = append(resources, r)
			}
		}

		// Default OpenStack roles to be used in the frontend
		openstackRoles := []string{"admin", "member", "reader"}

		config := ConfigResponse{
			Resources:           resources,
			OpenstackRoles:      openstackRoles,
			ProvisioningEnabled: cfg.ProvisioningEnabled,
		}

		// Include dummy dev users in config if set, to inform frontend of available users for testing.
		if len(cfg.DummyDevUsers) > 0 {
			config.DummyDevUsers = cfg.DummyDevUsers
		}
		c.JSON(http.StatusOK, config)
	}
}

func disableCachingMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		c.Header("Pragma", "no-cache")
		c.Header("Expires", "0")
		c.Next()
	}
}

func enableCorsOriginReflectionConfig(router *gin.RouterGroup) {
	allowedHeaders := []string{"Origin", "Content-Type", "Authorization", "X-DNS-Key-Name", "X-DNS-Key-Algorithm", "X-DNS-Key", "X-Dummy-Auth-User"}

	corsConfig := cors.Config{
		AllowOriginFunc: func(origin string) bool {
			return true
		},
		AllowCredentials: true,
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders:     allowedHeaders,
		MaxAge:           1 * time.Hour,
	}

	router.Use(cors.New(corsConfig))

	router.OPTIONS("/*path", func(c *gin.Context) {
		origin := c.Request.Header.Get("Origin")
		if origin != "" {
			c.Header("Access-Control-Allow-Origin", origin)
		}
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", strings.Join(allowedHeaders, ", "))
		c.Header("Access-Control-Allow-Credentials", "true")
		c.Header("Access-Control-Max-Age", fmt.Sprint(int(time.Hour.Seconds())))
		c.Status(http.StatusNoContent)
	})
}

func parsePagination(c *gin.Context) (int, int, error) {
	limit := defaultPageLimit
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		parsedLimit, err := strconv.Atoi(raw)
		if err != nil {
			return 0, 0, err
		}
		if parsedLimit <= 0 {
			return 0, 0, fmt.Errorf("limit must be positive")
		}
		if parsedLimit > maxPageLimit {
			parsedLimit = maxPageLimit
		}
		limit = parsedLimit
	}

	offset := 0
	if raw := strings.TrimSpace(c.Query("offset")); raw != "" {
		parsedOffset, err := strconv.Atoi(raw)
		if err != nil {
			return 0, 0, err
		}
		if parsedOffset < 0 {
			return 0, 0, fmt.Errorf("offset must be non-negative")
		}
		offset = parsedOffset
	}

	return limit, offset, nil
}
