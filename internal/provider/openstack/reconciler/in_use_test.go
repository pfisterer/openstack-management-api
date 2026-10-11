package reconciler

import (
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/tree"
	osclient "github.com/pfisterer/openstack-management-api/internal/provider/openstack/client"
)

// ProjectInUse is what makes an overcommit visible instead of merely flagged:
// a project shrunk below what it actually runs must still report the real
// figure, or the platform books the difference as free capacity.
func TestProjectInUse_ReportsRealConsumption(t *testing.T) {
	resources := []osclient.ManagedResource{
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
	if got := ProjectInUse([]osclient.ManagedResource{{ID: "cores", OSQuotaField: "cores", OSOvercommitCheck: true}}, nil); got != nil {
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

// An imported project is measured like a managed one, from the same quota
// response that gives its limit — the table showed a dash for everything in
// use, next to a quota that was real.
func TestMeasureImport(t *testing.T) {
	resources := []osclient.ManagedResource{
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
	tree.ApplyObservation(&leaf, "os-1", measureImport(resources, limit, detail))

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
		{Email: "svc-backup", RoleName: "member"},
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
	got := tree.MergeMembers(leaf, append(imported, common.AuthorizedUser{Token: "user:new@x", OpenstackRole: "member"}), "user:owner@x")
	tokens := map[string]string{}
	for _, u := range got {
		tokens[u.Token] = u.OpenstackRole
	}
	if len(got) != 3 || tokens["user:b@x"] != "member" || tokens["user:a@x"] != "member" || tokens["user:new@x"] != "member" {
		t.Errorf("merged %v: want b kept as set on the leaf, a and the newcomer added, the owner left out", got)
	}
}

// Only the groups this service manages become members; every other group is
// kept as an external assignment, so it cannot block adopting the project.
func TestImportGroups(t *testing.T) {
	members, external := importGroups("managed-", []importedGroup{
		{id: "g1", name: "managed-wwi23seb--dozent", role: "admin"},
		{id: "g2", name: "fakultaet-wi", role: "admin"},
		{id: "g2", name: "fakultaet-wi", role: "reader"},
		{id: "g3", name: "", role: "reader"},
	})
	if len(members) != 1 || members[0].Token != "group:wwi23seb#dozent" || members[0].OpenstackRole != "member" {
		t.Errorf("members = %v, want the managed group as member", members)
	}
	if len(external) != 2 || external[0].GroupID != "g2" || external[0].GroupName != "fakultaet-wi" || external[0].Role != "member" ||
		external[1].GroupID != "g3" || external[1].Role != "reader" {
		t.Errorf("external = %v, want fakultaet-wi once as member and the unresolved group as reader", external)
	}
}
