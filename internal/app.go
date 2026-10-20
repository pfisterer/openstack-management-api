package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/nodestore"
	"github.com/pfisterer/openstack-management-api/internal/core/roles"
	"github.com/pfisterer/openstack-management-api/internal/identity"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/cloud-self-service-golib/logging"
	"github.com/pfisterer/cloud-self-service-golib/oidcauth"
	"github.com/pfisterer/cloud-self-service-golib/token"
	"github.com/pfisterer/cloud-self-service-golib/tokengorm"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/tree"
	"github.com/pfisterer/openstack-management-api/internal/core/usage"
	"github.com/pfisterer/openstack-management-api/internal/dev"
	"github.com/pfisterer/openstack-management-api/internal/provider/aiaas"
	"github.com/pfisterer/openstack-management-api/internal/provider/openstack/catalog"
	osclient "github.com/pfisterer/openstack-management-api/internal/provider/openstack/client"
	"github.com/pfisterer/openstack-management-api/internal/provider/openstack/reconciler"
	"github.com/pfisterer/openstack-management-api/internal/roleprovider"
	"github.com/pfisterer/openstack-management-api/internal/webserver"
	"go.uber.org/zap"
)

// apiTokenPrefix is what tells this service's tokens apart from an OIDC bearer
// token, and from another service's tokens. It matches the prefix the auth
// middleware routes on.
const apiTokenPrefix = "os_mgt_"

func configureStores(cfg *StorageConfiguration, log *zap.SugaredLogger) (nodestore.Store, *token.Service, usage.Store, catalog.Store, aiaas.ClassStore, error) {
	storageType := strings.ToLower(strings.TrimSpace(cfg.Type))

	switch storageType {

	case "memory":
		// Memory mode is intended for local development and tests. Tokens do
		// not survive a restart here, which is fine for the one and unusable
		// for the other.
		return nodestore.NewInMemoryStore(log), token.NewService(apiTokenPrefix, token.NewMemoryStore()), usage.NewMemoryStore(), &catalog.MemoryStore{}, &aiaas.MemoryClassStore{}, nil

	case "postgres":
		store, err := nodestore.NewPostgresStore(cfg.ConnectionString, log)
		if err != nil {
			return nil, nil, nil, nil, nil, fmt.Errorf("postgres storage: %w", err)
		}
		// Same connection pool: this Postgres is shared with PowerDNS and its
		// connection budget is the reason NewPostgresStore caps it at all.
		tokens, err := tokengorm.NewService(apiTokenPrefix, store.DB())
		if err != nil {
			return nil, nil, nil, nil, nil, fmt.Errorf("token storage: %w", err)
		}
		used, err := usage.NewPostgresStore(store.DB())
		if err != nil {
			return nil, nil, nil, nil, nil, fmt.Errorf("usage storage: %w", err)
		}
		availabilities, err := catalog.NewPostgresStore(store.DB())
		if err != nil {
			return nil, nil, nil, nil, nil, fmt.Errorf("catalog storage: %w", err)
		}
		gpuClasses, err := aiaas.NewPostgresClassStore(store.DB())
		if err != nil {
			return nil, nil, nil, nil, nil, err
		}
		return store, tokens, used, availabilities, gpuClasses, nil

	default:
		return nil, nil, nil, nil, nil, fmt.Errorf("unsupported storage type %q", cfg.Type)
	}
}

// tokenLookupFor adapts the token service to what the auth middleware wants.
func tokenLookupFor(tokens *token.Service) identity.TokenLookupFunc {
	return func(ctx context.Context, secret string) (identity.TokenLookupResult, error) {
		rec, err := tokens.Lookup(ctx, secret)
		if errors.Is(err, token.ErrNotFound) {
			return identity.TokenLookupResult{Found: false}, nil
		}
		if err != nil {
			return identity.TokenLookupResult{}, err
		}
		return identity.TokenLookupResult{
			Found:    true,
			Subject:  rec.Subject,
			ReadOnly: rec.ReadOnly,
		}, nil
	}
}

// configureAuthMiddleware returns the middleware and, where one exists, the
// verifier behind it — the webserver reports its state to the UI, so a login
// that cannot work says why instead of just failing.
func configureAuthMiddleware(cfg *WebServerConfig, tokenLookup identity.TokenLookupFunc, userTokenResolver identity.UserTokenResolverFunc, log *zap.SugaredLogger) (gin.HandlerFunc, *oidcauth.Verifier, error) {

	// Setup Web server
	var authMiddleware gin.HandlerFunc
	var verifier *oidcauth.Verifier

	if cfg.DummyAuth {
		log.Warnw("DummyAuth enabled: every request is signed in as the dev user (no SSO); other people via X-Act-As", "dev_user", cfg.DevAuthUser)
		authMiddleware = dev.DummyAuthMiddleware(cfg.DevAuthUser)
	} else {

		// Create OIDC Auth Verifier. With OIDC_JWKS_URL configured this makes no
		// network call, so a provider that is down cannot stop this service
		// from starting (see the oidcauth package).
		oidcAuthVerifier, err := identity.NewOIDCAuthVerifier(identity.OIDCVerifierConfig{
			IssuerURL: cfg.OIDCIssuerURL,
			ClientID:  cfg.OIDCClientID,
			JWKSURL:   cfg.OIDCJWKSURL,
		}, log)

		if err != nil {
			log.Fatalf("Failed to initialize OIDCAuthVerifier: %v", err)
		}

		verifier = oidcAuthVerifier
		authMiddleware = identity.CombinedAuthMiddleware(oidcAuthVerifier, tokenLookup, userTokenResolver, log)
	}
	return authMiddleware, verifier, nil
}

func RunApplication() {
	// Load application configuration
	config, err := loadAppConfiguration()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load application configuration: %v\n", err)
		os.Exit(1)
	}

	// Initialize logger
	log, logger := logging.Init(config.DevMode)
	defer log.Sync()
	logger.Info("Starting OpenStack Management Application")
	logAppConfig(config, logger)

	// Fail closed: the dummy-auth bypass (every request signed in as the dev user,
	// a root admin, without any verification) must never run outside development.
	if config.WebServer.DummyAuth && !config.DevMode {
		logger.Fatal("API_DUMMY_AUTH=true is not allowed when API_MODE=production — refusing to start with an authentication bypass")
	}

	// Configure resource storage and token lookup
	nodeStore, apiTokens, usageStore, catalogStore, gpuClassStore, err := configureStores(&config.Storage, logger)
	if err != nil {
		logger.Fatal("Failed to initialize storage", zap.Error(err))
	}
	// The configured quantities plus the availabilities root admins manage in
	// the portal; everything below reads it live.
	resourceCatalog, err := catalog.Load(context.Background(), config.ProjectDefinitions, catalogStore, logger)
	if err != nil {
		logger.Fatal("Failed to load the resource catalogue", zap.Error(err))
	}
	logger.Infof("Using storage backend: %s", config.Storage.Type)

	// Create role provider based on ROLE_PROVIDER env var ("http" or "mock").
	roleProviderType, err := resolveRoleProviderType(config.RoleProvider.Type, config.DevMode)
	if err != nil {
		logger.Fatalw("Refusing to start", "error", err)
	}
	var roleProvider roles.Provider
	switch roleProviderType {
	case roleProviderHTTP:
		logger.Infow("Using HttpRoleProvider", "url", config.RoleProvider.URL)
		rp, err := roleprovider.NewHttpRoleProvider(
			config.RoleProvider.URL,
			config.RoleProvider.APIToken,
			config.ServiceTimeoutSeconds,
			logger,
		)
		if err != nil {
			logger.Fatalw("Failed to create HttpRoleProvider", zap.Error(err))
		}
		roleProvider = rp
	case roleProviderMock:
		// Built-in demo identities — for local/offline dev (pairs with dummy-auth); resolveRoleProviderType refuses it in production.
		logger.Warn("Using MockRoleProvider (built-in demo identities) — development only")
		roleProvider = dev.NewMockRoleProvider()
	}

	requestTimeout := time.Duration(config.ServiceTimeoutSeconds) * time.Second
	// Who a caller is and acts as (acting on behalf via X-Act-As, principal search).
	identitySvc := identity.NewService(nodeStore, roleProvider, requestTimeout, logger)
	// The tree sees every provider's resources as one catalogue: OpenStack's,
	// which root admins extend at runtime, the configured ones of the others,
	// and the GPU classes of the AI service, which its GPU administration adds
	// at runtime (two resources each; the classes seen are kept, so the tree
	// knows them even when the AI service does not answer at a start).
	var aiaasClient *aiaas.Client
	if config.AIaaS.URL != "" {
		aiaasClient = aiaas.NewClient(config.AIaaS.URL, config.AIaaS.Token, 30*time.Second)
	}
	gpuCatalog := aiaas.NewGPUCatalog(config.ProviderResources, gpuClassStore, aiaasClient, logger)
	if err := gpuCatalog.Load(context.Background()); err != nil {
		logger.Fatalw("Failed to load the GPU classes of the AI service", "error", err)
	}
	if aiaasClient != nil {
		refreshCtx, cancelRefresh := context.WithTimeout(context.Background(), 15*time.Second)
		if _, err := gpuCatalog.Refresh(refreshCtx); err != nil {
			logger.Warnw("GPU classes of the AI service not refreshed at the start; the ones seen before apply", "error", err)
		}
		cancelRefresh()
	}
	allResources := common.CombinedCatalog{resourceCatalog, gpuCatalog}
	treeSvc := tree.NewService(nodeStore, roleProvider, allResources.Resources(), config.RootAdminTokens, requestTimeout, config.MaxAuthorizedUsers, tree.Accounting{
		ChargeInUse:    config.ChargeOSInUse,
		ChargeReleased: config.ChargeReleased,
		ChargeArchived: config.ChargeArchived,
	}, logger)
	// A deletion request is only accepted where a reconciler will act on it.
	treeSvc.UseCatalog(allResources)
	var deleting []string
	if config.Reconciler.Enabled && reconciler.DeletesOnRequest(config.Reconciler.ReleasedDelete) {
		deleting = append(deleting, osclient.Provider)
	}
	if config.AIaaS.URL != "" {
		deleting = append(deleting, aiaas.Provider)
	}
	treeSvc.SetDeletingProviders(deleting...)
	treeSvc.SetMaxProjectsPerOwner(config.MaxProjectsPerOwner)
	treeSvc.SetMaxAuthorizedUsersOther(config.MaxAuthorizedUsersOther)

	// Bootstrap: optional mock seed into an empty store, then ensure the
	// structural root/unassigned nodes (root admin scope is synced from config).
	var mockIdentities []common.Identity
	var mockNodes []model.Node
	if config.Storage.AddMockData {
		mockIdentities, mockNodes = dev.DefaultMockTreeState()
		mockNodes = dev.WithProviderBudget(mockNodes, config.ProviderResources)
	}
	if err := treeSvc.Bootstrap(context.Background(), mockIdentities, mockNodes); err != nil {
		logger.Fatal("Failed to bootstrap resource tree", zap.Error(err))
	}

	//Create authentication middleware based on configuration.
	authMiddleware, oidcVerifier, err := configureAuthMiddleware(&config.WebServer, tokenLookupFor(apiTokens), roleProvider.GetUserTokens, logger)
	if err != nil {
		logger.Fatal("Failed to initialize authentication middleware", zap.Error(err))
	}

	// Add dummy dev users from mock data if dummy auth is enabled
	dummyDevUsers := []string{}
	if config.WebServer.DummyAuth {
		identities, _ := dev.DefaultMockTreeState()
		for _, ident := range identities {
			dummyDevUsers = append(dummyDevUsers, ident.Email)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Setup Gin web server with configured dependencies.

	// Without a reconciler nothing is checked in OpenStack, and nothing is
	// granted there either.
	catalogAdmin := &catalog.Admin{Catalog: resourceCatalog, Tree: treeSvc}

	// The AI service as the second provider: it carries out the projects that
	// hold its resources, like the reconciler does for OpenStack.
	if config.AIaaS.URL != "" {
		if config.AIaaS.Token == "" {
			logger.Fatal("AIAAS_URL is set but AIAAS_TREE_TOKEN is not")
		}
		syncer := &aiaas.Syncer{
			Tree:      treeSvc.ProviderSync(aiaas.Provider),
			Service:   aiaasClient,
			Resources: allResources.Resources,
			Interval:  time.Duration(config.AIaaS.IntervalSeconds) * time.Second,
			GraceDays: config.AIaaS.DeleteGraceDays,
			Log:       logger,
		}
		logger.Infow("Starting the AI service provider", "url", config.AIaaS.URL, "interval_seconds", config.AIaaS.IntervalSeconds)
		go syncer.Run(ctx)
		// New GPU classes reach the tree within AIAAS_CLASS_INTERVAL_SECONDS (30 s); the root gets them unlimited, to be granted down like any quantity.
		gpuCatalog.OnNew = treeSvc.AdoptNewResources
		gpuCatalog.OnRename = treeSvc.RenameResources
		go gpuCatalog.Run(ctx, time.Duration(max(config.AIaaS.ClassIntervalSeconds, 5))*time.Second)
	}

	// A local ReconcilerAPI variable avoids passing a typed nil as the interface,
	// which would make cfg.Reconciler != nil even when no reconciler exists.
	var reconcilerAPI webserver.ReconcilerAPI
	if config.Reconciler.Enabled {
		if err := reconciler.ValidateGroupPrefix(config.Reconciler.GroupPrefix); err != nil {
			logger.Fatalw("Invalid reconciler configuration", "error", err)
		}
		if err := reconciler.ValidateReleasedDelete(config.Reconciler.ReleasedDelete); err != nil {
			logger.Fatalw("Invalid reconciler configuration", "error", err)
		}
		if err := reconciler.ValidatePurgeSkipStages(config.Reconciler.PurgeSkipStages); err != nil {
			logger.Fatalw("Invalid reconciler configuration", "error", err)
		}
		logger.Infow("Starting reconciler", "interval_seconds", config.Reconciler.IntervalSeconds, "dry_run", config.Reconciler.DryRun)

		// Connecting to OpenStack is retried in the background rather than done
		// once here: a cloud that is away for a minute must not disable
		// provisioning until somebody restarts the pod. See reconciler.Supervisor.
		supervisor := &reconciler.Supervisor{}
		supervisor.ConnectAndStart(ctx, func() (*reconciler.Reconciler, error) {
			osClient, osErr := osclient.NewFromConfig(config.Openstack, log, logger)
			if osErr != nil {
				return nil, osErr
			}
			osClient.SetTagConfig(config.Reconciler.ManagedProjectTag, config.Reconciler.ResourceIDTagPrefix)
			osClient.SetFederationConfig(config.Openstack.FederatedProvisioning, config.Openstack.FederatedIdPID, config.Openstack.FederatedProtocolID, config.Openstack.FederatedDomainID)

			reconcilerCfg := reconciler.Config{
				Interval:                 time.Duration(config.Reconciler.IntervalSeconds) * time.Second,
				GroupPrefix:              config.Reconciler.GroupPrefix,
				ScopeParentID:            config.Reconciler.ScopeParentID,
				ScopeParentName:          config.Reconciler.ScopeParentName,
				DryRun:                   config.Reconciler.DryRun,
				ReleasedArchive:          config.Reconciler.ReleasedArchive,
				ReleasedDelete:           config.Reconciler.ReleasedDelete,
				ReleasedDeleteGraceDays:  config.Reconciler.ReleasedDeleteGraceDays,
				ArchivedTagPrefix:        config.Reconciler.ArchivedTagPrefix,
				PurgeDNSAndObjectStorage: config.Reconciler.PurgeDNSAndObjectStorage,
				PurgeSkipStages:          config.Reconciler.PurgeSkipStages,
				DeleteOrphanedUsers:      config.Reconciler.DeleteOrphanedUsers,
				PendingDeletionTagPrefix: config.Reconciler.PendingDeletionTagPrefix,
				ContactTagPrefix:         config.Reconciler.ContactTagPrefix,
				TerminationTagPrefix:     config.Reconciler.TerminationTagPrefix,
				ManagedProjectTag:        config.Reconciler.ManagedProjectTag,
				StatusTagPrefix:          config.Reconciler.StatusTagPrefix,
			}
			rec := reconciler.New(treeSvc.ProviderSync(osclient.Provider), osClient, reconcilerCfg, config.ProjectDefinitions, roleProvider, logger)
			rec.UseCatalog(resourceCatalog)
			if config.Reconciler.UsageEnabled {
				books := usage.NewBooks(usageStore, nodeStore, osclient.CoreResources(config.ProjectDefinitions), osclient.Provider, logger)
				books.BackfillDays = config.Reconciler.UsageBackfillDays
				rec.SetUsage(books, reconciler.NewUsageSource(osClient, config.Reconciler.UsagePublicNetworks))
			}
			return rec, nil
		}, logger)
		var _ webserver.ReconcilerAPI = supervisor
		reconcilerAPI = supervisor
		catalogAdmin.OpenStack = supervisor
	} else {
		logger.Info("Reconciler disabled (set RECONCILER_ENABLED=true to enable)")
	}

	router := webserver.SetupGinWebserver(webserver.SetupConfig{
		DevMode: config.DevMode,
		Log:     logger,
		StaticConfig: webserver.StaticConfig{
			OIDCIssuerURL: config.WebServer.OIDCIssuerURL,
			OIDCClientID:  config.WebServer.OIDCClientID,
			// Asked per request: whether the identity provider answers is a
			// state that changes while a pod runs.
			SignInAvailable: func() bool { return oidcVerifier == nil || !oidcVerifier.KeysUnavailable() },
		},
		API: webserver.APIConfig{
			ActAsTokens:  config.RootAdminTokens,
			Catalog:      allResources,
			CatalogAdmin: catalogAdmin,
			Service:      treeSvc,
			Identity:     identitySvc,
			// Asked per request: the reconciler may still be connecting.
			ProvisioningEnabled:   func() bool { return reconcilerAPI != nil && reconcilerAPI.Ready() },
			Providers:             activeProviders(config),
			OpenstackDashboardURL: config.WebServer.OpenstackDashboardURL,
			Retirement:            retirementConfig(config),
			Usage:                 usageStore,
			UsagePrices:           config.UsagePrices,
		},
		Tokens: webserver.TokenConfig{
			Service: apiTokens,
			TTL: token.TTLPolicy{
				Default:    time.Duration(config.WebServer.APITokenTTLHours) * time.Hour,
				Max:        time.Duration(config.WebServer.APITokenMaxTTLHours) * time.Hour,
				AllowNever: config.WebServer.APITokenAllowNeverExpires,
			},
		},
		Reconciler:         reconcilerAPI,
		RootAdminTokens:    config.RootAdminTokens,
		AuthMiddleware:     authMiddleware,
		CORSAllowedOrigins: config.WebServer.CORSAllowedOrigins,
	})

	// Start the Web server. An http.Server of its own rather than router.Run, which sets no timeouts: a client that sends its headers or body one byte a minute would hold a connection for ever.
	server := newHTTPServer(config.WebServer.GinBindString, router)
	logger.Infow("Listening", "address", server.Addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Fatal("Failed to start server", zap.Error(err))
	}

	logger.Info("Application completed successfully")
}

// newHTTPServer is the server the API runs in, with timeouts. The write timeout leaves room for the slowest legitimate answer — a usage report over a year, a listing that computes the rollup of a large subtree — and the service's own request deadline (SERVICE_TIMEOUT_SECONDS) ends work long before it.
func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}

// activeProviders are the providers besides OpenStack configured here.
func activeProviders(config AppConfiguration) []string {
	if config.AIaaS.URL != "" {
		return []string{aiaas.Provider}
	}
	return nil
}
