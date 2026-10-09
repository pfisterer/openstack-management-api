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

	changed := applyOSSyncState(&leaf, "os-1", nil)

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

	if !applyOSSyncState(&leaf, "os-new", nil) {
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

	if !applyOSSyncState(&leaf, "os-1", &osMeasurement{inUse: common.ProjectQuota{"cores": 0, "ram": 0}}) {
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
	servers, serverLimit := 1, 2
	leaf := tree.Node{OSProjectID: "os-1", OSInUse: common.ProjectQuota{"cores": 2}, OSServers: &servers, OSServerLimit: &serverLimit}

	if applyOSSyncState(&leaf, "os-1", &osMeasurement{inUse: common.ProjectQuota{"cores": 2}, servers: 1, serverLimit: 2}) {
		t.Error("nothing changed, but the node was marked for persisting")
	}
}

// The server count is part of the measurement: written with it, and a change
// in it alone is worth persisting.
func TestApplyOSSyncState_ServerCount(t *testing.T) {
	leaf := tree.Node{OSProjectID: "os-1", OSInUse: common.ProjectQuota{"cores": 2}}

	if !applyOSSyncState(&leaf, "os-1", &osMeasurement{inUse: common.ProjectQuota{"cores": 2}, servers: 3}) {
		t.Fatal("a first server count has to be persisted")
	}
	if leaf.OSServers == nil || *leaf.OSServers != 3 {
		t.Fatalf("OSServers = %v, want 3", leaf.OSServers)
	}
	if applyOSSyncState(&leaf, "os-1", nil) || *leaf.OSServers != 3 {
		t.Error("an unmeasured pass must keep the last count")
	}
}

// An imported project is measured like a managed one, from the same quota
// response that gives its limit — the table showed a dash for everything in
// use, next to a quota that was real.
func TestMeasureImport(t *testing.T) {
	resources := []common.ManagedProject{
		{ID: "cores", OSQuotaField: "cores", OSOvercommitCheck: true},
		{ID: "ram", OSQuotaField: "ram", OSOvercommitCheck: true, OSMultiplier: 1024},
		{ID: "storage", OSQuotaField: "gigabytes", OSOvercommitCheck: true},
	}
	detail := &osclient.ProjectQuotaDetail{
		Limit: osclient.QuotaSet{Cores: 200, RAM: 512000, Gigabytes: 1000, Instances: 100},
		InUse: osclient.QuotaSet{Cores: 80, RAM: 163840, Gigabytes: 0, Instances: 10},
	}
	limit := QuotaSetToProjectQuota(resources, detail.Limit)

	var leaf tree.Node
	applyOSSyncState(&leaf, "os-1", measureImport(resources, limit, detail))

	if leaf.OSInUse["cores"] != 80 || leaf.OSInUse["ram"] != 160 || leaf.OSInUse["storage"] != 0 {
		t.Errorf("in use = %v, want 80 cores, 160 GB RAM, 0 GB storage", leaf.OSInUse)
	}
	if _, measured := leaf.OSInUse["storage"]; !measured {
		t.Error("storage was measured as 0 and must not read as unmeasured")
	}
	if leaf.OSServers == nil || *leaf.OSServers != 10 || leaf.OSOvercommitted {
		t.Errorf("servers %v, overcommitted %v", leaf.OSServers, leaf.OSOvercommitted)
	}
	if leaf.OSServerLimit == nil || *leaf.OSServerLimit != 100 {
		t.Errorf("server limit %v, want 100", leaf.OSServerLimit)
	}
}

// Members come over from OpenStack with the two roles the tree knows, once
// each; a promotion adds whoever joined since the last pass.
func TestImportAndMergeMembers(t *testing.T) {
	imported := importMembers([]osclient.ProjectMemberInfo{
		{Email: "A@x", RoleName: "reader"},
		{Email: "a@x", RoleName: "admin"},
		{Email: "b@x", RoleName: "reader"},
		{Email: "owner@x", RoleName: "member"},
	})
	want := []common.AuthorizedUser{{Token: "user:a@x", OpenstackRole: "member"}, {Token: "user:b@x", OpenstackRole: "reader"}, {Token: "user:owner@x", OpenstackRole: "member"}}
	if len(imported) != len(want) {
		t.Fatalf("imported %v, want %v", imported, want)
	}
	for i := range want {
		if imported[i] != want[i] {
			t.Errorf("imported[%d] = %v, want %v", i, imported[i], want[i])
		}
	}

	leaf := []common.AuthorizedUser{{Token: "user:b@x", OpenstackRole: "member"}}
	got := mergeMembers(leaf, append(imported, common.AuthorizedUser{Token: "user:new@x", OpenstackRole: "member"}), "user:owner@x")
	tokens := map[string]string{}
	for _, u := range got {
		tokens[u.Token] = u.OpenstackRole
	}
	if len(got) != 3 || tokens["user:b@x"] != "member" || tokens["user:a@x"] != "member" || tokens["user:new@x"] != "member" {
		t.Errorf("merged %v: want b kept as set on the leaf, a and the newcomer added, the owner left out", got)
	}
}
