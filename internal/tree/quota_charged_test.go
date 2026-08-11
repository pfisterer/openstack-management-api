package tree

import (
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

var chargedResources = []string{"cores", "ram"}

func leafWith(limit, inUse common.ProjectQuota) Node {
	return Node{ID: "leaf", Limit: limit, OSInUse: inUse}
}

func TestChargedQuota_DisabledKeepsDeclaredLimit(t *testing.T) {
	leaf := leafWith(common.ProjectQuota{"cores": 1}, common.ProjectQuota{"cores": 8})
	got := chargedQuota(leaf, chargedResources, false)
	if got["cores"] != 1 {
		t.Errorf("with the switch off the declared limit must stand, got %d", got["cores"])
	}
}

// The attack from 2026-08-06: request 8 cores, fill them, shrink to 1.
func TestChargedQuota_BillsMeasuredUsageWhenHigher(t *testing.T) {
	leaf := leafWith(common.ProjectQuota{"cores": 1}, common.ProjectQuota{"cores": 8})
	got := chargedQuota(leaf, chargedResources, true)
	if got["cores"] != 8 {
		t.Errorf("shrinking below actual usage must still cost 8, got %d", got["cores"])
	}
}

func TestChargedQuota_KeepsLimitWhenUsageIsLower(t *testing.T) {
	leaf := leafWith(common.ProjectQuota{"cores": 8}, common.ProjectQuota{"cores": 2})
	got := chargedQuota(leaf, chargedResources, true)
	if got["cores"] != 8 {
		t.Errorf("a reservation costs its limit even when idle, got %d", got["cores"])
	}
}

// A resource OpenStack does not measure has NO key. Reading that as 0 would be
// the same understatement the feature exists to prevent.
func TestChargedQuota_MissingKeyIsNotZero(t *testing.T) {
	leaf := leafWith(common.ProjectQuota{"cores": 4, "ram": 8192}, common.ProjectQuota{"cores": 2})
	got := chargedQuota(leaf, chargedResources, true)
	if got["ram"] != 8192 {
		t.Errorf("unmeasured resource must keep its declared limit, got %d", got["ram"])
	}
}

// max(-1, n) would turn "no cap" into a finite number and tighten the budget.
func TestChargedQuota_UnlimitedStaysUnlimited(t *testing.T) {
	leaf := leafWith(
		common.ProjectQuota{"cores": common.UnlimitedQuota},
		common.ProjectQuota{"cores": 64},
	)
	got := chargedQuota(leaf, chargedResources, true)
	if got["cores"] != common.UnlimitedQuota {
		t.Errorf("unlimited must survive the max, got %d", got["cores"])
	}
}

// The ordering trap: max() does not distribute over addition. Two leaves, one
// over-consuming and one idle, must bill 8+8=16 — not max(9, 10)=10, which is
// what applying the max to the aggregate would produce.
func TestBuildRolledUpUsage_MaxAppliesPerLeafNotToTheSum(t *testing.T) {
	budget := "budget"
	leaves := []Node{
		{ID: "a", ParentID: &budget, Status: StatusApproved,
			Limit: common.ProjectQuota{"cores": 1}, OSInUse: common.ProjectQuota{"cores": 8}},
		{ID: "b", ParentID: &budget, Status: StatusApproved,
			Limit: common.ProjectQuota{"cores": 8}, OSInUse: common.ProjectQuota{"cores": 2}},
	}
	parentMap := map[string]*string{budget: nil}

	usage := buildRolledUpUsage(leaves, parentMap, chargedResources, true)
	total := usage[budget].Total(chargedResources)
	if total["cores"] != 16 {
		t.Errorf("expected 8+8=16 charged cores, got %d", total["cores"])
	}
}
