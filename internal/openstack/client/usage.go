package osclient

import (
	"fmt"
	"time"

	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/usage"
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
