package osclient

import (
	"fmt"

	blockquotas "github.com/gophercloud/gophercloud/openstack/blockstorage/extensions/quotasets"
	computequotas "github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/quotasets"
	networkquotas "github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/quotas"
	"go.uber.org/zap"
)

// ProjectQuotaDetail holds both the configured quota limit and current in-use values.
// Used by the reconciler to detect overcommitted projects.
type ProjectQuotaDetail struct {
	ProjectID string
	Limit     QuotaSet
	InUse     QuotaSet
}

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

// UpdateManagedQuotas updates only the quota fields controlled by the resource management
// system (compute cores/RAM/instances and block-storage gigabytes). Network quotas and
// other unmanaged fields are intentionally left untouched.
func (c *OpenStackClient) UpdateManagedQuotas(projectID string, quotas QuotaSet) error {
	computeOpts := computequotas.UpdateOpts{
		Instances: &quotas.Instances,
		Cores:     &quotas.Cores,
		RAM:       &quotas.RAM,
	}
	if _, err := computequotas.Update(c.Compute, projectID, computeOpts).Extract(); err != nil {
		return fmt.Errorf("compute quotas: %w", err)
	}

	blockOpts := blockquotas.UpdateOpts{
		Gigabytes: &quotas.Gigabytes,
	}
	if _, err := blockquotas.Update(c.Block, projectID, blockOpts).Extract(); err != nil {
		return fmt.Errorf("block storage quotas: %w", err)
	}

	c.logger.Info("Managed quotas updated",
		zap.String("project_id", projectID),
		zap.Int("instances", quotas.Instances),
		zap.Int("cores", quotas.Cores),
		zap.Int("ram_mb", quotas.RAM),
		zap.Int("gigabytes", quotas.Gigabytes))

	return nil
}

// GetProjectQuotaDetail returns both the configured limits and current in-use values for a
// project's compute and block-storage resources. Used by the reconciler to detect overcommit.
func (c *OpenStackClient) GetProjectQuotaDetail(projectID string) (*ProjectQuotaDetail, error) {
	detail := &ProjectQuotaDetail{ProjectID: projectID}

	compute, err := computequotas.GetDetail(c.Compute, projectID).Extract()
	if err != nil {
		return nil, fmt.Errorf("compute quota detail: %w", err)
	}
	detail.Limit.Cores = compute.Cores.Limit
	detail.Limit.RAM = compute.RAM.Limit
	detail.Limit.Instances = compute.Instances.Limit
	detail.InUse.Cores = compute.Cores.InUse
	detail.InUse.RAM = compute.RAM.InUse
	detail.InUse.Instances = compute.Instances.InUse

	// gophercloud's block-storage quotasets package only exposes Get (limits), not GetDetail
	// (limits + in-use). We therefore only populate the limit for storage; InUse.Gigabytes
	// remains 0 (unknown). Overcommit detection for storage is skipped as a result.
	blk, err := blockquotas.Get(c.Block, projectID).Extract()
	if err != nil {
		return nil, fmt.Errorf("block storage quota detail: %w", err)
	}
	detail.Limit.Gigabytes = blk.Gigabytes
	detail.InUse.Gigabytes = 0 // not available via this API

	return detail, nil
}
