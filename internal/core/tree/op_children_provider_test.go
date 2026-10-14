package tree

import (
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
)

// The kinds a project is, by what it holds or asks for.
func TestProjectProviders(t *testing.T) {
	resources := []common.Resource{{ID: "cores", Provider: "openstack"}, {ID: "llm-mtokens", Provider: "aiaas"}, {ID: "ws-storage-gb", Provider: "aiaas"}}
	pending := common.ProjectQuota{"llm-mtokens": 5}
	for name, c := range map[string]struct {
		node model.Node
		want []string
	}{
		"cloud":       {model.Node{Limit: common.ProjectQuota{"cores": 4, "llm-mtokens": 0}}, []string{"openstack"}},
		"lab":         {model.Node{Limit: common.ProjectQuota{"ws-storage-gb": common.UnlimitedQuota}}, []string{"aiaas"}},
		"both":        {model.Node{Limit: common.ProjectQuota{"cores": 2, "llm-mtokens": 1}}, []string{"openstack", "aiaas"}},
		"asking":      {model.Node{Limit: common.ProjectQuota{"cores": 2}, Pending: &model.PendingChanges{Limit: &pending}}, []string{"openstack", "aiaas"}},
		"nothing yet": {model.Node{}, nil},
	} {
		got := projectProviders(c.node, resources)
		if len(got) != len(c.want) {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
		for _, p := range c.want {
			if !got[p] {
				t.Errorf("%s: %v lacks %s", name, got, p)
			}
		}
	}
}
