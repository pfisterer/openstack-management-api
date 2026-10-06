package osclient

import (
	"fmt"

	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/quotasets"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/shelveunshelve"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/layer3/floatingips"
	networkquotas "github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/quotas"
	"github.com/gophercloud/gophercloud/pagination"
)

// What archiving a released project needs: its servers out of the way, its
// floating IPs back in the pool, and nothing new started in it. All of it is
// reversible except the floating IPs, which are scarce and released on purpose.

// ServerInfo is one server of a project.
type ServerInfo struct {
	ID        string
	Name      string
	Status    string
	ProjectID string
}

// Shelved reports whether the server is already off its hypervisor or on its
// way there — nothing more to do for it.
func (s ServerInfo) Shelved() bool {
	switch s.Status {
	case "SHELVED", "SHELVED_OFFLOADED":
		return true
	}
	return false
}

// ListProjectServers lists every server of a project, whatever its state.
func (c *OpenStackClient) ListProjectServers(projectID string) ([]ServerInfo, error) {
	var out []ServerInfo
	err := servers.List(c.computeSvc(), servers.ListOpts{AllTenants: true, TenantID: projectID}).
		EachPage(func(page pagination.Page) (bool, error) {
			list, err := servers.ExtractServers(page)
			if err != nil {
				return false, err
			}
			for _, s := range list {
				out = append(out, ServerInfo{ID: s.ID, Name: s.Name, Status: s.Status, ProjectID: s.TenantID})
			}
			return true, nil
		})
	if err != nil {
		return nil, fmt.Errorf("list servers of %s: %w", projectID, err)
	}
	return out, nil
}

// ShelveServer removes a server from its hypervisor, keeping its disk as an
// image; unshelving brings it back.
func (c *OpenStackClient) ShelveServer(serverID string) error {
	if err := shelveunshelve.Shelve(c.computeSvc(), serverID).ExtractErr(); err != nil {
		return fmt.Errorf("shelve %s: %w", serverID, err)
	}
	return nil
}

// FloatingIPInfo is one floating IP held by a project.
type FloatingIPInfo struct {
	ID        string
	Address   string
	ProjectID string
}

// ListProjectFloatingIPs lists the floating IPs a project holds.
func (c *OpenStackClient) ListProjectFloatingIPs(projectID string) ([]FloatingIPInfo, error) {
	var out []FloatingIPInfo
	err := floatingips.List(c.networkSvc(), floatingips.ListOpts{ProjectID: projectID}).
		EachPage(func(page pagination.Page) (bool, error) {
			list, err := floatingips.ExtractFloatingIPs(page)
			if err != nil {
				return false, err
			}
			for _, f := range list {
				out = append(out, FloatingIPInfo{ID: f.ID, Address: f.FloatingIP, ProjectID: firstNonEmpty(f.ProjectID, f.TenantID)})
			}
			return true, nil
		})
	if err != nil {
		return nil, fmt.Errorf("list floating IPs of %s: %w", projectID, err)
	}
	return out, nil
}

// ReleaseFloatingIP gives a floating IP back to the pool. Not reversible: the
// address may go to someone else.
func (c *OpenStackClient) ReleaseFloatingIP(id string) error {
	if err := floatingips.Delete(c.networkSvc(), id).ExtractErr(); err != nil {
		return fmt.Errorf("release floating IP %s: %w", id, err)
	}
	return nil
}

// FreezeProjectQuotas sets the quotas that let something new run to zero:
// instances, cores, RAM and floating IPs. Only these — Neutron and Cinder may
// refuse a quota below what is in use, and networks, ports and volumes stay
// in place until the project is deleted.
func (c *OpenStackClient) FreezeProjectQuotas(projectID string) error {
	zero := 0
	if _, err := quotasets.Update(c.computeSvc(), projectID, quotasets.UpdateOpts{
		Instances: &zero, Cores: &zero, RAM: &zero,
	}).Extract(); err != nil {
		return fmt.Errorf("compute quotas: %w", err)
	}
	if _, err := networkquotas.Update(c.networkSvc(), projectID, networkquotas.UpdateOpts{
		FloatingIP: &zero,
	}).Extract(); err != nil {
		return fmt.Errorf("network quotas: %w", err)
	}
	return nil
}
