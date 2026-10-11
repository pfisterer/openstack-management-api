package osclient

import (
	"cmp"
	"fmt"

	"go.uber.org/zap"
)

// Config describes OpenStack authentication and client settings, as the
// deployment configures them (OS_* / OPENSTACK_* environment variables).
//
// Two authentication methods are supported; configure exactly one (see
// AuthMethod). Application credentials are the simpler choice but are always
// project-scoped, which is a hard limit on clouds that enforce the modern RBAC
// scope defaults: creating projects or assigning roles across projects needs a
// system- or domain-scoped token there, and no application credential can carry
// one. Password auth for a service user exists for exactly that case, because it
// can request system or domain scope.
type Config struct {
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
	AuthNone          = "none"
	AuthAppCredential = "application_credential"
	AuthPassword      = "password"
)

// AuthMethod reports which authentication method the configuration selects.
// Application credentials win when both are present, so a leftover OS_USERNAME
// from a sourced openrc cannot silently take over an explicit credential.
func (c Config) AuthMethod() string {
	switch {
	case c.ApplicationCredentialID != "" && c.ApplicationCredentialSecret != "":
		return AuthAppCredential
	case c.Username != "" && c.Password != "":
		return AuthPassword
	default:
		return AuthNone
	}
}

// NewFromConfig builds the OpenStack client for the configured
// authentication method. Application credentials are the default; password auth
// for a service user exists because an application credential is always
// project-scoped and therefore cannot create projects on a cloud that enforces
// the modern RBAC scope defaults.
func NewFromConfig(cfg Config, log *zap.Logger, logger *zap.SugaredLogger) (*OpenStackClient, error) {
	switch cfg.AuthMethod() {
	case AuthAppCredential:
		logger.Infow("OpenStack auth: application credential", "auth_url", cfg.AuthURL)
		return newWithAppCredential(
			cfg.AuthURL,
			cfg.ApplicationCredentialID,
			cfg.ApplicationCredentialSecret,
			cfg.ProjectID,
			cfg.Region,
			cfg.Insecure,
			log,
			logger,
		)

	case AuthPassword:
		opts := PasswordAuthOpts{
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
		return newWithPassword(cfg.AuthURL, opts, cfg.Region, cfg.Insecure, log, logger)

	default:
		return nil, fmt.Errorf("no OpenStack credentials configured: set OS_APPLICATION_CREDENTIAL_ID/_SECRET or OS_USERNAME/OS_PASSWORD")
	}
}
