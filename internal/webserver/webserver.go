package webserver

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-contrib/cors"
	ginzap "github.com/gin-contrib/zap"
	"github.com/gin-gonic/gin"
	"github.com/pfisterer/cloud-self-service-golib/logging"
	"github.com/pfisterer/openstack-management-api/internal/common"
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
	// Ready reports whether the reconciler has a live OpenStack connection.
	// It is false while the connection is still being retried, which is a
	// different thing from the reconciler being switched off — the API says so
	// rather than presenting a configured-but-unreachable cloud as "disabled".
	Ready() bool
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
	// Tokens configures /v1/tokens. A nil Service omits the endpoints.
	Tokens TokenConfig
	// CORSAllowedOrigins are the browser origins allowed to call /v1
	// cross-origin. Empty means none — see enableCors.
	CORSAllowedOrigins []string
}

// ConfigResponse contains system-wide resource configuration for the frontend.
type ConfigResponse struct {
	Resources      []uiResource `json:"resources"`
	OpenstackRoles []string     `json:"openstackRoles"`
	DummyDevUsers  []string     `json:"dummyDevUsers,omitempty"`
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
	CreateNode(req tree.CreateNodeRequest, actor tree.Actor, userEmail string, userTokens common.TokenList) (tree.Node, error)
	UpdateNode(id string, req tree.UpdateNodeRequest, actor tree.Actor, userTokens common.TokenList) (tree.Node, error)
	RequestChange(id string, req tree.ChangeNodeRequest, actor tree.Actor, userTokens common.TokenList) (tree.Node, error)
	ApproveNode(id string, req tree.ApproveNodeRequest, actor tree.Actor, userTokens common.TokenList) (tree.Node, error)
	RejectNode(id string, req tree.RejectNodeRequest, actor tree.Actor, userTokens common.TokenList) (tree.Node, error)
	ReleaseNode(id string, actor tree.Actor, userTokens common.TokenList) (tree.Node, error)
	ReparentNode(id string, req tree.ReparentNodeRequest, actor tree.Actor, userTokens common.TokenList) (tree.Node, error)
	TransferOwner(id string, req tree.TransferOwnerRequest, actor tree.Actor, userTokens common.TokenList) (tree.Node, error)
	PromoteNode(id string, req tree.PromoteNodeRequest, actor tree.Actor, userTokens common.TokenList) (tree.Node, error)
	DeleteNode(id string, actor tree.Actor, userTokens common.TokenList) error

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
	// A function, not a bool: the reconciler may still be connecting when the
	// server starts, and a value frozen at startup would tell every client for
	// the rest of the pod's life that provisioning does not exist.
	ProvisioningEnabled func() bool
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
	ginLogWriter := &logging.Writer{Logger: cfg.Log, Level: cfg.Log.Level()}
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

	// Cross-origin access for the API is restricted to configured origins.
	enableCors(apiV1Group, cfg.CORSAllowedOrigins, cfg.DevMode, cfg.Log)

	// Apply authentication middleware to API routes if provided
	if cfg.AuthMiddleware != nil {
		apiV1Group.Use(cfg.AuthMiddleware)
	}

	// The read-only rule for REST, mounted here rather than inside the auth
	// middleware: "anything but GET is a write" is true of these routes and of
	// nothing else, so it belongs to the group it describes.
	apiV1Group.Use(RejectWritesForReadOnlyTokens(cfg.Log))

	// Register API routes with the provided tree service and role switch groups configuration
	RegisterApiRoutes(apiV1Group, cfg.API, cfg.Log)

	// After RegisterApiRoutes: the token handlers read the AuthContext that
	// EffectiveAuthMiddleware sets there.
	RegisterTokenRoutes(apiV1Group, cfg.Tokens, cfg.Log)

	// Always register reconciler admin endpoints so CORS headers are present even
	// when the reconciler is disabled. Handlers return 503 when Reconciler is nil.
	RegisterReconcilerRoutes(apiV1Group, cfg.Reconciler, cfg.RootAdminTokens, cfg.Log)

	// The MCP endpoint: same authentication as /v1, deliberately WITHOUT
	// RejectWritesForReadOnlyTokens. Every MCP call is a POST, so the method
	// cannot say whether an operation writes — the tool does, and mcp.go checks
	// it there. No CORS: this is for a local MCP client, not a browser.
	mcpGroup := router.Group("/mcp")
	if cfg.AuthMiddleware != nil {
		mcpGroup.Use(cfg.AuthMiddleware)
	}
	RegisterMCPRoutes(mcpGroup, cfg.API, cfg.Log)

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
//
// uiResource is what a resource looks like to the browser.
//
// A type of its own rather than the catalogue entry with the private parts
// removed. Both produce the same JSON today; the difference is what happens when
// somebody adds a field to ManagedProject. Stripping is a list of things NOT to
// send, so a new field ships by default and the omission is silent — that is how
// the OpenStack grant targets nearly went out. Here a new field reaches the
// browser only when someone writes it down.
type uiResource struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Default  int    `json:"default"`
	Min      int    `json:"min"`
	Max      int    `json:"max"`
	Unit     string `json:"unit,omitempty"`
	Message  string `json:"message,omitempty"`
	Kind     string `json:"kind,omitempty"`
	Group    string `json:"group,omitempty"`
	ShowOnUI bool   `json:"show_on_ui,omitempty"`
}

func uiResourceFrom(r common.ManagedProject) uiResource {
	return uiResource{
		ID:       r.ID,
		Name:     r.Name,
		Default:  r.Default,
		Min:      r.Min,
		Max:      r.Max,
		Unit:     r.Unit,
		Message:  r.Message,
		Kind:     r.Kind,
		Group:    r.Group,
		ShowOnUI: r.ShowOnUI,
	}
}

func getConfig(cfg APIConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		resources := make([]uiResource, 0, len(cfg.ProjectDefinitions))
		for _, r := range cfg.ProjectDefinitions {
			if !r.ShowOnUI {
				continue
			}
			resources = append(resources, uiResourceFrom(r))
		}

		// Same list the API validates authorized_users against, so the UI can
		// never offer a role the API would reject.
		openstackRoles := common.OpenstackRoles

		config := ConfigResponse{
			Resources:           resources,
			OpenstackRoles:      openstackRoles,
			ProvisioningEnabled: cfg.ProvisioningEnabled != nil && cfg.ProvisioningEnabled(),
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

// enableCors restricts cross-origin API access to allowedOrigins (exact origin
// matches, e.g. "https://selfservice.dhbw.cloud").
//
// The previous version reflected ANY origin and combined that with
// AllowCredentials — the combination a browser only tolerates because the
// reflection makes it look like a deliberate per-origin decision. It is not one:
// with the UI reachable through a BFF that turns a session cookie into a Bearer,
// a credentialed cross-origin request from an arbitrary page reaches the API
// authenticated, and reflected headers let that page READ the answer. An empty
// allowlist is therefore the correct default: in BFF mode the SPA is same-origin
// with the API and needs no CORS at all.
//
// In development the local Vite dev server (http://localhost:8084) is a genuine
// cross-origin caller, so devMode additionally allows any loopback origin —
// nobody should have to configure an allowlist to run the UI locally.
func enableCors(router *gin.RouterGroup, allowedOrigins []string, devMode bool, log *zap.SugaredLogger) {
	allowed := make(map[string]bool, len(allowedOrigins))
	for _, origin := range allowedOrigins {
		if trimmed := strings.TrimRight(strings.TrimSpace(origin), "/"); trimmed != "" {
			allowed[trimmed] = true
		}
	}

	switch {
	case devMode:
		log.Infof("CORS: development mode — allowing loopback origins plus %v", allowedOrigins)
	case len(allowed) == 0:
		log.Info("CORS: no allowed origins configured — cross-origin API access is disabled")
	default:
		log.Infof("CORS: allowing cross-origin API access from %v", allowedOrigins)
	}

	router.Use(cors.New(cors.Config{
		AllowOriginFunc: func(origin string) bool {
			origin = strings.TrimRight(origin, "/")
			return allowed[origin] || (devMode && isLoopbackOrigin(origin))
		},
		AllowCredentials: true,
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Authorization", "X-DNS-Key-Name", "X-DNS-Key-Algorithm", "X-DNS-Key", "X-Dummy-Auth-User"},
		MaxAge:           1 * time.Hour,
	}))

	// Gin runs group middleware only for requests that MATCH a route, and no
	// handler is registered for OPTIONS — without this catch-all a preflight
	// would 404 before the CORS middleware ever ran, and every allowed origin
	// would break too. The handler itself sets nothing: for an allowed origin
	// the middleware has already answered 204 with the headers, and for any
	// other origin it aborted with 403. That is the difference to the previous
	// version, whose catch-all wrote reflected headers on its own.
	router.OPTIONS("/*path", func(c *gin.Context) { c.Status(http.StatusNoContent) })
}

// isLoopbackOrigin reports whether an origin points at this machine — the dev
// server and any local tooling. Only consulted in development mode.
func isLoopbackOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
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
