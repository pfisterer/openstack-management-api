package reconciler

import (
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	osclient "github.com/pfisterer/openstack-management-api/internal/openstack/client"
	"github.com/pfisterer/openstack-management-api/internal/tree"
)

// ProjectInUse is what makes an overcommit visible instead of merely flagged:
// a project shrunk below what it actually runs must still report the real
// figure, or the platform books the difference as free capacity.
func TestProjectInUse_ReportsRealConsumption(t *testing.T) {
	resources := []common.ManagedProject{
		{ID: "cores", OSQuotaField: "cores", OSOvercommitCheck: true},
		// RAM is counted in GB here and in MB in OpenStack.
		{ID: "ram", OSQuotaField: "ram", OSOvercommitCheck: true, OSMultiplier: 1024},
		{ID: "storage", OSQuotaField: "gigabytes", OSOvercommitCheck: true},
		// Genuinely unmeasurable: OpenStack has no quota field for GPUs, so it
		// must be absent rather than 0.
		{ID: "gpu", OSOvercommitCheck: true},
	}
	detail := &osclient.ProjectQuotaDetail{InUse: osclient.QuotaSet{Cores: 8, RAM: 16384, Gigabytes: 50}}

	got := ProjectInUse(resources, detail)

	if got["cores"] != 8 {
		t.Errorf("cores = %d, want 8", got["cores"])
	}
	if got["ram"] != 16 {
		t.Errorf("ram = %d, want 16 (16384 MB / 1024)", got["ram"])
	}
	// Counted in GB on both sides, so no multiplier is involved.
	if got["storage"] != 50 {
		t.Errorf("storage = %d, want 50", got["storage"])
	}
	if _, present := got["gpu"]; present {
		t.Error("gpu has no OpenStack quota field; reporting it would read as 'nothing used' rather than 'unknown'")
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

// A pass that could not read the quota detail must leave the last known usage
// alone. Writing an empty map there is not a cosmetic bug: the accounting bills
// max(limit, in-use), so an erased measurement drops the charge back to the
// declared limit — which is exactly the shrink-after-filling loophole that
// billing the maximum exists to close, reopened by a transient API error.
func TestApplyOSSyncState_UnmeasuredPassKeepsTheLastKnownUsage(t *testing.T) {
	leaf := tree.Node{
		OSProjectID:     "os-1",
		OSOvercommitted: true,
		OSInUse:         common.ProjectQuota{"cores": 8, "ram": 5},
	}

	changed := applyOSSyncState(&leaf, "os-1", false, nil, false)

	if changed {
		t.Error("nothing was measured and the project id is unchanged; there is nothing to persist")
	}
	if !quotaEqual(leaf.OSInUse, common.ProjectQuota{"cores": 8, "ram": 5}) {
		t.Errorf("OSInUse = %v, want the stored measurement untouched", leaf.OSInUse)
	}
	if !leaf.OSOvercommitted {
		t.Error("OSOvercommitted was cleared by a pass that did not measure anything")
	}
}

// The project id is tracked even when the quota detail was unreadable — it does
// not come from the measurement, so there is no reason to lose it.
func TestApplyOSSyncState_UnmeasuredPassStillAdoptsTheProjectID(t *testing.T) {
	leaf := tree.Node{OSInUse: common.ProjectQuota{"cores": 8}}

	if !applyOSSyncState(&leaf, "os-new", false, nil, false) {
		t.Fatal("a new OS project id has to be persisted")
	}
	if leaf.OSProjectID != "os-new" {
		t.Errorf("OSProjectID = %q, want %q", leaf.OSProjectID, "os-new")
	}
	if !quotaEqual(leaf.OSInUse, common.ProjectQuota{"cores": 8}) {
		t.Errorf("OSInUse = %v, want it untouched", leaf.OSInUse)
	}
}

// A real measurement overwrites, including back down to zero: a project that
// genuinely released its servers must stop being billed for them.
func TestApplyOSSyncState_MeasuredPassOverwrites(t *testing.T) {
	leaf := tree.Node{
		OSProjectID:     "os-1",
		OSOvercommitted: true,
		OSInUse:         common.ProjectQuota{"cores": 8, "ram": 5},
	}

	if !applyOSSyncState(&leaf, "os-1", false, common.ProjectQuota{"cores": 0, "ram": 0}, true) {
		t.Fatal("the measurement changed, so the node needs persisting")
	}
	if !quotaEqual(leaf.OSInUse, common.ProjectQuota{"cores": 0, "ram": 0}) {
		t.Errorf("OSInUse = %v, want the fresh measurement", leaf.OSInUse)
	}
	if leaf.OSOvercommitted {
		t.Error("OSOvercommitted should follow the fresh measurement")
	}
}

// An unchanged measurement must not report a change, or the reconciler rewrites
// every leaf on every tick.
func TestApplyOSSyncState_IdenticalMeasurementIsNotAChange(t *testing.T) {
	leaf := tree.Node{OSProjectID: "os-1", OSInUse: common.ProjectQuota{"cores": 2}}

	if applyOSSyncState(&leaf, "os-1", false, common.ProjectQuota{"cores": 2}, true) {
		t.Error("nothing changed, but the node was marked for persisting")
	}
}
