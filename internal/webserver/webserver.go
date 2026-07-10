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
	ProjectAPI   ProjectAPIConfig
	// Reconciler is optional; when nil the /v1/admin/reconcile endpoints are omitted.
	Reconciler ReconcilerAPI
	// RootAdminTokens is the set of tokens whose holders may access the reconciler admin endpoints.
	// Requests that carry none of these tokens receive 403 Forbidden.
	RootAdminTokens common.TokenList
	AuthMiddleware  gin.HandlerFunc
}

type DelegationStrategy struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// ProjectConfigResponse contains system-wide project configuration.
type ProjectConfigResponse struct {
	Projects             []common.ManagedProject `json:"projects"`
	OpenstackRoles       []string                `json:"openstackRoles"`
	DelegationStrategies []DelegationStrategy    `json:"delegationStrategies"`
	DummyDevUsers        []string                `json:"dummyDevUsers,omitempty"`
}

// ProjectAPIService provides business operations consumed by HTTP handlers.
type ProjectAPIService interface {
	// Group search operation
	SearchGroupTokens(query string, limit int) (common.TokenList, error)

	// Project management operations
	GetProjectByID(id string, userTokens common.TokenList) (*common.Project, error)
	ListProjectsBy(userEmail string, limit, offset int) ([]common.Project, error)
	ListProjectsManagedBy(userEmail string, userTokens common.TokenList, limit, offset int) ([]common.Project, error)

	CreateProject(req CreateProjectRequest, actor string, userEmail string, userTokens common.TokenList) (common.Project, error)
	UpdateProject(id string, req UpdateProjectRequest, actor string, userTokens common.TokenList) (common.Project, error)
	ApproveProject(id string, req ApproveProjectRequest, actor string, userEmail string, userTokens common.TokenList) (common.Project, error)
	RejectProject(id string, req RejectProjectRequest, actor string, userTokens common.TokenList) (common.Project, error)
	ReleaseProject(id string, actor string, userTokens common.TokenList) (common.Project, error)
	MarkProjectForPromotion(id string, req PromoteProjectRequest, actor string, userTokens common.TokenList) (common.Project, error)

	// Delegation management operations
	GetDelegationsByAdminScope(userTokens common.TokenList, limit, offset int) ([]common.Delegation, error)
	GetDelegationsDelegatedToMe(userTokens common.TokenList, limit, offset int) ([]common.Delegation, error)
	ListDelegationsEligibleForMe(userTokens common.TokenList, limit, offset int) ([]common.Delegation, error)
	ListDelegationsEligibleForOwner(callerTokens common.TokenList, ownerTokens common.TokenList, limit, offset int) ([]common.Delegation, error)
	CreateDelegation(req CreateDelegationRequest, userEmail string, userTokens common.TokenList) (common.Delegation, error)
	UpdateDelegation(id string, req UpdateDelegationRequest, userEmail string, userTokens common.TokenList) (common.Delegation, error)
	DeleteDelegation(id string, userEmail string, userTokens common.TokenList) error

	// Token eligibility rule operations
	GetMyEligibilityRules(userTokens common.TokenList) ([]common.TokenEligibilityRule, error)
	SetEligibilityRule(ownerToken string, eligibleRequesters common.TokenList, actorEmail string, userTokens common.TokenList) (common.TokenEligibilityRule, error)
	DeleteEligibilityRule(ownerToken string, actorEmail string, userTokens common.TokenList) error

	//Role switch related operations
	GetUserGroupSwitchForActor(actorEmail string) *string
	SetUserGroupSwitchForActor(actorEmail, groupToken string) error
	SetUserImpersonationForActor(actorEmail, targetEmail string) error
	ClearUserGroupSwitchForActor(actorEmail string)
	ResolveEffectiveUserTokens(actorEmail string, originalTokens common.TokenList) common.TokenList
	ResolveEffectiveEmail(actorEmail string) string
	ListAssumableIdentities() ([]common.Identity, error)
}

// ProjectAPIConfig configures resource API route registration.
type ProjectAPIConfig struct {
	RoleSwitchGroups   common.TokenList
	ProjectDefinitions []common.ManagedProject
	Service            ProjectAPIService
	DummyDevUsers      []string
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

	// Register API routes with the provided resource service and role switch groups configuration
	RegisterProjectApiRoutes(apiV1Group, cfg.ProjectAPI, cfg.Log)

	// Always register reconciler admin endpoints so CORS headers are present even
	// when the reconciler is disabled. Handlers return 503 when Reconciler is nil.
	RegisterReconcilerRoutes(apiV1Group, cfg.Reconciler, cfg.RootAdminTokens, cfg.Log)

	return router
}

// RegisterProjectApiRoutes wires all resource-management API endpoints.
func RegisterProjectApiRoutes(v1 *gin.RouterGroup, cfg ProjectAPIConfig, log *zap.SugaredLogger) *gin.RouterGroup {
	v1.Use(EffectiveAuthMiddleware(cfg.Service))

	v1.GET("/config", getConfig(cfg))

	roleSwitch := v1.Group("/role-switch")
	{
		roleSwitch.GET("", getRoleSwitch(cfg))
		roleSwitch.PUT("", setRoleSwitch(cfg))
		roleSwitch.DELETE("", clearRoleSwitch(cfg))
		roleSwitch.GET("/identities", listRoleSwitchIdentities(cfg))
	}

	delegations := v1.Group("/delegations")
	{
		delegations.GET("/delegated-to-me", listDelegationsToMe(cfg))
		delegations.GET("/made-by-me", listDelegationsMadeByMe(cfg))
		delegations.GET("/eligible-for-me", listDelegationsEligibleForMe(cfg))
		delegations.GET("/eligible-for-owner", listDelegationsEligibleForOwner(cfg))
		delegations.POST("", createDelegation(cfg))
		delegations.PUT("/:id", updateDelegation(cfg))
		delegations.DELETE("/:id", deleteDelegation(cfg))
	}

	groups := v1.Group("/groups")
	{
		groups.GET("/search", searchGroups(cfg))
		groups.GET("/mine", listMyGroups(cfg))
	}

	projects := v1.Group("/projects")
	{
		projects.GET("/mine", listMyProjects(cfg))
		projects.GET("/manage", listProjectsToManage(cfg))
		projects.GET("/:id", getProject(cfg))
		projects.POST("", createProject(cfg))
		projects.PUT("/:id", updateProject(cfg))
		projects.POST("/:id/approve", approveProject(cfg))
		projects.POST("/:id/reject", rejectProject(cfg))
		projects.POST("/:id/release", releaseProject(cfg))
		projects.POST("/:id/promote", markProjectForPromotion(cfg))
	}

	eligibility := v1.Group("/eligibility")
	{
		eligibility.GET("", listMyEligibilityRules(cfg))
		eligibility.PUT("/:token", setEligibilityRule(cfg))
		eligibility.DELETE("/:token", deleteEligibilityRule(cfg))
	}

	return v1
}

// getConfig returns the system-wide resource configuration.
//
//	@Summary		Get resource configuration
//	@Description	Retrieves system-wide configuration including resource types and roles.
//	@Tags			config
//	@Produce		json
//	@Security		Bearer
//	@Success		200	{ProjectConfigResponse}	resourceConfig	"Resource configuration."
//	@ID				getConfig
//	@Router			/v1/config [get]
func getConfig(cfg ProjectAPIConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		resources := make([]common.ManagedProject, 0, len(cfg.ProjectDefinitions))
		for _, r := range cfg.ProjectDefinitions {
			if r.ShowOnUI {
				resources = append(resources, r)
			}
		}

		// Set default delegation strategies
		delegationStrategies := []DelegationStrategy{
			{Value: common.DelegationStrategyPool, Label: "Pool (Shared, manual approval)"},
			{Value: common.DelegationStrategyAllowance, Label: "Allowance (Per-user, auto-approve)"},
		}

		// Default OpenStack roles to be used in the frontend
		openstackRoles := []string{"admin", "member", "reader"}

		// Return the static configuration
		config := ProjectConfigResponse{
			Projects:             resources,
			DelegationStrategies: delegationStrategies,
			OpenstackRoles:       openstackRoles,
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
