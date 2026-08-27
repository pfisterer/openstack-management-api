package osclient

import (
	"cmp"
	"crypto/tls"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack"
	"go.uber.org/zap"
)

// newBlockStorageV3 resolves the volume service under either name it can carry
// in the catalog: gophercloud v1 only looks for the legacy type "volumev3",
// while newer clouds (DevStack since Xena) advertise it as "block-storage" and
// would otherwise fail the whole client with "No suitable endpoint could be
// found in the service catalog".
func newBlockStorageV3(provider *gophercloud.ProviderClient, eo gophercloud.EndpointOpts) (*gophercloud.ServiceClient, error) {
	sc, err := openstack.NewBlockStorageV3(provider, eo)
	if err == nil {
		return sc, nil
	}

	const altType = "block-storage"
	altOpts := eo
	altOpts.ApplyDefaults(altType)
	url, altErr := provider.EndpointLocator(altOpts)
	if altErr != nil {
		return nil, err // report the lookup under the well-known name
	}
	return &gophercloud.ServiceClient{ProviderClient: provider, Endpoint: url, Type: altType}, nil
}

// OpenStackClient is the main OpenStack administrative client
// holding service clients for identity, compute, network, and block storage.
type OpenStackClient struct {
	Identity *gophercloud.ServiceClient
	Compute  *gophercloud.ServiceClient
	Network  *gophercloud.ServiceClient
	Block    *gophercloud.ServiceClient
	Image    *gophercloud.ServiceClient
	logger   *zap.Logger
	log      *zap.SugaredLogger
	region   string

	managedProjectTag   string
	resourceIDTagPrefix string

	federatedProvisioning bool
	federatedIdPID        string
	federatedProtocolID   string
	federatedDomainID     string
}

// SetTagConfig sets the managed-project tag and resource-ID tag prefix from config.
// Must be called before any project tag operations.
func (c *OpenStackClient) SetTagConfig(managedProjectTag, resourceIDTagPrefix string) {
	c.managedProjectTag = managedProjectTag
	c.resourceIDTagPrefix = resourceIDTagPrefix
}

// SetFederationConfig enables federated pre-provisioning and sets the IdP/protocol that a
// pre-created user is bound to. See FindOrCreateUser for why this matters on ephemeral OIDC
// mappings. When enabled is false, FindOrCreateUser creates plain local accounts.
func (c *OpenStackClient) SetFederationConfig(enabled bool, idpID, protocolID, domainID string) {
	c.federatedProvisioning = enabled
	c.federatedIdPID = idpID
	c.federatedProtocolID = protocolID
	c.federatedDomainID = cmp.Or(domainID, "default")
}

// NewOSAdmin creates a new OpenStack administrative client with default region.
func NewOSAdmin(authURL, token, projectID string, logger *zap.Logger, sugaredLogger *zap.SugaredLogger) (*OpenStackClient, error) {
	return NewOSAdminWithRegion(authURL, token, projectID, "RegionOne", false, logger, sugaredLogger)
}

// NewOSAdminWithRegion creates a new OpenStack administrative client with custom region.
func NewOSAdminWithRegion(authURL, token, projectID, region string, insecure bool, logger *zap.Logger, sugaredLogger *zap.SugaredLogger) (*OpenStackClient, error) {
	if region == "" {
		region = "RegionOne"
	}

	logger, sugaredLogger = normalizeLoggers(logger, sugaredLogger)

	provider, err := openstack.NewClient(authURL)
	if err != nil {
		return nil, fmt.Errorf("create provider client: %w", err)
	}
	provider.TokenID = token

	if insecure {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		provider.HTTPClient = http.Client{Transport: transport}
		sugaredLogger.Warn("TLS certificate verification disabled")
	}

	authOpts := gophercloud.AuthOptions{
		IdentityEndpoint: authURL,
		TokenID:          token,
	}
	if projectID != "" {
		authOpts.Scope = &gophercloud.AuthScope{
			ProjectID: projectID,
		}
	}
	if err := openstack.Authenticate(provider, authOpts); err != nil {
		return nil, fmt.Errorf("authenticate provider: %w", err)
	}

	endpointOpts := gophercloud.EndpointOpts{
		Availability: gophercloud.AvailabilityPublic,
		Region:       region,
	}

	client, err := buildServiceClients(provider, endpointOpts)
	if err != nil {
		return nil, err
	}
	client.region = region
	client.logger = logger
	client.log = sugaredLogger
	return client, nil
}

// buildServiceClients resolves every OpenStack service this package talks to.
//
// One place, because there are two ways in — an application credential and a
// service user's password — and both build the same struct. Adding the image
// client to only one of them would have left password auth with a nil Image and
// a panic on the first image grant, which is precisely the path production uses.
func buildServiceClients(provider *gophercloud.ProviderClient, eo gophercloud.EndpointOpts) (*OpenStackClient, error) {
	// Identity takes no endpoint options, which is what the application-credential
	// path did before this was one function. The password path passed the region
	// instead — the two had drifted apart, and unifying them means picking one.
	// This one, because it is the path every current deployment runs on.
	//
	// The difference only shows on a cloud whose catalogue carries more than one
	// identity endpoint; Keystone is not region-scoped, so on ours both resolve
	// the same entry.
	identity, err := openstack.NewIdentityV3(provider, gophercloud.EndpointOpts{})
	if err != nil {
		return nil, fmt.Errorf("create identity client: %w", err)
	}
	compute, err := openstack.NewComputeV2(provider, eo)
	if err != nil {
		return nil, fmt.Errorf("create compute client: %w", err)
	}
	network, err := openstack.NewNetworkV2(provider, eo)
	if err != nil {
		return nil, fmt.Errorf("create network client: %w", err)
	}
	block, err := newBlockStorageV3(provider, eo)
	if err != nil {
		return nil, fmt.Errorf("create block storage client: %w", err)
	}
	// Glance, for image availabilities: granting one is an image MEMBER, which
	// no other service can express.
	image, err := openstack.NewImageServiceV2(provider, eo)
	if err != nil {
		return nil, fmt.Errorf("create image client: %w", err)
	}

	return &OpenStackClient{
		Identity: identity,
		Compute:  compute,
		Network:  network,
		Block:    block,
		Image:    image,
	}, nil
}

// NewOSAdminWithAppCredential creates a new OpenStack admin client using application credentials.
func NewOSAdminWithAppCredential(
	authURL, appCredID, appCredSecret, projectID, region string,
	insecure bool,
	logger *zap.Logger,
	sugaredLogger *zap.SugaredLogger,
) (*OpenStackClient, error) {
	// Application credentials must not include an explicit project scope — the scope is
	// embedded in the credential by Keystone. Setting authOpts.Scope causes a 401 because
	// Keystone treats it as an attempt to override the credential's own project binding.
	authOpts := gophercloud.AuthOptions{
		IdentityEndpoint:            authURL,
		ApplicationCredentialID:     appCredID,
		ApplicationCredentialSecret: appCredSecret,
		AllowReauth:                 true,
	}
	return newOSAdmin(authURL, authOpts, region, insecure, logger, sugaredLogger)
}

// PasswordAuthOpts describes a service user and the scope its token should have.
// Scope precedence: SystemScope > DomainName > project (ProjectID, or ProjectName
// together with ProjectDomainName). Leaving all of them unset yields an unscoped
// token, which cannot call anything — Keystone grants roles per scope.
type PasswordAuthOpts struct {
	Username       string
	Password       string
	UserDomainName string

	SystemScope       bool
	DomainName        string
	ProjectID         string
	ProjectName       string
	ProjectDomainName string
}

// NewOSAdminWithPassword creates a client authenticating a service user with a
// password. Unlike an application credential this can request system or domain
// scope, which is what clouds enforcing the modern RBAC defaults require for
// project creation and cross-project role assignments.
func NewOSAdminWithPassword(
	authURL string,
	opts PasswordAuthOpts,
	region string,
	insecure bool,
	logger *zap.Logger,
	sugaredLogger *zap.SugaredLogger,
) (*OpenStackClient, error) {
	authOpts := gophercloud.AuthOptions{
		IdentityEndpoint: authURL,
		Username:         opts.Username,
		Password:         opts.Password,
		DomainName:       opts.UserDomainName,
		AllowReauth:      true,
	}

	switch {
	case opts.SystemScope:
		authOpts.Scope = &gophercloud.AuthScope{System: true}
	case opts.DomainName != "":
		authOpts.Scope = &gophercloud.AuthScope{DomainName: opts.DomainName}
	case opts.ProjectID != "":
		authOpts.Scope = &gophercloud.AuthScope{ProjectID: opts.ProjectID}
	case opts.ProjectName != "":
		authOpts.Scope = &gophercloud.AuthScope{
			ProjectName: opts.ProjectName,
			DomainName:  cmp.Or(opts.ProjectDomainName, opts.UserDomainName),
		}
	}

	// Note: AuthOptions.DomainName stays set — it identifies the *user's* domain
	// and is required for username auth; the scope above is independent of it.

	return newOSAdmin(authURL, authOpts, region, insecure, logger, sugaredLogger)
}

// newOSAdmin authenticates with the given options and builds the service clients.
// Shared by every constructor above — only the AuthOptions differ between them.
func newOSAdmin(
	authURL string,
	authOpts gophercloud.AuthOptions,
	region string,
	insecure bool,
	logger *zap.Logger,
	sugaredLogger *zap.SugaredLogger,
) (*OpenStackClient, error) {
	if region == "" {
		region = "microstack"
	}

	logger, sugaredLogger = normalizeLoggers(logger, sugaredLogger)

	sugaredLogger.Debugw("Creating OpenStack client",
		"auth_url", authURL,
		"region", region,
		"insecure", insecure)

	provider, err := openstack.NewClient(authURL)
	if err != nil {
		return nil, err
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if insecure {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		sugaredLogger.Warn("TLS certificate verification disabled")
	}

	provider.HTTPClient = http.Client{Transport: transport}

	if err := openstack.Authenticate(provider, authOpts); err != nil {
		return nil, fmt.Errorf("authentication failed: %w", err)
	}

	sugaredLogger.Info("OpenStack authentication successful")

	eo := gophercloud.EndpointOpts{
		Region:       region,
		Availability: gophercloud.AvailabilityPublic,
	}

	client, err := buildServiceClients(provider, eo)
	if err != nil {
		return nil, err
	}
	client.region = region
	client.logger = logger
	client.log = sugaredLogger
	return client, nil
}

func normalizeLoggers(logger *zap.Logger, sugaredLogger *zap.SugaredLogger) (*zap.Logger, *zap.SugaredLogger) {
	if logger == nil {
		if sugaredLogger != nil {
			logger = sugaredLogger.Desugar()
		} else {
			logger = zap.NewNop()
		}
	}

	if sugaredLogger == nil {
		sugaredLogger = logger.Sugar()
	}

	return logger, sugaredLogger
}
