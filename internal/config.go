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

// ReconcilerConfiguration controls the two-way sync with OpenStack.
type ReconcilerConfiguration struct {
	// Enabled activates the reconciler. When false the reconciler goroutine is not started.
	Enabled bool `json:"enabled"`
	// IntervalSeconds is the time between automatic reconciliation runs.
	IntervalSeconds int `json:"interval_seconds"`
	// ProjectPrefix is prepended to the project ID when naming new OS projects.
	ProjectPrefix string `json:"project_prefix"`
	// ScopeParentID, when set, scopes the OS-only import to children of this parent project.
	// Projects outside this scope are ignored during the reverse-sync phase.
	ScopeParentID string `json:"scope_parent_id"`
	// DryRun runs reconciliation logic without making any writes. Useful for testing.
	DryRun bool `json:"dry_run"`
	// NoDelete disables all destructive reconciler operations (project/user removal,
	// released-project deletion) while still syncing/creating. A safety mode for
	// initial rollout; wired from RECONCILER_NO_DELETE (default false).
	NoDelete bool `json:"no_delete"`
	// ManagedProjectTag is the OpenStack project tag used to identify projects created by
	// this system. Default: "dhbw-managed".
	ManagedProjectTag string `json:"managed_project_tag"`
	// ResourceIDTagPrefix is the prefix for the tag that encodes the linked request ID.
	// Full tag format: "<prefix><requestID>". Default: "dhbw-resource-id:".
	ResourceIDTagPrefix string `json:"resource_id_tag_prefix"`

	// DeleteReleasedProjects controls what happens when a managed OS project's request
	// is released. When true the OS project is deleted immediately. When false (default)
	// the project is kept but tagged with a pending-deletion date and contact info so
	// external workflow tools can drive the actual cleanup.
	DeleteReleasedProjects bool `json:"delete_released_projects"`
	// PendingDeletionGraceDays is the number of days from the current reconcile run
	// used as the scheduled deletion date written into PendingDeletionTagPrefix tags.
	// Only relevant when DeleteReleasedProjects is false. Default: 30.
	PendingDeletionGraceDays int `json:"pending_deletion_grace_days"`
	// PendingDeletionTagPrefix is the tag prefix written to released projects when
	// DeleteReleasedProjects is false. The full tag is "<prefix><YYYY-MM-DD>".
	// Default: "pending-deletion:".
	PendingDeletionTagPrefix string `json:"pending_deletion_tag_prefix"`
	// ContactTagPrefix is the prefix for tags that record requester contact addresses.
	// One tag per requester email is written alongside the pending-deletion tag.
	// Default: "contact:".
	ContactTagPrefix string `json:"contact_tag_prefix"`
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
}

// RoleProviderConfig selects which RoleProvider implementation to use.
type RoleProviderConfig struct {
	// Type is "mock" (default) or "http" (group-auth-service).
	Type string `json:"type"`
	// URL is the base URL of the group-auth-service, required when Type is "http".
	URL string `json:"url"`
	// APIToken is the Bearer token sent to group-auth-service, required when Type is "http".
	APIToken string `json:"api_token"`
}

// AppConfiguration is the top-level application configuration.
type AppConfiguration struct {
	Storage      common.StorageConfiguration `json:"storage" validate:"required"`
	Openstack    OpenstackConfiguration      `json:"openstack" validate:"required"`
	Reconciler   ReconcilerConfiguration     `json:"reconciler"`
	WebServer    WebServerConfig             `json:"web_server" validate:"required"`
	RoleProvider RoleProviderConfig          `json:"role_provider"`
	DevMode      bool                        `json:"dev_mode"`
	// RootAdminTokens (from ROOT_ADMIN_TOKENS) are the system-wide admin tokens.
	// They gate three surfaces: the service's root-admin checks, the reconciler
	// admin endpoints, and the role-switch allowlist.
	RootAdminTokens       common.TokenList        `json:"root_admin_tokens"`
	ProjectDefinitions    []common.ManagedProject `json:"resource_definitions" validate:"required,min=1,dive"`
	ServiceTimeoutSeconds int                     `json:"service_timeout_seconds"`
}

// loadAppConfiguration loads configuration from an optional .env file and environment variables.
// Priority order (low to high): .env < environment variables.
func loadAppConfiguration() (AppConfiguration, error) {
	// Load .env if present. Overload (not Load) is used so .env values take precedence over
	// any OS-level environment variables with the same name. This prevents the shell's
	// OpenStack credentials (e.g. OS_AUTH_URL sourced from openrc) from silently shadowing
	// the values configured for this application instance.
	if _, err := os.Stat(".env"); err == nil {
		if err := godotenv.Overload(".env"); err != nil {
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
			Insecure:                    getEnvBool("OPENSTACK_INSECURE", "OS_INSECURE", false),
		},
		WebServer: WebServerConfig{
			DummyAuth:     getEnvBool("API_DUMMY_AUTH", "API_DUMMY_AUTH", false),
			OIDCIssuerURL: helper.GetEnvString("OIDC_ISSUER_URL", ""),
			OIDCClientID:  helper.GetEnvString("OIDC_CLIENT_ID", ""),
			GinBindString: helper.GetEnvString("API_BIND", ":8083"),
		},

		Reconciler: ReconcilerConfiguration{
			Enabled:                  helper.GetEnvBool("RECONCILER_ENABLED", false),
			IntervalSeconds:          helper.GetEnvInt("RECONCILER_INTERVAL_SECONDS", 300),
			ProjectPrefix:            helper.GetEnvString("RECONCILER_PROJECT_PREFIX", "managed-"),
			ScopeParentID:            helper.GetEnvString("RECONCILER_SCOPE_PARENT_ID", ""),
			DryRun:                   helper.GetEnvBool("RECONCILER_DRY_RUN", false),
			NoDelete:                 helper.GetEnvBool("RECONCILER_NO_DELETE", false),
			ManagedProjectTag:        helper.GetEnvString("RECONCILER_MANAGED_PROJECT_TAG", "managed"),
			ResourceIDTagPrefix:      helper.GetEnvString("RECONCILER_RESOURCE_ID_TAG_PREFIX", "managed-resource-id:"),
			DeleteReleasedProjects:   helper.GetEnvBool("RECONCILER_DELETE_RELEASED_PROJECTS", false),
			PendingDeletionGraceDays: helper.GetEnvInt("RECONCILER_PENDING_DELETION_GRACE_DAYS", 30),
			PendingDeletionTagPrefix: helper.GetEnvString("RECONCILER_PENDING_DELETION_TAG_PREFIX", "pending-deletion:"),
			ContactTagPrefix:         helper.GetEnvString("RECONCILER_CONTACT_TAG_PREFIX", "contact:"),
		},
		RoleProvider: RoleProviderConfig{
			Type:     helper.GetEnvString("ROLE_PROVIDER", "mock"),
			URL:      helper.GetEnvString("ROLE_PROVIDER_URL", ""),
			APIToken: helper.GetEnvString("ROLE_PROVIDER_API_TOKEN", ""),
		},
		DevMode:               getEnvString("API_MODE", "API_MODE", "production") == "development",
		RootAdminTokens:       parseCSVEnv(helper.GetEnvString("ROOT_ADMIN_TOKENS", "")),
		ProjectDefinitions:    loadProjectDefinitionsOrDefaults(),
		ServiceTimeoutSeconds: helper.GetEnvInt("SERVICE_TIMEOUT_SECONDS", 30),
	}

	if err := validateConfig(cfg); err != nil {
		return AppConfiguration{}, err
	}

	return cfg, nil
}

func loadProjectDefinitionsOrDefaults() []common.ManagedProject {

	// Default set — single source of truth for UI display AND OpenStack quota mapping.
	// ShowOnUI: true  → returned to the frontend via /v1/config (user-configurable).
	// Static: true    → quota is fixed at Default; applied once at project creation.
	return []common.ManagedProject{
		// ── User-configurable resources (shown on UI) ────────────
		{
			ID: "cores", Name: "Cores", Default: 4, Min: 1, Max: 100000,
			Message:           "1 - 100000 cores",
			ShowOnUI:          true,
			OSQuotaField:      "cores",
			OSLinkedField:     "instances", // instances cap mirrors cores
			OSOvercommitCheck: true,
		},
		{
			ID: "ram", Name: "RAM", Default: 16, Min: 1, Max: 256000,
			Unit:              "GB",
			Message:           "1 GB - 256 TB",
			ShowOnUI:          true,
			OSQuotaField:      "ram",
			OSMultiplier:      1024, // stored in GB, OpenStack expects MB
			OSOvercommitCheck: true,
		},
		{
			ID: "storage", Name: "Storage", Default: 50, Min: 1, Max: 100000,
			Unit:         "GB",
			Message:      "1 GB - 100 TB",
			ShowOnUI:     true,
			OSQuotaField: "gigabytes",
		},
		{
			ID: "gpu", Name: "GPUs", Default: 0, Min: 0, Max: 1000,
			Unit:     "units",
			Message:  "0 - 1000 GPUs",
			ShowOnUI: true,
			// No standard OpenStack quota field for GPUs; OSQuotaField intentionally empty.
		},

		// ── Static network/storage quotas (not shown on UI, fixed at project creation) ─
		// To change a default, update the Default field here. The OSQuotaField drives the
		// mapping to OpenStack — no other file needs to change.
		{ID: "networks", Name: "Networks", Default: helper.GetEnvInt("RECONCILER_DEFAULT_NETWORKS", 2),
			Static: true, OSQuotaField: "networks"},
		{ID: "subnets", Name: "Subnets", Default: helper.GetEnvInt("RECONCILER_DEFAULT_SUBNETS", 4),
			Static: true, OSQuotaField: "subnets"},
		{ID: "ports", Name: "Ports", Default: helper.GetEnvInt("RECONCILER_DEFAULT_PORTS", 50),
			Static: true, OSQuotaField: "ports"},
		{ID: "routers", Name: "Routers", Default: helper.GetEnvInt("RECONCILER_DEFAULT_ROUTERS", 1),
			Static: true, OSQuotaField: "routers"},
		{ID: "floating_ips", Name: "Floating IPs", Default: helper.GetEnvInt("RECONCILER_DEFAULT_FLOATING_IPS", 2),
			Static: true, OSQuotaField: "floating_ips"},
		{ID: "security_groups", Name: "Security Groups", Default: helper.GetEnvInt("RECONCILER_DEFAULT_SECURITY_GROUPS", 10),
			Static: true, OSQuotaField: "security_groups"},
		{ID: "volumes", Name: "Volumes", Default: helper.GetEnvInt("RECONCILER_DEFAULT_VOLUMES", 10),
			Static: true, OSQuotaField: "volumes"},
		{ID: "snapshots", Name: "Snapshots", Default: helper.GetEnvInt("RECONCILER_DEFAULT_SNAPSHOTS", 10),
			Static: true, OSQuotaField: "snapshots"},
	}
}

// redactSecret masks a secret for logging, showing at most the first 4 characters
// as a hint. Safe for empty/short strings (the old first-5 slice panicked on those).
func redactSecret(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 4 {
		return "****"
	}
	return s[:4] + "****"
}

func logAppConfig(appConfig AppConfiguration, log *zap.SugaredLogger) {
	var appConfigJson []byte
	var err error

	// Redact ALL secrets before marshalling — the whole config is logged below.
	appConfig.Openstack.ApplicationCredentialSecret = redactSecret(appConfig.Openstack.ApplicationCredentialSecret)
	appConfig.RoleProvider.APIToken = redactSecret(appConfig.RoleProvider.APIToken)
	appConfig.Storage.ConnectionString = redactSecret(appConfig.Storage.ConnectionString)

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
