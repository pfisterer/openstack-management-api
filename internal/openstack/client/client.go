package osclient

import (
	"crypto/tls"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack"
	"go.uber.org/zap"
)

// OpenStackClient is the main OpenStack administrative client
// holding service clients for identity, compute, network, and block storage.
type OpenStackClient struct {
	Identity *gophercloud.ServiceClient
	Compute  *gophercloud.ServiceClient
	Network  *gophercloud.ServiceClient
	Block    *gophercloud.ServiceClient
	logger   *zap.Logger
	log      *zap.SugaredLogger
	region   string

	managedProjectTag   string
	resourceIDTagPrefix string
}

// SetTagConfig sets the managed-project tag and resource-ID tag prefix from config.
// Must be called before any project tag operations.
func (c *OpenStackClient) SetTagConfig(managedProjectTag, resourceIDTagPrefix string) {
	c.managedProjectTag = managedProjectTag
	c.resourceIDTagPrefix = resourceIDTagPrefix
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

	identityClient, err := openstack.NewIdentityV3(provider, gophercloud.EndpointOpts{})
	if err != nil {
		return nil, fmt.Errorf("create identity client: %w", err)
	}

	computeClient, err := openstack.NewComputeV2(provider, endpointOpts)
	if err != nil {
		return nil, fmt.Errorf("create compute client: %w", err)
	}

	blockClient, err := openstack.NewBlockStorageV3(provider, endpointOpts)
	if err != nil {
		return nil, fmt.Errorf("create block storage client: %w", err)
	}

	networkClient, err := openstack.NewNetworkV2(provider, endpointOpts)
	if err != nil {
		return nil, fmt.Errorf("create network client: %w", err)
	}

	return &OpenStackClient{
		Identity: identityClient,
		Compute:  computeClient,
		Network:  networkClient,
		Block:    blockClient,
		region:   region,
		logger:   logger,
		log:      sugaredLogger,
	}, nil
}

// NewOSAdminWithAppCredential creates a new OpenStack admin client using application credentials.
func NewOSAdminWithAppCredential(
	authURL, appCredID, appCredSecret, projectID, region string,
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

	// Application credentials must not include an explicit project scope — the scope is
	// embedded in the credential by Keystone. Setting authOpts.Scope causes a 401 because
	// Keystone treats it as an attempt to override the credential's own project binding.
	authOpts := gophercloud.AuthOptions{
		IdentityEndpoint:            authURL,
		ApplicationCredentialID:     appCredID,
		ApplicationCredentialSecret: appCredSecret,
		AllowReauth:                 true,
	}

	if err := openstack.Authenticate(provider, authOpts); err != nil {
		return nil, fmt.Errorf("authentication failed: %w", err)
	}

	sugaredLogger.Info("OpenStack authentication successful")

	eo := gophercloud.EndpointOpts{
		Region:       region,
		Availability: gophercloud.AvailabilityPublic,
	}

	identity, err := openstack.NewIdentityV3(provider, eo)
	if err != nil {
		return nil, err
	}
	compute, err := openstack.NewComputeV2(provider, eo)
	if err != nil {
		return nil, err
	}
	network, err := openstack.NewNetworkV2(provider, eo)
	if err != nil {
		return nil, err
	}
	block, err := openstack.NewBlockStorageV3(provider, eo)
	if err != nil {
		return nil, err
	}

	return &OpenStackClient{
		Identity: identity,
		Compute:  compute,
		Network:  network,
		Block:    block,
		region:   region,
		logger:   logger,
		log:      sugaredLogger,
	}, nil
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
