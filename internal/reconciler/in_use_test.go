package reconciler

import (
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	osclient "github.com/pfisterer/openstack-management-api/internal/openstack/client"
)

// ProjectInUse is what makes an overcommit visible instead of merely flagged:
// a project shrunk below what it actually runs must still report the real
// figure, or the platform books the difference as free capacity.
func TestProjectInUse_ReportsRealConsumption(t *testing.T) {
	resources := []common.ManagedProject{
		{ID: "cores", OSQuotaField: "cores", OSOvercommitCheck: true},
		// RAM is counted in GB here and in MB in OpenStack.
		{ID: "ram", OSQuotaField: "ram", OSOvercommitCheck: true, OSMultiplier: 1024},
		// Not measured: no in-use counter, so it must be absent rather than 0.
		{ID: "storage", OSQuotaField: "gigabytes", OSOvercommitCheck: false},
	}
	detail := &osclient.ProjectQuotaDetail{InUse: osclient.QuotaSet{Cores: 8, RAM: 16384, Gigabytes: 50}}

	got := ProjectInUse(resources, detail)

	if got["cores"] != 8 {
		t.Errorf("cores = %d, want 8", got["cores"])
	}
	if got["ram"] != 16 {
		t.Errorf("ram = %d, want 16 (16384 MB / 1024)", got["ram"])
	}
	if _, present := got["storage"]; present {
		t.Error("storage is not measured; reporting it would read as 'nothing used' rather than 'unknown'")
	}
}

// Without quota detail there is nothing to claim — nil, not an empty map that
// would later be indistinguishable from "measured everything as zero".
func TestProjectInUse_NilDetail(t *testing.T) {
	if got := ProjectInUse([]common.ManagedProject{{ID: "cores", OSQuotaField: "cores", OSOvercommitCheck: true}}, nil); got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

// The in-use map is persisted and compared on every pass; a missing key and a
// zero must not be conflated or the node would be rewritten in a loop.
func TestQuotaEqual_DistinguishesMissingFromZero(t *testing.T) {
	if quotaEqual(common.ProjectQuota{"cores": 0}, common.ProjectQuota{}) {
		t.Error("a measured 0 and an unmeasured resource compared equal")
	}
	if !quotaEqual(common.ProjectQuota{"cores": 2, "ram": 4}, common.ProjectQuota{"ram": 4, "cores": 2}) {
		t.Error("same values in another order compared unequal")
	}
}
