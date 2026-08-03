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
//
// Two authentication methods are supported; configure exactly one (see
// AuthMethod). Application credentials are the simpler choice but are always
// project-scoped, which is a hard limit on clouds that enforce the modern RBAC
// scope defaults: creating projects or assigning roles across projects needs a
// system- or domain-scoped token there, and no application credential can carry
// one. Password auth for a service user exists for exactly that case, because it
// can request system or domain scope.
type OpenstackConfiguration struct {
	AuthURL                     string `json:"auth_url" validate:"required,url"`
	ApplicationCredentialID     string `json:"application_credential_id"`
	ApplicationCredentialSecret string `json:"application_credential_secret"`
	// Username/Password authenticate a service user (OS_USERNAME / OS_PASSWORD).
	// UserDomainName is the domain the user itself lives in (OS_USER_DOMAIN_NAME).
	Username       string `json:"username"`
	Password       string `json:"password"`
	UserDomainName string `json:"user_domain_name"`
	// Scope of the requested token, in the order it is applied: SystemScope
	// (OS_SYSTEM_SCOPE=all) > DomainName (OS_DOMAIN_NAME) > project
	// (ProjectID/ProjectName + ProjectDomainName). Password auth only.
	SystemScope       bool   `json:"system_scope"`
	DomainName        string `json:"domain_name"`
	ProjectName       string `json:"project_name"`
	ProjectDomainName string `json:"project_domain_name"`
	// ProjectID scopes token auth; with application credentials the scope comes
	// from the credential itself and this stays unused.
	ProjectID string `json:"project_id"`
	Region    string `json:"region" validate:"required"`
	Insecure  bool   `json:"insecure"`
	// FederatedProvisioning makes pre-created users federated shadow users instead of plain
	// local users. Required on clouds whose OIDC mapping is ephemeral (the Keystone default
	// when the mapping sets no user "type"): there a plain local user is ignored on SSO login
	// and the role assignment is orphaned. Leave off for type:local mappings, where a local
	// user binds by name. Wired from OPENSTACK_FEDERATED_PROVISIONING (default false).
	FederatedProvisioning bool `json:"federated_provisioning"`
	// FederatedIdPID is the Keystone identity-provider id a pre-created federated user is bound
	// to; it must match the IdP the user logs in through. Default "keycloak".
	FederatedIdPID string `json:"federated_idp_id"`
	// FederatedProtocolID is the federation protocol id (e.g. "openid", "saml2"). Default "openid".
	FederatedProtocolID string `json:"federated_protocol_id"`
	// FederatedDomainID is the Keystone domain a federated account lives in. It is part of
	// the derived user ID (sha256(domain_id + "user" + unique_id)), which is how a
	// pre-created account is matched against the one an SSO login resolves to. Default "default".
	FederatedDomainID string `json:"federated_domain_id"`
}

// OpenStack authentication methods.
const (
	OpenstackAuthNone          = "none"
	OpenstackAuthAppCredential = "application_credential"
	OpenstackAuthPassword      = "password"
)

// AuthMethod reports which authentication method the configuration selects.
// Application credentials win when both are present, so a leftover OS_USERNAME
// from a sourced openrc cannot silently take over an explicit credential.
func (c OpenstackConfiguration) AuthMethod() string {
	switch {
	case c.ApplicationCredentialID != "" && c.ApplicationCredentialSecret != "":
		return OpenstackAuthAppCredential
	case c.Username != "" && c.Password != "":
		return OpenstackAuthPassword
	default:
		return OpenstackAuthNone
	}
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
	// ScopeParentName is the name-based alternative to ScopeParentID: the reconciler
	// resolves the project by name and creates it if it does not exist yet.
	// Ignored when ScopeParentID is set.
	ScopeParentName string `json:"scope_parent_name"`
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
			Username:                    getEnvString("OPENSTACK_USERNAME", "OS_USERNAME", ""),
			Password:                    getEnvString("OPENSTACK_PASSWORD", "OS_PASSWORD", ""),
			UserDomainName:              getEnvString("OPENSTACK_USER_DOMAIN_NAME", "OS_USER_DOMAIN_NAME", "Default"),
			// openrc spells the system scope OS_SYSTEM_SCOPE=all.
			SystemScope:           strings.EqualFold(getEnvString("OPENSTACK_SYSTEM_SCOPE", "OS_SYSTEM_SCOPE", ""), "all"),
			DomainName:            getEnvString("OPENSTACK_DOMAIN_NAME", "OS_DOMAIN_NAME", ""),
			ProjectName:           getEnvString("OPENSTACK_PROJECT_NAME", "OS_PROJECT_NAME", ""),
			ProjectDomainName:     getEnvString("OPENSTACK_PROJECT_DOMAIN_NAME", "OS_PROJECT_DOMAIN_NAME", ""),
			ProjectID:             getEnvString("OPENSTACK_PROJECT_ID", "OS_PROJECT_ID", ""),
			Region:                getEnvString("OPENSTACK_REGION", "OS_REGION_NAME", "microstack"),
			Insecure:              getEnvBool("OPENSTACK_INSECURE", "OS_INSECURE", false),
			FederatedProvisioning: helper.GetEnvBool("OPENSTACK_FEDERATED_PROVISIONING", false),
			FederatedIdPID:        helper.GetEnvString("OPENSTACK_FEDERATED_IDP_ID", "keycloak"),
			FederatedProtocolID:   helper.GetEnvString("OPENSTACK_FEDERATED_PROTOCOL_ID", "openid"),
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
			ScopeParentName:          helper.GetEnvString("RECONCILER_SCOPE_PARENT_NAME", ""),
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
	appConfig.Openstack.Password = redactSecret(appConfig.Openstack.Password)
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
