package tree

import (
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

// The sentinel is the whole point of this helper: -1 sorts below every real
// value numerically but means "no cap", so both unlimited cases are checked here
// rather than through the service — a leaf may never carry -1, and a budget never
// takes the no-approval path, so neither is reachable from RequestChange.
func TestQuotaNeverGrows(t *testing.T) {
	resourceIDs := []string{"cores", "ram"}
	unlimited := common.UnlimitedQuota

	cases := []struct {
		name string
		from common.ProjectQuota
		to   common.ProjectQuota
		want bool
	}{
		{"unchanged", common.ProjectQuota{"cores": 4, "ram": 8}, common.ProjectQuota{"cores": 4, "ram": 8}, true},
		{"every resource shrinks", common.ProjectQuota{"cores": 4, "ram": 8}, common.ProjectQuota{"cores": 2, "ram": 4}, true},
		{"one resource grows", common.ProjectQuota{"cores": 4, "ram": 8}, common.ProjectQuota{"cores": 2, "ram": 16}, false},
		{"unlimited to concrete is a reduction", common.ProjectQuota{"cores": unlimited, "ram": 8}, common.ProjectQuota{"cores": 999, "ram": 8}, true},
		{"concrete to unlimited is an increase", common.ProjectQuota{"cores": 4, "ram": 8}, common.ProjectQuota{"cores": unlimited, "ram": 8}, false},
		{"unlimited stays unlimited", common.ProjectQuota{"cores": unlimited, "ram": 8}, common.ProjectQuota{"cores": unlimited, "ram": 8}, true},
		{"a missing key counts as zero", common.ProjectQuota{"cores": 4, "ram": 8}, common.ProjectQuota{"cores": 4}, true},
		{"a resource appearing from nothing grows", common.ProjectQuota{"cores": 4}, common.ProjectQuota{"cores": 4, "ram": 1}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := quotaNeverGrows(tc.from, tc.to, resourceIDs); got != tc.want {
				t.Errorf("quotaNeverGrows(%v, %v) = %v, want %v", tc.from, tc.to, got, tc.want)
			}
		})
	}
}
