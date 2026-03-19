package osclient

import (
	"fmt"

	blockquotas "github.com/gophercloud/gophercloud/openstack/blockstorage/extensions/quotasets"
	computequotas "github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/quotasets"
	networkquotas "github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/quotas"
	"go.uber.org/zap"
)

// QuotaSet represents combined quotas for a project.
type QuotaSet struct {
	ProjectID string
	// Compute quotas
	Instances int
	Cores     int
	RAM       int
	// Network quotas
	Networks       int
	Subnets        int
	Ports          int
	Routers        int
	FloatingIPs    int
	SecurityGroups int
	// Block storage quotas
	Volumes   int
	Snapshots int
	Gigabytes int
}

// GetProjectQuotas retrieves compute, network, and block storage quotas for a project.
func (c *OpenStackClient) GetProjectQuotas(projectID string) (*QuotaSet, error) {
	result := QuotaSet{ProjectID: projectID}

	compute, err := computequotas.GetDetail(c.Compute, projectID).Extract()
	if err != nil {
		return nil, fmt.Errorf("compute quotas: %w", err)
	}
	result.Instances = compute.Instances.Limit
	result.Cores = compute.Cores.Limit
	result.RAM = compute.RAM.Limit

	net, err := networkquotas.Get(c.Network, projectID).Extract()
	if err != nil {
		return nil, fmt.Errorf("network quotas: %w", err)
	}
	result.Networks = net.Network
	result.Subnets = net.Subnet
	result.Ports = net.Port
	result.Routers = net.Router
	result.FloatingIPs = net.FloatingIP
	result.SecurityGroups = net.SecurityGroup

	blk, err := blockquotas.Get(c.Block, projectID).Extract()
	if err != nil {
		return nil, fmt.Errorf("block storage quotas: %w", err)
	}
	result.Volumes = blk.Volumes
	result.Snapshots = blk.Snapshots
	result.Gigabytes = blk.Gigabytes

	return &result, nil
}

// UpdateProjectQuotas updates quotas for a project.
func (c *OpenStackClient) UpdateProjectQuotas(projectID string, quotas QuotaSet) error {
	computeOpts := computequotas.UpdateOpts{
		Instances: &quotas.Instances,
		Cores:     &quotas.Cores,
		RAM:       &quotas.RAM,
	}
	if _, err := computequotas.Update(c.Compute, projectID, computeOpts).Extract(); err != nil {
		return fmt.Errorf("compute quotas: %w", err)
	}

	networkOpts := networkquotas.UpdateOpts{
		Network:       &quotas.Networks,
		Subnet:        &quotas.Subnets,
		Port:          &quotas.Ports,
		Router:        &quotas.Routers,
		FloatingIP:    &quotas.FloatingIPs,
		SecurityGroup: &quotas.SecurityGroups,
	}
	if _, err := networkquotas.Update(c.Network, projectID, networkOpts).Extract(); err != nil {
		return fmt.Errorf("network quotas: %w", err)
	}

	blockOpts := blockquotas.UpdateOpts{
		Volumes:   &quotas.Volumes,
		Snapshots: &quotas.Snapshots,
		Gigabytes: &quotas.Gigabytes,
	}
	if _, err := blockquotas.Update(c.Block, projectID, blockOpts).Extract(); err != nil {
		return fmt.Errorf("block storage quotas: %w", err)
	}

	c.logger.Info("Quotas updated",
		zap.String("project_id", projectID),
		zap.Int("instances", quotas.Instances),
		zap.Int("cores", quotas.Cores),
		zap.Int("ram_mb", quotas.RAM))

	return nil
}
