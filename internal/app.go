package app

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/openstack-management-api/internal/applogic"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/helper"
	"github.com/pfisterer/openstack-management-api/internal/mockdata"
	osclient "github.com/pfisterer/openstack-management-api/internal/openstack/client"
	"github.com/pfisterer/openstack-management-api/internal/reconciler"
	"github.com/pfisterer/openstack-management-api/internal/roleprovider"
	"github.com/pfisterer/openstack-management-api/internal/storage"
	"github.com/pfisterer/openstack-management-api/internal/webserver"
	"go.uber.org/zap"
)

func configureStores(cfg *common.StorageConfiguration, log *zap.SugaredLogger) (applogic.ProjectStore, common.TokenLookupFunc, error) {
	storageType := strings.ToLower(strings.TrimSpace(cfg.Type))

	switch storageType {

	case "memory":
		// Memory mode is intended for local development and tests.
		tokenLookup := func(_ context.Context, _ string) (common.TokenLookupResult, error) {
			return common.TokenLookupResult{Found: false}, nil
		}
		return storage.NewInMemoryProjectStore(log), tokenLookup, nil

	case "postgres":
		store, err := storage.NewPostgresProjectStore(cfg.ConnectionString, log)
		if err != nil {
			return nil, nil, fmt.Errorf("postgres storage: %w", err)
		}
		tokenLookup := func(_ context.Context, _ string) (common.TokenLookupResult, error) {
			return common.TokenLookupResult{Found: false}, nil
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
	resourceStore, tokenLookup, err := configureStores(&config.Storage, logger)
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
	resourceSvc := applogic.NewService(resourceStore, roleProvider, resourceTypeIDs, config.RoleSwitchGroups, requestTimeout, logger)
	if err := resourceSvc.InitializeState(context.Background(), config.Storage.AddMockData); err != nil {
		logger.Fatal("Failed to initialize resource state storage", zap.Error(err))
	}

	//Create authentication middleware based on configuration.
	authMiddleware, err := configureAuthMiddleware(&config.WebServer, tokenLookup, roleProvider.GetUserTokens, logger)
	if err != nil {
		logger.Fatal("Failed to initialize authentication middleware", zap.Error(err))
	}

	// Add dummy dev users from mock data if dummy auth is enabled
	dummyDevUsers := []string{}
	if config.WebServer.DummyAuth {
		identities, _, _, _ := mockdata.DefaultMockResourceState()
		for _, ident := range identities {
			dummyDevUsers = append(dummyDevUsers, ident.Email)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var rec *reconciler.Reconciler

	if config.Reconciler.Enabled {
		logger.Infow("Starting reconciler", "interval_seconds", config.Reconciler.IntervalSeconds, "dry_run", config.Reconciler.DryRun)

		osClient, osErr := osclient.NewOSAdminWithAppCredential(
			config.Openstack.AuthURL,
			config.Openstack.ApplicationCredentialID,
			config.Openstack.ApplicationCredentialSecret,
			config.Openstack.ProjectID,
			config.Openstack.Region,
			config.Openstack.Insecure,
			log,
			logger,
		)
		if osErr != nil {
			logger.Warnw("OpenStack API not reachable — reconciler will be disabled; restart to retry", zap.Error(osErr))
		} else {
			osClient.SetTagConfig(config.Reconciler.ManagedProjectTag, config.Reconciler.ResourceIDTagPrefix)

			reconcilerCfg := reconciler.Config{
				Interval:                 time.Duration(config.Reconciler.IntervalSeconds) * time.Second,
				ProjectPrefix:            config.Reconciler.ProjectPrefix,
				ScopeParentID:            config.Reconciler.ScopeParentID,
				DryRun:                   config.Reconciler.DryRun,
				DeleteReleasedProjects:   config.Reconciler.DeleteReleasedProjects,
				PendingDeletionGraceDays: config.Reconciler.PendingDeletionGraceDays,
				PendingDeletionTagPrefix: config.Reconciler.PendingDeletionTagPrefix,
				ContactTagPrefix:         config.Reconciler.ContactTagPrefix,
			}
			rec = reconciler.New(resourceStore, osClient, reconcilerCfg, config.ProjectDefinitions, roleProvider, logger)
			go rec.Start(ctx)
		}
	} else {
		logger.Info("Reconciler disabled (set RECONCILER_ENABLED=true to enable)")
	}

	// Setup Gin web server with configured dependencies.
	// Use a local ReconcilerAPI variable to avoid passing a typed nil (*reconciler.Reconciler)
	// as the interface, which would make cfg.Reconciler != nil even when rec is nil.
	var reconcilerAPI webserver.ReconcilerAPI
	if rec != nil {
		reconcilerAPI = rec
	}

	router := webserver.SetupGinWebserver(webserver.SetupConfig{
		DevMode: config.DevMode,
		Log:     logger,
		StaticConfig: webserver.StaticConfig{
			OIDCIssuerURL: config.WebServer.OIDCIssuerURL,
			OIDCClientID:  config.WebServer.OIDCClientID,
		},
		ProjectAPI: webserver.ProjectAPIConfig{
			RoleSwitchGroups:   config.RoleSwitchGroups,
			ProjectDefinitions: config.ProjectDefinitions,
			Service:            resourceSvc,
			DummyDevUsers:      dummyDevUsers,
		},
		Reconciler:      reconcilerAPI,
		RootAdminTokens: config.RoleSwitchGroups,
		AuthMiddleware:  authMiddleware,
	})

	// Start the Web server
	err = router.Run(config.WebServer.GinBindString)
	if err != nil {
		logger.Fatal("Failed to start server", zap.Error(err))
	}

	logger.Info("Application completed successfully")
}
