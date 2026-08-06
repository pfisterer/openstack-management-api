package app

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/helper"
	"github.com/pfisterer/openstack-management-api/internal/mockdata"
	osclient "github.com/pfisterer/openstack-management-api/internal/openstack/client"
	"github.com/pfisterer/openstack-management-api/internal/reconciler"
	"github.com/pfisterer/openstack-management-api/internal/roleprovider"
	"github.com/pfisterer/openstack-management-api/internal/tree"
	"github.com/pfisterer/openstack-management-api/internal/webserver"
	"go.uber.org/zap"
)

func configureStores(cfg *common.StorageConfiguration, log *zap.SugaredLogger) (tree.Store, common.TokenLookupFunc, error) {
	storageType := strings.ToLower(strings.TrimSpace(cfg.Type))

	// API-token auth is a placeholder in both modes; real auth is OIDC.
	tokenLookup := func(_ context.Context, _ string) (common.TokenLookupResult, error) {
		return common.TokenLookupResult{Found: false}, nil
	}

	switch storageType {

	case "memory":
		// Memory mode is intended for local development and tests.
		return tree.NewInMemoryStore(log), tokenLookup, nil

	case "postgres":
		store, err := tree.NewPostgresStore(cfg.ConnectionString, log)
		if err != nil {
			return nil, nil, fmt.Errorf("postgres storage: %w", err)
		}
		return store, tokenLookup, nil

	default:
		return nil, nil, fmt.Errorf("unsupported storage type %q", cfg.Type)
	}
}

func configureAuthMiddleware(cfg *WebServerConfig, tokenLookup common.TokenLookupFunc, userTokenResolver common.UserTokenResolverFunc, log *zap.SugaredLogger) (gin.HandlerFunc, error) {

	// Setup Web server
	var authMiddleware gin.HandlerFunc

	if cfg.DummyAuth {
		log.Warn("DummyAuth enabled: using DummyAuthMiddleware (no SSO, user=group:uni_root)")
		authMiddleware = webserver.DummyAuthMiddleware()
	} else {

		// Create OIDC Auth Verifier
		oidcAuthVerifier, err := webserver.NewOIDCAuthVerifier(webserver.OIDCVerifierConfig{
			IssuerURL: cfg.OIDCIssuerURL,
			ClientID:  cfg.OIDCClientID,
		}, log)

		if err != nil {
			log.Fatalf("Failed to initialize OIDCAuthVerifier: %v", err)
		}

		authMiddleware = webserver.CombinedAuthMiddleware(oidcAuthVerifier, tokenLookup, userTokenResolver, log)
	}
	return authMiddleware, nil
}

// newOpenstackClient builds the OpenStack client for the configured
// authentication method. Application credentials are the default; password auth
// for a service user exists because an application credential is always
// project-scoped and therefore cannot create projects on a cloud that enforces
// the modern RBAC scope defaults.
func newOpenstackClient(cfg OpenstackConfiguration, log *zap.Logger, logger *zap.SugaredLogger) (*osclient.OpenStackClient, error) {
	switch cfg.AuthMethod() {
	case OpenstackAuthAppCredential:
		logger.Infow("OpenStack auth: application credential", "auth_url", cfg.AuthURL)
		return osclient.NewOSAdminWithAppCredential(
			cfg.AuthURL,
			cfg.ApplicationCredentialID,
			cfg.ApplicationCredentialSecret,
			cfg.ProjectID,
			cfg.Region,
			cfg.Insecure,
			log,
			logger,
		)

	case OpenstackAuthPassword:
		opts := osclient.PasswordAuthOpts{
			Username:          cfg.Username,
			Password:          cfg.Password,
			UserDomainName:    cfg.UserDomainName,
			SystemScope:       cfg.SystemScope,
			DomainName:        cfg.DomainName,
			ProjectID:         cfg.ProjectID,
			ProjectName:       cfg.ProjectName,
			ProjectDomainName: cfg.ProjectDomainName,
		}
		logger.Infow("OpenStack auth: password",
			"auth_url", cfg.AuthURL, "user", cfg.Username,
			"system_scope", cfg.SystemScope, "domain", cfg.DomainName,
			"project", cmp.Or(cfg.ProjectName, cfg.ProjectID))
		return osclient.NewOSAdminWithPassword(cfg.AuthURL, opts, cfg.Region, cfg.Insecure, log, logger)

	default:
		return nil, fmt.Errorf("no OpenStack credentials configured: set OS_APPLICATION_CREDENTIAL_ID/_SECRET or OS_USERNAME/OS_PASSWORD")
	}
}

func RunApplication() {
	// Load application configuration
	config, err := loadAppConfiguration()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load application configuration: %v\n", err)
		os.Exit(1)
	}

	// Initialize logger
	log, logger := helper.InitLogger(config.DevMode)
	defer log.Sync()
	logger.Info("Starting OpenStack Management Application")
	logAppConfig(config, logger)

	// Fail closed: the dummy-auth bypass (identity from an unverified header, with an
	// unknown user resolving to root tokens) must never run outside development.
	if config.WebServer.DummyAuth && !config.DevMode {
		logger.Fatal("API_DUMMY_AUTH=true is not allowed when API_MODE=production — refusing to start with an authentication bypass")
	}

	// Configure resource storage and token lookup
	nodeStore, tokenLookup, err := configureStores(&config.Storage, logger)
	if err != nil {
		logger.Fatal("Failed to initialize storage", zap.Error(err))
	}
	logger.Infof("Using storage backend: %s", config.Storage.Type)

	resourceTypeIDs := make([]string, 0, len(config.ProjectDefinitions))
	for _, definition := range config.ProjectDefinitions {
		resourceTypeIDs = append(resourceTypeIDs, definition.ID)
	}

	// Create role provider based on ROLE_PROVIDER env var ("http" or "mock").
	var roleProvider common.RoleProvider
	switch strings.ToLower(strings.TrimSpace(config.RoleProvider.Type)) {
	case "http":
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
	case "mock", "":
		// Built-in demo identities — for local/offline dev (pairs with dummy-auth).
		// NOT for production: it silently grants fake group memberships. Warn so an
		// accidental selection is visible in the logs (production sets ROLE_PROVIDER=http).
		logger.Warn("Using MockRoleProvider (built-in demo identities) — do not use in production")
		roleProvider = roleprovider.NewMockRoleProvider()
	default:
		// Fail loud on a typo (e.g. "htpp") instead of silently falling back to the
		// mock, which in production would run with wrong authorization data.
		logger.Fatalw("invalid ROLE_PROVIDER: must be 'http' or 'mock'", "value", config.RoleProvider.Type)
	}

	requestTimeout := time.Duration(config.ServiceTimeoutSeconds) * time.Second
	treeSvc := tree.NewService(nodeStore, roleProvider, resourceTypeIDs, config.RootAdminTokens, requestTimeout, config.MaxAuthorizedUsers, logger)

	// Bootstrap: optional mock seed into an empty store, then ensure the
	// structural root/unassigned nodes (root admin scope is synced from config).
	var mockIdentities []common.Identity
	var mockNodes []tree.Node
	if config.Storage.AddMockData {
		mockIdentities, mockNodes = mockdata.DefaultMockTreeState()
	}
	if err := treeSvc.Bootstrap(context.Background(), mockIdentities, mockNodes); err != nil {
		logger.Fatal("Failed to bootstrap resource tree", zap.Error(err))
	}

	//Create authentication middleware based on configuration.
	authMiddleware, err := configureAuthMiddleware(&config.WebServer, tokenLookup, roleProvider.GetUserTokens, logger)
	if err != nil {
		logger.Fatal("Failed to initialize authentication middleware", zap.Error(err))
	}

	// Add dummy dev users from mock data if dummy auth is enabled
	dummyDevUsers := []string{}
	if config.WebServer.DummyAuth {
		identities, _ := mockdata.DefaultMockTreeState()
		for _, ident := range identities {
			dummyDevUsers = append(dummyDevUsers, ident.Email)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Setup Gin web server with configured dependencies.
	// A local ReconcilerAPI variable avoids passing a typed nil as the interface,
	// which would make cfg.Reconciler != nil even when no reconciler exists.
	var reconcilerAPI webserver.ReconcilerAPI

	if config.Reconciler.Enabled {
		logger.Infow("Starting reconciler", "interval_seconds", config.Reconciler.IntervalSeconds, "dry_run", config.Reconciler.DryRun)

		// Connecting to OpenStack is retried in the background rather than done
		// once here: a cloud that is away for a minute must not disable
		// provisioning until somebody restarts the pod. See reconcilerSupervisor.
		supervisor := &reconcilerSupervisor{}
		supervisor.connectAndStart(ctx, func() (*reconciler.Reconciler, error) {
			osClient, osErr := newOpenstackClient(config.Openstack, log, logger)
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
				NoDelete:                 config.Reconciler.NoDelete,
				DeleteReleasedProjects:   config.Reconciler.DeleteReleasedProjects,
				PendingDeletionGraceDays: config.Reconciler.PendingDeletionGraceDays,
				PendingDeletionTagPrefix: config.Reconciler.PendingDeletionTagPrefix,
				ContactTagPrefix:         config.Reconciler.ContactTagPrefix,
			}
			return reconciler.New(nodeStore, osClient, reconcilerCfg, config.ProjectDefinitions, roleProvider, logger), nil
		}, logger)
		reconcilerAPI = supervisor
	} else {
		logger.Info("Reconciler disabled (set RECONCILER_ENABLED=true to enable)")
	}

	router := webserver.SetupGinWebserver(webserver.SetupConfig{
		DevMode: config.DevMode,
		Log:     logger,
		StaticConfig: webserver.StaticConfig{
			OIDCIssuerURL: config.WebServer.OIDCIssuerURL,
			OIDCClientID:  config.WebServer.OIDCClientID,
		},
		API: webserver.APIConfig{
			RoleSwitchGroups:   config.RootAdminTokens,
			ProjectDefinitions: config.ProjectDefinitions,
			Service:            treeSvc,
			DummyDevUsers:      dummyDevUsers,
			// Asked per request: the reconciler may still be connecting.
			ProvisioningEnabled: func() bool { return reconcilerAPI != nil && reconcilerAPI.Ready() },
		},
		Reconciler:         reconcilerAPI,
		RootAdminTokens:    config.RootAdminTokens,
		AuthMiddleware:     authMiddleware,
		CORSAllowedOrigins: config.WebServer.CORSAllowedOrigins,
	})

	// Start the Web server
	err = router.Run(config.WebServer.GinBindString)
	if err != nil {
		logger.Fatal("Failed to start server", zap.Error(err))
	}

	logger.Info("Application completed successfully")
}
