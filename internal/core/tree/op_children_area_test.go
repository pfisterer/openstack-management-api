package tree

import (
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
)

// The areas a project is in, by what it holds or asks for; the AI service's provider serves two.
func TestProjectAreas(t *testing.T) {
	resources := []common.Resource{{ID: "cores", Provider: "openstack"}, {ID: "llm-mtokens", Provider: "aiaas", Area: common.AreaLLM}, {ID: "ws-storage-gb", Provider: "aiaas", Area: common.AreaWorkspaces}}
	pending := common.ProjectQuota{"llm-mtokens": 5}
	for name, c := range map[string]struct {
		node model.Node
		want []string
	}{
		"cloud":       {model.Node{Limit: common.ProjectQuota{"cores": 4, "llm-mtokens": 0}}, []string{"cloud"}},
		"workspaces":  {model.Node{Limit: common.ProjectQuota{"ws-storage-gb": common.UnlimitedQuota}}, []string{"workspaces"}},
		"llm":         {model.Node{Limit: common.ProjectQuota{"llm-mtokens": 1}}, []string{"llm"}},
		"both":        {model.Node{Limit: common.ProjectQuota{"cores": 2, "llm-mtokens": 1, "ws-storage-gb": 5}}, []string{"cloud", "llm", "workspaces"}},
		"asking":      {model.Node{Limit: common.ProjectQuota{"cores": 2}, Pending: &model.PendingChanges{Limit: &pending}}, []string{"cloud", "llm"}},
		"nothing yet": {model.Node{}, nil},
	} {
		got := projectAreas(c.node, resources)
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
