package tree

import (
	"slices"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

func ids(nodes []Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.ID)
	}
	return out
}

// Resources sort as numbers: "10" after "9", unlimited above every amount, and
// a project not measured yet below every measured one, never among the idle.
func TestSortChildren_ByResource(t *testing.T) {
	one, three := 1, 3
	nodes := []Node{
		{ID: "a", Limit: common.ProjectQuota{"cores": 10}, OSInUse: common.ProjectQuota{"cores": 0}, OSServers: &one},
		{ID: "b", Limit: common.ProjectQuota{"cores": 9}, OSInUse: common.ProjectQuota{"cores": 4}, OSServers: &three},
		{ID: "c", Limit: common.ProjectQuota{"cores": common.UnlimitedQuota}},
		{ID: "d", Limit: common.ProjectQuota{"cores": 2},
			Allocations: []Allocation{{BudgetID: "x", Limit: common.ProjectQuota{"cores": 16}}}},
	}
	cases := []struct {
		key  string
		desc bool
		want []string
	}{
		{"reserved:cores", false, []string{"b", "a", "d", "c"}},
		{"reserved:cores", true, []string{"c", "d", "a", "b"}},
		{"used:cores", true, []string{"b", "a", "c", "d"}},
		{"servers", true, []string{"b", "a", "c", "d"}},
	}
	for _, tc := range cases {
		got := slices.Clone(nodes)
		sortChildren(got, tc.key, tc.desc)
		if !slices.Equal(ids(got), tc.want) {
			t.Errorf("%s desc=%v: %v, want %v", tc.key, tc.desc, ids(got), tc.want)
		}
	}
	for _, key := range []string{"reserved:", "used:", "cores", "servers:x"} {
		if err := (ChildFilter{Sort: key}).Validate(); err == nil {
			t.Errorf("sort %q accepted", key)
		}
	}
	if err := (ChildFilter{Sort: "used:ram"}).Validate(); err != nil {
		t.Errorf("used:ram refused: %v", err)
	}
}
