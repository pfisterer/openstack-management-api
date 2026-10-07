package osclient

import (
	"fmt"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/usage"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/layer3/floatingips"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/ports"
	"github.com/gophercloud/gophercloud/pagination"
)

// TenantUsage is what Nova accounted for one project over a period: how long
// its servers existed, weighted by their size. Nova keeps these books itself
// (os-simple-tenant-usage), from the instance records — so they reach back as
// far as those records do, and include servers deleted in the meantime.
type TenantUsage struct {
	ProjectID     string
	ServerHours   float64
	VCPUHours     float64
	MemoryMBHours float64
	LocalGBHours  float64
}

// TenantUsages returns Nova's usage of every project with servers in
// [start, end). Totals only: the per-server breakdown would multiply the
// response by the number of servers, and the totals are what is stored.
func (c *OpenStackClient) TenantUsages(start, end time.Time) ([]TenantUsage, error) {
	var out []TenantUsage
	err := usage.AllTenants(c.computeSvc(), usage.AllTenantsOpts{Start: &start, End: &end}).
		EachPage(func(page pagination.Page) (bool, error) {
			tenants, err := usage.ExtractAllTenants(page)
			if err != nil {
				return false, err
			}
			for _, t := range tenants {
				out = append(out, TenantUsage{
					ProjectID:     t.TenantID,
					ServerHours:   t.TotalHours,
					VCPUHours:     t.TotalVCPUsUsage,
					MemoryMBHours: t.TotalMemoryMBUsage,
					LocalGBHours:  t.TotalLocalGBUsage,
				})
			}
			return true, nil
		})
	if err != nil {
		return nil, fmt.Errorf("compute usage %s – %s: %w", start.Format(time.DateOnly), end.Format(time.DateOnly), err)
	}
	return out, nil
}

// PublicIPv4ByProject counts, per project, the public IPv4 addresses held right
// now: its floating IPs, and its fixed IPv4 addresses on the given public
// networks — a VM attached to one directly, a router's gateway. A floating IP's
// own port on the external network is not counted again, and DHCP ports belong
// to the network, not to a project's use of it.
func (c *OpenStackClient) PublicIPv4ByProject(publicNetworks []string) (map[string]int, error) {
	counts := map[string]int{}
	err := floatingips.List(c.networkSvc(), floatingips.ListOpts{}).EachPage(func(page pagination.Page) (bool, error) {
		list, err := floatingips.ExtractFloatingIPs(page)
		if err != nil {
			return false, err
		}
		for _, f := range list {
			counts[firstNonEmpty(f.ProjectID, f.TenantID)]++
		}
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("list floating IPs: %w", err)
	}
	for _, network := range publicNetworks {
		err := ports.List(c.networkSvc(), ports.ListOpts{NetworkID: network}).EachPage(func(page pagination.Page) (bool, error) {
			list, err := ports.ExtractPorts(page)
			if err != nil {
				return false, err
			}
			for _, p := range list {
				if p.DeviceOwner == "network:floatingip" || p.DeviceOwner == "network:dhcp" {
					continue
				}
				for _, ip := range p.FixedIPs {
					if strings.Contains(ip.IPAddress, ".") {
						counts[firstNonEmpty(p.ProjectID, p.TenantID)]++
					}
				}
			}
			return true, nil
		})
		if err != nil {
			return nil, fmt.Errorf("list ports on %s: %w", network, err)
		}
	}
	return counts, nil
}
