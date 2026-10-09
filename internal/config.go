package app

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/go-playground/validator/v10"
	"github.com/joho/godotenv"
	"github.com/pfisterer/cloud-self-service-golib/envconf"
	"github.com/pfisterer/cloud-self-service-golib/redact"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/usage"
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
	// GroupPrefix is prepended to the group token when naming Keystone groups.
	// Projects are not prefixed — they are named after their node (name + node ID).
	GroupPrefix string `json:"group_prefix"`
	// ScopeParentID, when set, scopes the OS-only import to children of this parent project.
	// Projects outside this scope are ignored during the reverse-sync phase.
	ScopeParentID string `json:"scope_parent_id"`
	// ScopeParentName is the name-based alternative to ScopeParentID: the reconciler
	// resolves the project by name and creates it if it does not exist yet.
	// Ignored when ScopeParentID is set.
	ScopeParentName string `json:"scope_parent_name"`
	// DryRun runs reconciliation logic without making any writes. Useful for testing.
	DryRun bool `json:"dry_run"`
	// ManagedProjectTag is the OpenStack project tag used to identify projects created by
	// this system. Default: "dhbw-managed".
	ManagedProjectTag string `json:"managed_project_tag"`
	// ResourceIDTagPrefix is the prefix for the tag that encodes the linked request ID.
	// Full tag format: "<prefix><requestID>". Default: "dhbw-resource-id:".
	ResourceIDTagPrefix string `json:"resource_id_tag_prefix"`

	// ReleasedArchive archives the OpenStack project of a released leaf: disabled,
	// servers shelved, floating IPs released, quotas for new servers zero.
	ReleasedArchive bool `json:"released_archive"`
	// ReleasedDelete is when that project is deleted: "never", "after-grace" or
	// "immediately" (only "never" is supported yet).
	ReleasedDelete string `json:"released_delete"`
	// ReleasedDeleteGraceDays is added to the release day to compute the
	// deletion day in the pending-deletion tag. Default: 30.
	ReleasedDeleteGraceDays int `json:"released_delete_grace_days"`
	// ArchivedTagPrefix marks an archived project. Default: "archived:".
	ArchivedTagPrefix string `json:"archived_tag_prefix"`
	// PurgeDNSAndObjectStorage lets deleting a project delete its DNS zones
	// and object storage; off, they keep the project from being deleted.
	PurgeDNSAndObjectStorage bool `json:"purge_dns_and_object_storage"`
	// PurgeSkipStages names stages of emptying a project that are passed
	// over; what is in them stays behind (reconciler.PurgeStages).
	PurgeSkipStages []string `json:"purge_skip_stages"`
	// DeleteOrphanedUsers deletes accounts this service created once they
	// hold no project role; off by default, see reconciler.pruneOrphanedUsers.
	DeleteOrphanedUsers bool `json:"delete_orphaned_users"`
	// PendingDeletionTagPrefix is the tag prefix written to released projects when
	// DeleteReleasedProjects is false. The full tag is "<prefix><YYYY-MM-DD>".
	// Default: "pending-deletion:".
	PendingDeletionTagPrefix string `json:"pending_deletion_tag_prefix"`
	// TerminationTagPrefix is the tag prefix carrying a leaf's termination date,
	// so the intended end of life can be read off an OpenStack project without
	// going through this API. The value is the stored timestamp verbatim
	// (RFC3339), and the tag is absent while no date is set.
	// Default: "termination:".
	TerminationTagPrefix string `json:"termination_tag_prefix"`
	// StatusTagPrefix is the tag prefix carrying a leaf's lifecycle status, so an
	// outside workflow can select OpenStack projects by state — above all the
	// released ones it is supposed to clean up — without querying this API.
	// Full tag format: "<prefix><status>", e.g. "status:released".
	// Default: "status:". Empty switches the tag off.
	StatusTagPrefix string `json:"status_tag_prefix"`
	// ContactTagPrefix is the prefix for tags that record requester contact addresses.
	// One tag per requester email is written alongside the pending-deletion tag.
	// Default: "contact:".
	ContactTagPrefix string `json:"contact_tag_prefix"`

	// UsageEnabled records, once per finished day, what every project used
	// according to Nova's accounting (see package usage). Read-only towards
	// OpenStack. Default: true.
	UsageEnabled bool `json:"usage_enabled"`
	// UsageBackfillDays is how far back the first collection reaches into
	// Nova's records. Default: 365.
	UsageBackfillDays int `json:"usage_backfill_days"`
	// UsagePublicNetworks are the networks whose addresses count as public
	// IPv4 in the usage history besides floating IPs — networks VMs attach to
	// directly.
	UsagePublicNetworks []string `json:"usage_public_networks"`
}

type WebServerConfig struct {
	// Use dummy authentication middleware that allows all requests and sets a default user (for development/testing).
	// If false, the real authentication middleware with OIDC and API token support will be used.
	DummyAuth bool `json:"dummy_auth"`
	// The OIDC issuer URL for authentication
	OIDCIssuerURL string `json:"oidc_issuer_url" validate:"required,url"`
	// The OIDC client ID for authentication
	OIDCClientID string `json:"oidc_client_id" validate:"required"`
	// OIDCJWKSURL is the provider's key set, e.g.
	// https://sso.example/realms/x/protocol/openid-connect/certs.
	//
	// Set it and this service starts without asking the provider anything: the
	// keys are fetched when the first token needs checking and cached after
	// that. Empty means the address is discovered from the issuer at startup,
	// which requires the provider to be up in that second — on 2026-09-25 a
	// Keycloak outage during a power cut therefore kept every restarting pod
	// down long after our own cluster was healthy.
	OIDCJWKSURL string `json:"oidc_jwks_url"`
	// The bind string for the Gin web server (e.g., ":8082")
	GinBindString string `json:"gin_bind_string" validate:"required"`
	// CORSAllowedOrigins lists the exact browser origins allowed to call this
	// API cross-origin, e.g. "https://selfservice.dhbw.cloud". Empty (the
	// default) allows none: in BFF mode the SPA reaches the API same-origin
	// through its own host, so nothing cross-origin is needed at all. Every
	// entry is a full origin — scheme and host, no path, no wildcard.
	CORSAllowedOrigins []string `json:"cors_allowed_origins"`
	// APITokenTTLHours is how long an issued API token stays valid when the
	// request does not say. It matches dynamic-zones' API_TOKEN_TTL_HOURS so
	// that the two services do not answer the same question differently.
	APITokenTTLHours int `json:"api_token_ttl_hours"`
	// APITokenAllowNeverExpires permits tokens with no expiry.
	//
	// The code default is off, deliberately more conservative than the chart:
	// a service started without configuration should not mint permanent
	// credentials. Saying yes is a deployment's decision, and this one does —
	// see the chart, and note that what carries it is visibility rather than
	// expiry (every token lists its description and last use, and revoking is
	// one request).
	APITokenAllowNeverExpires bool `json:"api_token_allow_never_expires"`
	// OpenstackDashboardURL is the public address of the OpenStack dashboard
	// (Horizon), e.g. "https://newstack.dhbw.cloud". Set, the UI links every
	// project that exists in OpenStack straight to it; empty, it links nothing.
	// Configured rather than derived from the Keystone URL: the dashboard is a
	// different service on a different address, and a guess that is wrong
	// sends people to a page that does not exist.
	OpenstackDashboardURL string `json:"openstack_dashboard_url" validate:"omitempty,url"`
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
	// MaxAuthorizedUsers caps the participants one project may list. Each entry
	// costs the reconciler Keystone work on every run (a group plus an account
	// per member), so the ceiling bounds what a single request can trigger.
	// A course or department belongs in as ONE group token, not as N entries.
	MaxAuthorizedUsers int `json:"max_authorized_users"`
	// ChargeOSInUse bills a project the LARGER of its declared limit and what
	// OpenStack reports it actually consumes, in both the auto-approve check
	// and the budget rollup. Default on: with it off, shrinking a limit after
	// filling the project frees budget on paper while the servers keep running
	// (see tree.chargedQuota). The switch exists to turn the stricter
	// accounting off if a reconciler bug ever reports inflated usage — that
	// would otherwise lock budgets that are genuinely within their limits, and
	// waiting for a release to undo it is the worse failure mode.
	ChargeOSInUse bool `json:"charge_os_in_use"`
	// ChargeReleased keeps a released project booked against its budget until
	// OpenStack has actually deleted it. Default on: releasing only tags the
	// project for deletion, so the servers keep running and the capacity stays
	// occupied — freeing the budget at that moment lets the same hardware be
	// booked twice. See tree.Accounting.ChargeReleased.
	//
	// Turning it off restores the pre-2026-08 behaviour, which is a deliberate
	// trade: budgets free up immediately, at the price of over-booking for as
	// long as the deletion takes.
	ChargeReleased bool `json:"charge_released"`
	// ChargeArchived keeps the storage an archived project still holds booked
	// against its budget. See tree.Accounting.ChargeArchived.
	ChargeArchived bool `json:"charge_archived"`
	// UsagePrices are public-cloud list prices in euro that the root admins'
	// usage evaluation puts on what was used. Unset, it shows no value.
	UsagePrices *usage.Prices `json:"usage_prices,omitempty"`
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
	// Loaded before the struct so a bad catalogue stops the process here, with a
	// message naming the offending resource, rather than surfacing later as a
	// resource that exists in the portal and governs nothing.
	resourceCatalogue, err := loadResourceCatalogue()
	if err != nil {
		return AppConfiguration{}, err
	}

	cfg := AppConfiguration{
		Storage: common.StorageConfiguration{
			Type:             strings.ToLower(strings.TrimSpace(envconf.String("DB_TYPE", "memory"))),
			ConnectionString: envconf.String("DB_CONNECTION_STRING", "host=localhost user=postgres password=postgres dbname=openstack_management_api port=5432 sslmode=disable TimeZone=UTC"),
			AddMockData:      envconf.Bool("DB_ADD_MOCK_DATA", false),
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
			FederatedProvisioning: envconf.Bool("OPENSTACK_FEDERATED_PROVISIONING", false),
			FederatedIdPID:        envconf.String("OPENSTACK_FEDERATED_IDP_ID", "keycloak"),
			FederatedProtocolID:   envconf.String("OPENSTACK_FEDERATED_PROTOCOL_ID", "openid"),
			FederatedDomainID:     envconf.String("OPENSTACK_FEDERATED_DOMAIN_ID", "default"),
		},
		WebServer: WebServerConfig{
			DummyAuth:                 getEnvBool("API_DUMMY_AUTH", "API_DUMMY_AUTH", false),
			OIDCIssuerURL:             envconf.String("OIDC_ISSUER_URL", ""),
			OIDCClientID:              envconf.String("OIDC_CLIENT_ID", ""),
			OIDCJWKSURL:               envconf.String("OIDC_JWKS_URL", ""),
			GinBindString:             envconf.String("API_BIND", ":8083"),
			CORSAllowedOrigins:        parseCSVEnv(envconf.String("CORS_ALLOWED_ORIGINS", "")),
			APITokenTTLHours:          envconf.Int("API_TOKEN_TTL_HOURS", 24),
			APITokenAllowNeverExpires: envconf.Bool("API_TOKEN_ALLOW_NEVER_EXPIRES", false),
			OpenstackDashboardURL:     strings.TrimRight(envconf.String("OPENSTACK_DASHBOARD_URL", ""), "/"),
		},

		Reconciler: ReconcilerConfiguration{
			Enabled:                  envconf.Bool("RECONCILER_ENABLED", false),
			IntervalSeconds:          envconf.Int("RECONCILER_INTERVAL_SECONDS", 300),
			GroupPrefix:              envconf.String("RECONCILER_GROUP_PREFIX", "managed-"),
			ScopeParentID:            envconf.String("RECONCILER_SCOPE_PARENT_ID", ""),
			ScopeParentName:          envconf.String("RECONCILER_SCOPE_PARENT_NAME", ""),
			DryRun:                   envconf.Bool("RECONCILER_DRY_RUN", false),
			ManagedProjectTag:        envconf.String("RECONCILER_MANAGED_PROJECT_TAG", "managed"),
			ResourceIDTagPrefix:      envconf.String("RECONCILER_RESOURCE_ID_TAG_PREFIX", "managed-resource-id:"),
			ReleasedArchive:          envconf.Bool("RECONCILER_RELEASED_ARCHIVE", false),
			ReleasedDelete:           envconf.String("RECONCILER_RELEASED_DELETE", "never"),
			ReleasedDeleteGraceDays:  envconf.Int("RECONCILER_RELEASED_DELETE_GRACE_DAYS", 30),
			ArchivedTagPrefix:        envconf.String("RECONCILER_ARCHIVED_TAG_PREFIX", "archived:"),
			PurgeDNSAndObjectStorage: envconf.Bool("RECONCILER_PURGE_DNS_AND_OBJECT_STORAGE", false),
			PurgeSkipStages:          envconf.StringSlice("RECONCILER_PURGE_SKIP_STAGES", nil, strings.ToLower),
			DeleteOrphanedUsers:      envconf.Bool("RECONCILER_DELETE_ORPHANED_USERS", false),
			PendingDeletionTagPrefix: envconf.String("RECONCILER_PENDING_DELETION_TAG_PREFIX", "pending-deletion:"),
			ContactTagPrefix:         envconf.String("RECONCILER_CONTACT_TAG_PREFIX", "contact:"),
			TerminationTagPrefix:     envconf.String("RECONCILER_TERMINATION_TAG_PREFIX", "termination:"),
			StatusTagPrefix:          envconf.String("RECONCILER_STATUS_TAG_PREFIX", "status:"),
			UsageEnabled:             envconf.Bool("RECONCILER_USAGE_ENABLED", true),
			UsageBackfillDays:        envconf.Int("RECONCILER_USAGE_BACKFILL_DAYS", 365),
			UsagePublicNetworks:      envconf.StringSlice("RECONCILER_USAGE_PUBLIC_NETWORKS", nil),
		},
		RoleProvider: RoleProviderConfig{
			Type:     envconf.String("ROLE_PROVIDER", "mock"),
			URL:      envconf.String("ROLE_PROVIDER_URL", ""),
			APIToken: envconf.String("ROLE_PROVIDER_API_TOKEN", ""),
		},
		DevMode:               getEnvString("API_MODE", "API_MODE", "production") == "development",
		RootAdminTokens:       parseCSVEnv(envconf.String("ROOT_ADMIN_TOKENS", "")),
		ProjectDefinitions:    resourceCatalogue,
		ServiceTimeoutSeconds: envconf.Int("SERVICE_TIMEOUT_SECONDS", 30),
		MaxAuthorizedUsers:    envconf.Int("API_MAX_AUTHORIZED_USERS", common.DefaultMaxAuthorizedUsers),
		ChargeOSInUse:         envconf.Bool("API_CHARGE_OS_IN_USE", true),
		ChargeReleased:        envconf.Bool("API_CHARGE_RELEASED", true),
		ChargeArchived:        envconf.Bool("API_CHARGE_ARCHIVED", false),
	}
	// A JSON object rather than three numbers: envconf reads no floats, and the
	// prices belong together.
	if raw := strings.TrimSpace(envconf.String("API_USAGE_PRICES", "")); raw != "" {
		var prices usage.Prices
		if err := json.Unmarshal([]byte(raw), &prices); err != nil {
			return AppConfiguration{}, fmt.Errorf("API_USAGE_PRICES: %w", err)
		}
		cfg.UsagePrices = &prices
	}

	if err := validateConfig(cfg); err != nil {
		return AppConfiguration{}, err
	}

	return cfg, nil
}

// resourceCatalogueEnv holds the catalogue as a JSON array of ManagedProject.
// Empty falls back to the built-in defaults below.
const resourceCatalogueEnv = "RESOURCE_DEFINITIONS"

// loadResourceCatalogue reads the managed resources for this deployment.
//
// It used to be a function called loadProjectDefinitionsOrDefaults that had no
// load path at all — the name promised a fallback to defaults, and defaults were
// the only thing it ever returned. The catalogue is deployment configuration
// now: staging and production carry different GPU flavours and different
// networks, and adding one should be an Ansible run, not a release.
//
// A malformed or invalid catalogue is a startup failure. The alternative — log
// and fall back to the defaults — would replace the operator's catalogue with a
// different one while reporting success, and the difference would only show when
// somebody's resource had quietly stopped existing.
func loadResourceCatalogue() ([]common.ManagedProject, error) {
	raw := strings.TrimSpace(envconf.String(resourceCatalogueEnv, ""))
	if raw == "" {
		// Validated too, and not as ceremony: the defaults are edited by hand in
		// this file, and the same mistakes are available there.
		defs := defaultResourceCatalogue()
		if err := common.ValidateManagedProjects(defs); err != nil {
			return nil, fmt.Errorf("built-in resource catalogue is invalid: %w", err)
		}
		return defs, nil
	}

	var defs []common.ManagedProject
	if err := json.Unmarshal([]byte(raw), &defs); err != nil {
		return nil, fmt.Errorf("%s is not a valid JSON array of resource definitions: %w", resourceCatalogueEnv, err)
	}
	if err := common.ValidateManagedProjects(defs); err != nil {
		return nil, fmt.Errorf("%s: %w", resourceCatalogueEnv, err)
	}

	return defs, nil
}

func defaultResourceCatalogue() []common.ManagedProject {

	// Default set — single source of truth for UI display AND OpenStack quota mapping.
	// ShowOnUI: true  → returned to the frontend via /v1/config (user-configurable).
	// Static: true    → quota is fixed at Default; applied once at project creation.
	return []common.ManagedProject{
		// ── User-configurable resources (shown on UI) ────────────
		{
			ID: "cores", Name: "Cores", Default: 4, Min: 1, Max: 100000,
			Group:             "Compute",
			Message:           "1 - 100000 cores",
			ShowOnUI:          true,
			OSQuotaField:      "cores",
			OSLinkedField:     "instances", // instances cap mirrors cores
			OSOvercommitCheck: true,
		},
		{
			ID: "ram", Name: "RAM", Default: 16, Min: 1, Max: 256000,
			Group:             "Compute",
			Unit:              "GB",
			Message:           "1 GB - 256 TB",
			ShowOnUI:          true,
			OSQuotaField:      "ram",
			OSMultiplier:      1024, // stored in GB, OpenStack expects MB
			OSOvercommitCheck: true,
		},
		{
			ID: "storage", Name: "Storage", Default: 50, Min: 1, Max: 100000,
			Group:    "Storage",
			Unit:     "GB",
			Message:  "1 GB - 100 TB",
			ShowOnUI: true,
			// No OSMultiplier: OpenStack counts `gigabytes` in GB and so do we.
			OSQuotaField: "gigabytes",
			// Measured, like cores and ram — and for the same reason. Without
			// this flag ProjectInUse skips the resource entirely, so storage
			// never reaches OSInUse and the accounting keeps billing the
			// declared limit. That leaves the shrink-after-filling loophole
			// wide open for exactly the resource that is cheapest to abuse:
			// request 500 GB, fill it with volumes, shrink to 5. OpenStack
			// accepts the smaller quota and keeps the volumes; the books say 5.
			// The number was available all along — it rides in the same quota
			// detail response the reconciler already fetches.
			OSOvercommitCheck: true,
		},

		// ── Static network/storage quotas (not shown on UI, fixed at project creation) ─
		// To change a default, update the Default field here. The OSQuotaField drives the
		// mapping to OpenStack — no other file needs to change.
		{Group: "Infrastructure", ID: "networks", Name: "Networks", Default: envconf.Int("RECONCILER_DEFAULT_NETWORKS", 2),
			Static: true, OSQuotaField: "networks"},
		{Group: "Infrastructure", ID: "subnets", Name: "Subnets", Default: envconf.Int("RECONCILER_DEFAULT_SUBNETS", 4),
			Static: true, OSQuotaField: "subnets"},
		{Group: "Infrastructure", ID: "ports", Name: "Ports", Default: envconf.Int("RECONCILER_DEFAULT_PORTS", 50),
			Static: true, OSQuotaField: "ports"},
		{Group: "Infrastructure", ID: "routers", Name: "Routers", Default: envconf.Int("RECONCILER_DEFAULT_ROUTERS", 1),
			Static: true, OSQuotaField: "routers"},
		{Group: "Infrastructure", ID: "floating_ips", Name: "Floating IPs", Default: envconf.Int("RECONCILER_DEFAULT_FLOATING_IPS", 2),
			Static: true, OSQuotaField: "floating_ips"},
		{Group: "Infrastructure", ID: "security_groups", Name: "Security Groups", Default: envconf.Int("RECONCILER_DEFAULT_SECURITY_GROUPS", 10),
			Static: true, OSQuotaField: "security_groups"},
		{Group: "Infrastructure", ID: "volumes", Name: "Volumes", Default: envconf.Int("RECONCILER_DEFAULT_VOLUMES", 10),
			Static: true, OSQuotaField: "volumes"},
		{Group: "Infrastructure", ID: "snapshots", Name: "Snapshots", Default: envconf.Int("RECONCILER_DEFAULT_SNAPSHOTS", 10),
			Static: true, OSQuotaField: "snapshots"},
	}
}

func logAppConfig(appConfig AppConfiguration, log *zap.SugaredLogger) {
	var appConfigJson []byte
	var err error

	// Redact ALL secrets before marshalling — the whole config is logged below.
	appConfig.Openstack.ApplicationCredentialSecret = redact.Secret(appConfig.Openstack.ApplicationCredentialSecret)
	appConfig.Openstack.Password = redact.Secret(appConfig.Openstack.Password)
	appConfig.RoleProvider.APIToken = redact.Secret(appConfig.RoleProvider.APIToken)
	appConfig.Storage.ConnectionString = redact.ConnString(appConfig.Storage.ConnectionString)

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
