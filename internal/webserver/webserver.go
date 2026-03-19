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
	"go.uber.org/zap"
)

const (
	defaultPageLimit = 100
	maxPageLimit     = 500
)

// SetupConfig defines required dependencies for constructing the HTTP router.
type SetupConfig struct {
	DevMode        bool
	Log            *zap.SugaredLogger
	StaticConfig   StaticConfig
	ResourceAPI    ResourceAPIConfig
	AuthMiddleware gin.HandlerFunc
}

// ResourceAPIService provides business operations consumed by HTTP handlers.
type ResourceAPIService interface {
	// Returns delegations created by the given user (CreatedBy matches userEmail). Supports pagination.
	// Group search operation
	SearchGroupTokens(query string, limit int) (common.TokenList, error)

	// Request management operations
	ListRequestsBy(userEmail string, limit, offset int) ([]Request, error)
	ListRequestsManagedBy(userEmail string, userTokens common.TokenList, limit, offset int) ([]Request, error)

	CreateRequest(req CreateRequestRequest, actor string, userEmail string, userTokens common.TokenList) (Request, error)
	UpdateRequest(id string, req UpdateRequestRequest, actor string) (Request, error)
	ApproveRequest(id string, req ApproveRequestRequest, actor string, userEmail string, userTokens common.TokenList) (Request, error)
	RejectRequest(id string, req RejectRequestRequest, actor string) (Request, error)
	ReleaseRequest(id string, actor string) (Request, error)

	// Delegation management operations
	GetDelegationsBy(userTokens common.TokenList, limit, offset int) ([]Delegation, error)
	GetDelegationsFor(userTokens common.TokenList, limit, offset int) ([]Delegation, error)
	CreateDelegation(req CreateDelegationRequest, userEmail string) (Delegation, error)
	UpdateDelegation(id string, req UpdateDelegationRequest, userEmail string) (Delegation, error)
	DeleteDelegation(id string, userEmail string) error

	//Role switch related operations
	GetUserGroupSwitchForActor(actorEmail string) *string
	SetUserGroupSwitchForActor(actorEmail, groupToken string) error
	ClearUserGroupSwitchForActor(actorEmail string)
	ResolveEffectiveUserTokens(actorEmail string, originalTokens common.TokenList) common.TokenList
}

// ResourceAPIConfig configures resource API route registration.
type ResourceAPIConfig struct {
	RoleSwitchGroups    common.TokenList
	ResourceDefinitions []ResourceDefinition
	Service             ResourceAPIService
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
	RegisterResourceApiRoutes(apiV1Group, cfg.ResourceAPI)

	return router
}

// RegisterResourceApiRoutes wires all resource-management API endpoints.
func RegisterResourceApiRoutes(v1 *gin.RouterGroup, cfg ResourceAPIConfig) *gin.RouterGroup {
	v1.GET("/config", getConfig(cfg))

	roleSwitch := v1.Group("/role-switch")
	{
		roleSwitch.GET("", getRoleSwitch(cfg))
		roleSwitch.PUT("", setRoleSwitch(cfg))
		roleSwitch.DELETE("", clearRoleSwitch(cfg))
	}

	delegations := v1.Group("/delegations")
	{
		delegations.GET("/delegated-to-me", listDelegationsToMe(cfg))
		delegations.GET("/made-by-me", listDelegationsMadeByMe(cfg))
		delegations.POST("", createDelegation(cfg))
		delegations.PUT(":id", updateDelegation(cfg))
		delegations.DELETE(":id", deleteDelegation(cfg))
	}

	groups := v1.Group("/groups")
	{
		groups.GET("/search", searchGroups(cfg))
	}

	requests := v1.Group("/requests")
	{
		requests.GET("", listRequests(cfg))
		requests.POST("", createRequest(cfg))
		requests.PUT("/:id", updateRequest(cfg))
		requests.POST("/:id/approve", approveRequest(cfg))
		requests.POST("/:id/reject", rejectRequest(cfg))
		requests.POST("/:id/release", releaseRequest(cfg))
	}

	return v1
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
	allowedHeaders := []string{"Origin", "Content-Type", "Authorization", "X-DNS-Key-Name", "X-DNS-Key-Algorithm", "X-DNS-Key"}

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
