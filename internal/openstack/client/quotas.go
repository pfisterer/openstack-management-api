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

	// GetUsage, not Get: Cinder answers GET /os-quota-sets/{id}?usage=true with
	// limit AND in_use, and gophercloud has exposed it all along — under a name
	// that does not match the compute package's GetDetail, which is how it came
	// to be believed missing. Until 2026-08-26 this read Get (limits only) and
	// hard-coded InUse.Gigabytes = 0, so a project with volumes reported no
	// storage in use: the accounting billed the declared limit, and the
	// shrink-after-filling loophole stayed open for storage while it was closed
	// for cores and RAM. Measured on staging: 3 GB of volumes, reported as 0.
	usage, err := blockquotas.GetUsage(c.Block, projectID).Extract()
	if err != nil {
		return nil, fmt.Errorf("block storage quota usage: %w", err)
	}
	detail.Limit.Gigabytes = usage.Gigabytes.Limit
	detail.InUse.Gigabytes = usage.Gigabytes.InUse

	return detail, nil
}
