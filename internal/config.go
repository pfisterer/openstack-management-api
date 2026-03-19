package app

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/go-playground/validator/v10"
	"github.com/joho/godotenv"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/helper"
	"github.com/pfisterer/openstack-management-api/internal/webserver"
	"go.uber.org/zap"
)

// OpenstackConfiguration describes OpenStack authentication and client settings.
type OpenstackConfiguration struct {
	AuthURL                     string `json:"auth_url" validate:"required,url"`
	ApplicationCredentialID     string `json:"application_credential_id" validate:"required"`
	ApplicationCredentialSecret string `json:"application_credential_secret" validate:"required"`
	ProjectID                   string `json:"project_id" validate:"required"`
	Region                      string `json:"region" validate:"required"`
	Insecure                    bool   `json:"insecure"`
}

type WebServerConfig struct {
	// Use dummy authentication middleware that allows all requests and sets a default user (for development/testing).
	// If false, the real authentication middleware with OIDC and API token support will be used.
	DummyAuth bool `json:"dummy_auth"`
	// The OIDC issuer URL for authentication
	OIDCIssuerURL string `json:"oidc_issuer_url" validate:"required,url"`
	// The OIDC client ID for authentication
	OIDCClientID string `json:"oidc_client_id" validate:"required"`
	// The bind string for the Gin web server (e.g., ":8082")
	GinBindString string `json:"gin_bind_string" validate:"required"`
	// The base URL for the web server (e.g., "http://localhost:8082")
	WebserverBaseUrl string `json:"webserver_base_url" validate:"required,url"`
	// The TTL (in hours) for API tokens
	ApiTokenTTLHours int `json:"api_token_ttl_hours"`
	// The version of the external DNS image to use
	ExternalDnsVersion string `json:"external_dns_version" validate:"required"`
}

// AppConfiguration is the top-level application configuration.
type AppConfiguration struct {
	Storage             common.StorageConfiguration    `json:"storage" validate:"required"`
	Openstack           OpenstackConfiguration         `json:"openstack" validate:"required"`
	WebServer           WebServerConfig                `json:"web_server" validate:"required"`
	DevMode             bool                           `json:"dev_mode"`
	RoleSwitchGroups    common.TokenList               `json:"role_switch_groups"`
	ResourceDefinitions []webserver.ResourceDefinition `json:"resource_definitions" validate:"required,min=1,dive"`
}

// loadAppConfiguration loads configuration from an optional .env file and environment variables.
// Priority order (low to high): .env < environment variables.
func loadAppConfiguration() (AppConfiguration, error) {
	// Load .env if present. Existing environment variables are not overridden.
	if _, err := os.Stat(".env"); err == nil {
		if err := godotenv.Load(".env"); err != nil {
			return AppConfiguration{}, fmt.Errorf("failed to load .env configuration: %w", err)
		}
	}

	// Generate the application configuration struct from environment variables.
	cfg := AppConfiguration{
		Storage: common.StorageConfiguration{
			Type:             strings.ToLower(strings.TrimSpace(helper.GetEnvString("DB_TYPE", "memory"))),
			ConnectionString: helper.GetEnvString("DB_CONNECTION_STRING", "host=localhost user=postgres password=postgres dbname=openstack_management_api port=5432 sslmode=disable TimeZone=UTC"),
			AddMockData:      helper.GetEnvBool("DB_ADD_MOCK_DATA", false),
		},
		Openstack: OpenstackConfiguration{
			AuthURL:                     getEnvString("OPENSTACK_AUTH_URL", "OS_AUTH_URL", ""),
			ApplicationCredentialID:     getEnvString("OPENSTACK_APPLICATION_CREDENTIAL_ID", "OS_APPLICATION_CREDENTIAL_ID", ""),
			ApplicationCredentialSecret: getEnvString("OPENSTACK_APPLICATION_CREDENTIAL_SECRET", "OS_APPLICATION_CREDENTIAL_SECRET", ""),
			ProjectID:                   getEnvString("OPENSTACK_PROJECT_ID", "OS_PROJECT_ID", ""),
			Region:                      getEnvString("OPENSTACK_REGION", "OS_REGION_NAME", "microstack"),
			Insecure:                    getEnvBool("OPENSTACK_INSECURE", "OS_INSECURE", true),
		},
		WebServer: WebServerConfig{
			ApiTokenTTLHours:   helper.GetEnvInt("API_TOKEN_TTL_HOURS", 24),
			DummyAuth:          getEnvBool("API_DUMMY_AUTH", "API_DUMMY_AUTH", false),
			OIDCIssuerURL:      helper.GetEnvString("OIDC_ISSUER_URL", ""),
			OIDCClientID:       helper.GetEnvString("OIDC_CLIENT_ID", ""),
			GinBindString:      helper.GetEnvString("API_BIND", ":8083"),
			WebserverBaseUrl:   helper.GetEnvString("API_BASE_URL", "http://localhost:8083"),
			ExternalDnsVersion: helper.GetEnvString("EXTERNAL_DNS_IMAGE_VERSION", "v0.19.0"),
		},

		DevMode:             getEnvString("API_MODE", "API_MODE", "production") == "development",
		RoleSwitchGroups:    parseCSVEnv(helper.GetEnvString("ROLE_SWITCH_GROUPS", "")),
		ResourceDefinitions: loadResourceDefinitions(helper.GetEnvString("RESOURCE_DEFINITIONS_FILE", "")),
	}

	if err := validateConfig(cfg); err != nil {
		return AppConfiguration{}, err
	}

	return cfg, nil
}

func loadResourceDefinitions(filename string) []webserver.ResourceDefinition {
	if filename != "" {
		content, err := os.ReadFile(filename)
		if err != nil {
			panic(fmt.Sprintf("failed to read resource definitions file %q: %v", filename, err))
		}

		var defs []webserver.ResourceDefinition
		if err := json.Unmarshal(content, &defs); err != nil {
			panic(fmt.Sprintf("failed to parse resource definitions file %q: %v", filename, err))
		}

		return defs
	}

	return webserver.DefaultResourceDefinitions()
}

func logAppConfig(appConfig AppConfiguration, log *zap.SugaredLogger) {
	var appConfigJson []byte
	var err error

	// Redact sensitive information (print first 10 characters of the secret)
	appConfig.Openstack.ApplicationCredentialSecret = fmt.Sprintf("%s**********", appConfig.Openstack.ApplicationCredentialSecret[:10])

	if appConfig.DevMode {
		appConfigJson, err = json.MarshalIndent(appConfig, "", "  ")
	} else {
		// In production mode, we use a compact JSON format without indentation
		appConfigJson, err = json.Marshal(appConfig)
	}

	//marshall the appConfig to JSON for logging
	if err != nil {
		log.Errorf("app.LogAppConfig: Failed to marshal appConfig to JSON: %v", err)
		return
	}

	log.Infof("app.LogAppConfig: Application configuration: %s", appConfigJson)
}

func getEnvString(primaryKey, secondaryKey, defaultValue string) string {
	if value := os.Getenv(primaryKey); value != "" {
		return value
	}
	if value := os.Getenv(secondaryKey); value != "" {
		return value
	}
	return defaultValue
}

func getEnvBool(primaryKey, secondaryKey string, defaultValue bool) bool {
	if value := os.Getenv(primaryKey); value != "" {
		parsed, err := strconv.ParseBool(value)
		if err == nil {
			return parsed
		}
	}
	if value := os.Getenv(secondaryKey); value != "" {
		parsed, err := strconv.ParseBool(value)
		if err == nil {
			return parsed
		}
	}
	return defaultValue
}

func validateConfig(config AppConfiguration) error {
	v := validator.New(validator.WithRequiredStructEnabled())
	if err := v.Struct(config); err != nil {
		if validationErrors, ok := err.(validator.ValidationErrors); ok {
			return fmt.Errorf("configuration validation failed: %s", formatValidationErrors(validationErrors))
		}
		return err
	}
	return nil
}

func formatValidationErrors(errs validator.ValidationErrors) string {
	var message strings.Builder
	for _, e := range errs {
		fmt.Fprintf(&message, "\n - field '%s' failed on '%s' (value: '%v')", e.Namespace(), e.Tag(), e.Value())
	}
	return message.String()
}

func parseCSVEnv(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return []string{}
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		value := strings.TrimSpace(part)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}
