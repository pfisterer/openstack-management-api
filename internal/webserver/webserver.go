package webserver

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/roles"
	"github.com/pfisterer/openstack-management-api/internal/identity"

	"github.com/gin-contrib/cors"
	ginzap "github.com/gin-contrib/zap"
	"github.com/gin-gonic/gin"
	"github.com/pfisterer/cloud-self-service-golib/authn"
	"github.com/pfisterer/cloud-self-service-golib/ginweb"
	"github.com/pfisterer/cloud-self-service-golib/logging"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/tree"
	"github.com/pfisterer/openstack-management-api/internal/core/usage"
	"github.com/pfisterer/openstack-management-api/internal/provider/openstack/catalog"
	"github.com/pfisterer/openstack-management-api/internal/provider/openstack/reconciler"
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
	// cross-origin. Empty means none — see ginweb.EnableCORS.
	CORSAllowedOrigins []string
}

// ConfigResponse contains system-wide resource configuration for the frontend.
type ConfigResponse struct {
	Resources      []uiResource `json:"resources"`
	OpenstackRoles []string     `json:"openstackRoles"`
	// ProvisioningEnabled reports whether the reconciler is running. Only then
	// does an approved project eventually get an OpenStack project — without it
	// the UI would show every project as "waiting for OpenStack" forever.
	ProvisioningEnabled bool `json:"provisioningEnabled"`
	// Providers are the providers besides OpenStack that carry projects out
	// here (e.g. aiaas): a project holding their resources gets its side there
	// on their next pass, and until then it is being set up.
	Providers []string `json:"providers"`
	// OpenstackDashboardURL is where a project can be opened in OpenStack;
	// empty when none is configured.
	OpenstackDashboardURL string `json:"openstackDashboardUrl,omitempty"`
	// Retirement says what releasing a project leads to in this deployment.
	Retirement RetirementConfig `json:"retirement"`
}

// RetirementConfig is what happens to a released project here: whether it is
// archived, when it is deleted, and what it still costs its budget meanwhile.
type RetirementConfig struct {
	// Archive: the project is archived once released.
	Archive bool `json:"archive"`
	// Delete: never, on-request, after-grace or immediately. Everything but
	// never lets a released or archived project be deleted on request.
	Delete string `json:"delete" enums:"never,on-request,after-grace,immediately"`
	// DeleteGraceDays: days from release to deletion under after-grace.
	DeleteGraceDays int `json:"deleteGraceDays"`
	// ChargeReleased: a released project still costs its budget in full.
	ChargeReleased bool `json:"chargeReleased"`
	// ChargeArchived: an archived project still costs the storage it holds.
	ChargeArchived bool `json:"chargeArchived"`
	// AIaaSDeleteGraceDays: days after a project's end (or its release, if earlier) when its Lab/AI part — workspaces and their files — is deleted; 0 = never.
	AIaaSDeleteGraceDays int `json:"aiaasDeleteGraceDays"`
}

// IdentityAPI is who a caller is and acts as: the tokens of a person acted for, and the principal search for token fields. Implemented by identity.Service.
type IdentityAPI interface {
	identity.Effective
	SearchPrincipals(query string, limit int) ([]roles.GroupSummary, []string, error)
}

// APIService provides the business operations consumed by the HTTP handlers.
// It is implemented by tree.Service.
type APIService interface {
	// Node reads / views
	GetNode(id string, userTokens common.TokenList) (*model.Node, error)
	ListChildren(parentID string, filter tree.ChildFilter, userTokens common.TokenList, limit, offset int) (tree.NodePage, error)
	ListMine(userEmail string, userTokens common.TokenList, limit, offset int) (tree.NodePage, error)
	ListMyBudgets(userTokens common.TokenList, limit, offset int) (tree.NodePage, error)
	ListEligibleForMe(userTokens common.TokenList, limit, offset int) (tree.NodePage, error)
	ListEligibleForOwner(callerTokens common.TokenList, ownerTokens common.TokenList, limit, offset int) (tree.NodePage, error)
	ListToManage(userTokens common.TokenList, includeSubtree bool, limit, offset int) (tree.NodePage, error)
	SearchNodes(userTokens common.TokenList, query string, limit, offset int) (tree.NodePage, error)

	// Node lifecycle
	CreateNode(req tree.CreateNodeRequest, actor model.Actor, userEmail string, userTokens common.TokenList) (model.Node, error)
	UpdateNode(id string, req tree.UpdateNodeRequest, actor model.Actor, userTokens common.TokenList) (model.Node, error)
	RequestChange(id string, req tree.ChangeNodeRequest, actor model.Actor, userTokens common.TokenList) (model.Node, error)
	ApproveNode(id string, req tree.ApproveNodeRequest, actor model.Actor, userTokens common.TokenList) (model.Node, error)
	RejectNode(id string, req tree.RejectNodeRequest, actor model.Actor, userTokens common.TokenList) (model.Node, error)
	ReleaseNode(id string, req tree.ReleaseNodeRequest, actor model.Actor, userTokens common.TokenList) (model.Node, error)
	RequestDeletion(id string, actor model.Actor, userTokens common.TokenList) (model.Node, error)
	ReparentNode(id string, req tree.ReparentNodeRequest, actor model.Actor, userTokens common.TokenList) (model.Node, error)
	TransferOwner(id string, req tree.TransferOwnerRequest, actor model.Actor, userTokens common.TokenList) (model.Node, error)
	SetAllocation(id string, req tree.AllocationRequest, actor model.Actor, userTokens common.TokenList) (model.Node, error)
	SetAttributes(id string, req tree.SetAttributesRequest, actor model.Actor, userTokens common.TokenList) (model.Node, error)
	RemoveExternalGroup(id, groupID string, actor model.Actor, userTokens common.TokenList) (model.Node, error)
	AllocationSources(id string, userTokens common.TokenList) ([]model.Node, error)
	UsageScope(id string, userTokens common.TokenList) (*model.Node, error)
	AttributesVisible(n *model.Node, userTokens common.TokenList) (bool, error)
	PromoteNode(id string, req tree.PromoteNodeRequest, actor model.Actor, userTokens common.TokenList) (model.Node, error)
	DeleteNode(id string, actor model.Actor, userTokens common.TokenList) error
}

// APIConfig configures API route registration.
type APIConfig struct {
	// ActAsTokens may act on behalf of another person (X-Act-As): the root admins.
	ActAsTokens common.TokenList
	// Catalog is the resource catalogue, read per request: root admins change
	// its availabilities at runtime.
	Catalog common.ResourceCatalog
	// CatalogAdmin runs the root admins' catalogue steps; nil leaves the
	// routes answering 503.
	CatalogAdmin *catalog.Admin
	Service      APIService
	// Identity is who a caller is and acts as.
	Identity IdentityAPI
	// ProvisioningEnabled mirrors "the reconciler is configured and running".
	// A function, not a bool: the reconciler may still be connecting when the
	// server starts, and a value frozen at startup would tell every client for
	// the rest of the pod's life that provisioning does not exist.
	ProvisioningEnabled func() bool
	// Providers besides OpenStack that carry projects out (see ConfigResponse).
	Providers []string
	// OpenstackDashboardURL is passed through to the UI (see ConfigResponse).
	OpenstackDashboardURL string
	// Retirement is passed through to the UI (see ConfigResponse).
	Retirement RetirementConfig
	// Usage holds the daily consumption rows; nil omits the usage endpoints'
	// data (they answer with empty reports).
	Usage UsageReader
	// UsagePrices put a euro value on the root admins' evaluation; nil for none.
	UsagePrices *usage.Prices
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
		router.Use(ginweb.DisableCaching())
	}

	// Pipe Gin internals through Zap logger outputs.
	ginLogWriter := &logging.Writer{Logger: cfg.Log, Level: cfg.Log.Level()}
	gin.DefaultWriter = ginLogWriter
	gin.DefaultErrorWriter = ginLogWriter
	router.Use(ginzap.RecoveryWithZap(cfg.Log.Desugar(), true))

	// Setup static file serving routes
	staticGroup := router.Group("/")
	staticGroup.Use(cors.Default())
	// These routes serve the landing page, swagger.json and config.json — all of
	// which embed the running version. Served uncached so a deploy is visible on
	// the next reload instead of whenever a browser cache expires. (The generated
	// TypeScript client used to be served here too and had the same problem
	// harder — a stale cached SDK once masked the identity
	// picker; it is an npm package consumed at build time now.) In DevMode the
	// whole router already gets this middleware.
	if !cfg.DevMode {
		staticGroup.Use(ginweb.DisableCaching())
	}
	RegisterStaticRoutes(staticGroup, cfg.StaticConfig)

	// Setup API v1 routes
	apiV1Group := router.Group("/v1")

	// Cross-origin access for the API is restricted to configured origins.
	ginweb.EnableCORS(apiV1Group, ginweb.CORSOptions{
		AllowedOrigins: cfg.CORSAllowedOrigins,
		DevMode:        cfg.DevMode,
		AllowHeaders:   []string{"Origin", "Content-Type", "Authorization", "X-DNS-Key-Name", "X-DNS-Key-Algorithm", "X-DNS-Key", authn.ActAsHeader},
		Log:            cfg.Log,
	})

	// Apply authentication middleware to API routes if provided
	if cfg.AuthMiddleware != nil {
		apiV1Group.Use(cfg.AuthMiddleware)
	}

	// The read-only rule for REST, mounted here rather than inside the auth
	// middleware: "anything but GET is a write" is true of these routes and of
	// nothing else, so it belongs to the group it describes.
	apiV1Group.Use(ginweb.RejectWritesForReadOnlyTokens(cfg.Log))

	// Register API routes with the provided tree service and the act-as allowlist
	RegisterApiRoutes(apiV1Group, cfg.API, cfg.Log)

	// After RegisterApiRoutes: the token handlers read the AuthContext that
	// EffectiveAuthMiddleware sets there.
	RegisterTokenRoutes(apiV1Group, cfg.Tokens, cfg.Log)

	// Always register reconciler admin endpoints so CORS headers are present even
	// when the reconciler is disabled. Handlers return 503 when Reconciler is nil.
	RegisterReconcilerRoutes(apiV1Group, cfg.Reconciler, cfg.RootAdminTokens, cfg.Log)
	RegisterUsageAdminRoutes(apiV1Group, cfg.API, cfg.RootAdminTokens, cfg.Log)
	RegisterCatalogAdminRoutes(apiV1Group, cfg.API, cfg.RootAdminTokens, cfg.Log)

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

// resources is the current catalogue; none where no catalogue is configured.
func (cfg APIConfig) resources() []common.Resource {
	if cfg.Catalog == nil {
		return nil
	}
	return cfg.Catalog.Resources()
}

// RegisterApiRoutes wires all resource-management API endpoints.
func RegisterApiRoutes(v1 *gin.RouterGroup, cfg APIConfig, log *zap.SugaredLogger) *gin.RouterGroup {
	v1.Use(identity.EffectiveAuthMiddleware(cfg.Identity, cfg.ActAsTokens, log))

	v1.GET("/config", getConfig(cfg))
	v1.GET("/act-as", getActAs(cfg))

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
		nodes.POST("/:id/deletion", requestNodeDeletion(cfg))
		nodes.POST("/:id/reparent", reparentNode(cfg))
		nodes.POST("/:id/transfer-owner", transferNodeOwner(cfg))
		nodes.PUT("/:id/allocations", setNodeAllocation(cfg))
		nodes.PUT("/:id/attributes", setNodeAttributes(cfg))
		nodes.DELETE("/:id/external-groups/:groupId", removeNodeExternalGroup(cfg))
		nodes.GET("/:id/allocation-sources", listAllocationSources(cfg))
		nodes.GET("/:id/usage", getNodeUsage(cfg))
		nodes.POST("/:id/promote", promoteNode(cfg))
	}

	return v1
}

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
	// GrantType is the kind of an availability (network, flavor, image), which
	// tells how its use is counted — devices on a network, servers otherwise.
	// The type only: the target stays private.
	GrantType string `json:"grant_type,omitempty"`
	// Provider is the system that carries the resource out (openstack,
	// workspaces), so the UI can group by it.
	Provider string `json:"provider,omitempty"`
	// Area is the concern it serves (cloud, workspaces, llm): the UI arranges projects and forms by it.
	Area string `json:"area,omitempty"`
	// Window is the time window of an amount per window (kind window): 24h, 7d or 30d.
	Window string `json:"window,omitempty"`
}

func uiResourceFrom(r common.Resource) uiResource {
	return uiResource{
		ID:        r.ID,
		Name:      r.Name,
		Default:   r.Default,
		Min:       r.Min,
		Max:       r.Max,
		Unit:      r.Unit,
		Message:   r.Message,
		Kind:      r.Kind,
		Group:     r.Group,
		ShowOnUI:  r.ShowOnUI,
		GrantType: r.GrantType,
		Provider:  r.Provider,
		Area:      r.InArea(),
		Window:    r.Window,
	}
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
		all := cfg.resources()
		resources := make([]uiResource, 0, len(all))
		for _, r := range all {
			// A withdrawn availability is still known but offered to nobody.
			if !r.ShowOnUI || r.Withdrawn {
				continue
			}
			resources = append(resources, uiResourceFrom(r))
		}

		// Same list the API validates authorized_users against, so the UI can
		// never offer a role the API would reject.
		openstackRoles := common.OpenstackRoles

		config := ConfigResponse{
			Resources:             resources,
			OpenstackRoles:        openstackRoles,
			ProvisioningEnabled:   cfg.ProvisioningEnabled != nil && cfg.ProvisioningEnabled(),
			Providers:             append([]string{}, cfg.Providers...),
			OpenstackDashboardURL: cfg.OpenstackDashboardURL,
			Retirement:            cfg.Retirement,
		}
		c.JSON(http.StatusOK, config)
	}
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
