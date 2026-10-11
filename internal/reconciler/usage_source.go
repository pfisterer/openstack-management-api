package reconciler

import (
	"time"

	osclient "github.com/pfisterer/openstack-management-api/internal/openstack/client"
	"github.com/pfisterer/openstack-management-api/internal/usage"
)

// UsageSource answers the usage collector from OpenStack: Nova's accounting
// per project, and the public IPv4 addresses held now. The collector books what
// it reports and knows nothing about Nova.
type UsageSource struct {
	client *osclient.OpenStackClient
	// publicNetworks are the networks whose addresses count as public IPv4
	// besides floating IPs — a network VMs attach to directly.
	publicNetworks []string
}

func NewUsageSource(client *osclient.OpenStackClient, publicNetworks []string) *UsageSource {
	return &UsageSource{client: client, publicNetworks: publicNetworks}
}

// DayUsage is Nova's usage of every project with servers in [start, end).
func (s *UsageSource) DayUsage(start, end time.Time) (map[string]usage.Measured, error) {
	tenants, err := s.client.TenantUsages(start, end)
	if err != nil {
		return nil, err
	}
	return measuredFrom(tenants), nil
}

// PublicIPv4 counts each project's floating IPs and addresses on the public
// networks.
func (s *UsageSource) PublicIPv4() (map[string]int, error) {
	return s.client.PublicIPv4ByProject(s.publicNetworks)
}

// measuredFrom turns Nova's per-project totals into the collector's figures;
// Nova counts memory in MB-hours, the books in GB-hours.
func measuredFrom(tenants []osclient.TenantUsage) map[string]usage.Measured {
	out := make(map[string]usage.Measured, len(tenants))
	for _, u := range tenants {
		out[u.ProjectID] = usage.Measured{
			ServerHours: u.ServerHours,
			VCPUHours:   u.VCPUHours,
			RAMGBHours:  u.MemoryMBHours / 1024,
			DiskGBHours: u.LocalGBHours,
		}
	}
	return out
}
