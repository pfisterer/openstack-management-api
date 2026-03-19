package app

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/openstack-management-api/internal/applogic"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/helper"
	"github.com/pfisterer/openstack-management-api/internal/mockdata"
	"github.com/pfisterer/openstack-management-api/internal/storage"
	"github.com/pfisterer/openstack-management-api/internal/webserver"
	"go.uber.org/zap"
)

func configureStores(cfg *common.StorageConfiguration, log *zap.SugaredLogger) (applogic.ResourceStore, common.TokenLookupFunc, error) {
	storageType := strings.ToLower(strings.TrimSpace(cfg.Type))

	switch storageType {

	case "memory":
		// Memory mode is intended for local development and tests.
		tokenLookup := func(_ context.Context, _ string) (common.TokenLookupResult, error) {
			return common.TokenLookupResult{Found: false}, nil
		}
		return storage.NewInMemoryResourceStore(log), tokenLookup, nil

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

	// Configure resource storage and token lookup
	resourceStore, tokenLookup, err := configureStores(&config.Storage, logger)
	if err != nil {
		logger.Fatal("Failed to initialize storage", zap.Error(err))
	}
	logger.Infof("Using storage backend: %s", config.Storage.Type)

	resourceTypeIDs := make([]string, 0, len(config.ResourceDefinitions))
	for _, definition := range config.ResourceDefinitions {
		resourceTypeIDs = append(resourceTypeIDs, definition.ID)
	}

	// Create resource service
	roleProvider := mockdata.NewMockRoleProvider()
	resourceSvc := applogic.NewService(resourceStore, roleProvider, resourceTypeIDs, logger)
	if err := resourceSvc.InitializeState(context.Background(), config.Storage.AddMockData); err != nil {
		logger.Fatal("Failed to initialize resource state storage", zap.Error(err))
	}

	//Create authentication middleware based on configuration.
	authMiddleware, err := configureAuthMiddleware(&config.WebServer, tokenLookup, roleProvider.GetUserTokens, logger)
	if err != nil {
		logger.Fatal("Failed to initialize authentication middleware", zap.Error(err))
	}

	// Setup Gin web server with configured dependencies
	router := webserver.SetupGinWebserver(webserver.SetupConfig{
		DevMode: config.DevMode,
		Log:     logger,
		StaticConfig: webserver.StaticConfig{
			OIDCIssuerURL: config.WebServer.OIDCIssuerURL,
			OIDCClientID:  config.WebServer.OIDCClientID,
		},
		ResourceAPI: webserver.ResourceAPIConfig{
			RoleSwitchGroups:    config.RoleSwitchGroups,
			ResourceDefinitions: config.ResourceDefinitions,
			Service:             resourceSvc,
		},
		AuthMiddleware: authMiddleware,
	})

	// Start the Web server
	err = router.Run(config.WebServer.GinBindString)
	if err != nil {
		logger.Fatal("Failed to start server", zap.Error(err))
	}

	logger.Info("Application completed successfully")
}
