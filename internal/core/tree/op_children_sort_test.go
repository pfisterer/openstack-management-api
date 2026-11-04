package tree

import (
	"slices"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
)

func ids(nodes []model.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.ID)
	}
	return out
}

// Resources sort as numbers: "10" after "9", unlimited above every amount, and
// a project not measured yet below every measured one, never among the idle.
func TestSortChildren_ByResource(t *testing.T) {
	nodes := []model.Node{
		{ID: "a", Limit: common.ProjectQuota{"cores": 10}, Bindings: map[string]model.Binding{"openstack": {InUse: common.ProjectQuota{"cores": 0}, Facts: map[string]int{model.FactServers: 1}}}},
		{ID: "b", Limit: common.ProjectQuota{"cores": 9}, Bindings: map[string]model.Binding{"openstack": {InUse: common.ProjectQuota{"cores": 4}, Facts: map[string]int{model.FactServers: 3}}}},
		{ID: "c", Limit: common.ProjectQuota{"cores": common.UnlimitedQuota}},
		{ID: "d", Limit: common.ProjectQuota{"cores": 2},
			Allocations: []model.Allocation{{BudgetID: "x", Limit: common.ProjectQuota{"cores": 16}}}},
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
	for _, key := range []string{"reserved:", "used:", "used:cores,", "cores", "servers:x"} {
		if err := (ChildFilter{Sort: key}).Validate(); err == nil {
			t.Errorf("sort %q accepted", key)
		}
	}
	if err := (ChildFilter{Sort: "used:ram"}).Validate(); err != nil {
		t.Errorf("used:ram refused: %v", err)
	}
}

// Several ids sort by their sum in use; a project with none of them measured stays below every measured one.
func TestSortChildren_BySumInUse(t *testing.T) {
	inUse := func(q common.ProjectQuota) map[string]model.Binding {
		return map[string]model.Binding{"aiaas": {InUse: q}}
	}
	nodes := []model.Node{
		{ID: "a", Bindings: inUse(common.ProjectQuota{"gpu-h-small": 5, "gpu-h-large": 1})},
		{ID: "b", Bindings: inUse(common.ProjectQuota{"gpu-h-large": 8})},
		{ID: "c"},
		{ID: "d", Bindings: inUse(common.ProjectQuota{"gpu-h-small": 0})},
	}
	got := slices.Clone(nodes)
	sortChildren(got, "used:gpu-h-small,gpu-h-large", true)
	if want := []string{"b", "a", "d", "c"}; !slices.Equal(ids(got), want) {
		t.Errorf("sum desc: %v, want %v", ids(got), want)
	}
	if err := (ChildFilter{Sort: "used:gpu-h-small,gpu-h-large"}).Validate(); err != nil {
		t.Errorf("a sum refused: %v", err)
	}
}
