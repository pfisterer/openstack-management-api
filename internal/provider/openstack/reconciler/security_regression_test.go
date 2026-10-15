package reconciler

import (
	"math"
	"testing"

	"github.com/gophercloud/gophercloud/openstack/identity/v3/groups"
	"github.com/pfisterer/openstack-management-api/internal/common"
	osclient "github.com/pfisterer/openstack-management-api/internal/provider/openstack/client"
)

// A value times its unit must fit OpenStack's quota field: an overflowing product wrapped negative, which OpenStack reads as unlimited.
func TestProjectQuotaToQuotaSet_Clamps(t *testing.T) {
	resources := []osclient.ManagedResource{
		{ID: "cores", OSQuotaField: "cores"},
		{ID: "ram", OSQuotaField: "ram", OSMultiplier: 1024},
	}
	for _, v := range []int{math.MaxInt, math.MaxInt / 1024, common.MaxQuotaValue} {
		qs := ProjectQuotaToQuotaSet(resources, common.ProjectQuota{"cores": v, "ram": v})
		if qs.Cores < 0 || qs.Cores > math.MaxInt32 || qs.RAM < 0 || qs.RAM > math.MaxInt32 {
			t.Fatalf("%d mapped to cores %d, ram %d", v, qs.Cores, qs.RAM)
		}
	}
	qs := ProjectQuotaToQuotaSet(resources, common.ProjectQuota{"cores": 8, "ram": 16})
	if qs.Cores != 8 || qs.RAM != 16*1024 {
		t.Fatalf("ordinary values: cores %d, ram %d", qs.Cores, qs.RAM)
	}
	if qs := ProjectQuotaToQuotaSet(resources, common.ProjectQuota{"cores": -5, "ram": common.UnlimitedQuota}); qs.Cores != 0 || qs.RAM != common.UnlimitedQuota {
		t.Fatalf("negative values: cores %d, ram %d", qs.Cores, qs.RAM)
	}
}

// Without a prefix the groups of this service would be named like any other Keystone group.
func TestValidateGroupPrefix(t *testing.T) {
	for _, p := range []string{"", "  "} {
		if ValidateGroupPrefix(p) == nil {
			t.Errorf("prefix %q accepted", p)
		}
	}
	if err := ValidateGroupPrefix("managed-"); err != nil {
		t.Errorf("managed-: %v", err)
	}
}

// Only a group this service created is managed; a foreign one of the same name is left alone.
func TestIsManagedGroup(t *testing.T) {
	if !isManagedGroup(&groups.Group{Name: "managed-x", Description: managedGroupDescription}) {
		t.Error("our own group is not recognised")
	}
	for _, g := range []*groups.Group{nil, {Name: "managed-x"}, {Name: "managed-x", Description: "Cloud operators"}} {
		if isManagedGroup(g) {
			t.Errorf("foreign group %+v counted as ours", g)
		}
	}
}
