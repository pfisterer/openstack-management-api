package tree

import (
	"math"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

// A sum that would overflow is refused where it decides something and saturates where it is only shown — it never wraps negative, which read as free capacity.
func TestQuotaAdd_NeverWraps(t *testing.T) {
	ids := []string{"cores"}
	a, b := common.ProjectQuota{"cores": math.MaxInt - 1}, common.ProjectQuota{"cores": 2}
	if _, err := quotaAddChecked(a, b, ids); err == nil {
		t.Fatal("an overflowing sum was accepted")
	}
	if got := quotaAdd(a, b, ids)["cores"]; got != math.MaxInt {
		t.Fatalf("a shown sum wrapped to %d", got)
	}
	neg := common.ProjectQuota{"cores": math.MinInt + 1}
	if got := quotaAdd(neg, common.ProjectQuota{"cores": -2}, ids)["cores"]; got != math.MinInt {
		t.Fatalf("a negative sum wrapped to %d", got)
	}
	if got, err := quotaAddChecked(common.ProjectQuota{"cores": 3}, common.ProjectQuota{"cores": 4}, ids); err != nil || got["cores"] != 7 {
		t.Fatalf("an ordinary sum: %v %v", got, err)
	}
}
