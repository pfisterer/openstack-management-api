package aiaas

import (
	"encoding/json"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/nodestore"
)

// The values the AI service is told about a project's release times, local storage, batch priority and batch jobs, for chains of budgets. Written before the four settings became caps of the catalogue (docs/plan-caps-katalog.md, 10.11.2026) with the then hand-written model, and kept unchanged since: the wire to the AI service must not move. Only budget() knows how a setting is stored on a node.

// set is what one budget sets of its own; nil/""/0 = nothing.
type set struct {
	cpuOpen, cpuClosed, cpuMax *int
	gpuOpen, gpuClosed, gpuMax *int
	scratch                    *int
	prio                       string
	jobs, jobHours             int
	inherits                   bool
}

func n(v int) *int { return &v }

func wireOf(chain []model.Node) string {
	b, _ := json.Marshal(struct {
		Release  *Release
		Scratch  int
		Priority string
		Jobs     *BatchJobs
	}{ReleaseFor(chain), ScratchFor(chain), BatchPriorityFor(chain), BatchJobsFor(chain)})
	return string(b)
}

func TestCapsWireEquivalence(t *testing.T) {
	cases := []struct {
		name  string
		chain []set // parent first, root last
		want  string
	}{
		{"nothing set", []set{{}, {}}, `{"Release":{"cpu":{},"gpu":{}},"Scratch":0,"Priority":"","Jobs":{"jobs_per_person":0,"max_hours":0}}`},
		{"root only", []set{{cpuOpen: n(60), cpuClosed: n(10), cpuMax: n(24), gpuOpen: n(30), gpuClosed: n(5), gpuMax: n(0), scratch: n(20), prio: "guaranteed", jobs: 3, jobHours: 12}}, `{"Release":{"cpu":{"idle_open_minutes":60,"idle_closed_minutes":10,"max_hours":24},"gpu":{"idle_open_minutes":30,"idle_closed_minutes":5,"max_hours":0}},"Scratch":20,"Priority":"guaranteed","Jobs":{"jobs_per_person":3,"max_hours":12}}`},
		{"stricter below wins", []set{{cpuOpen: n(30), gpuMax: n(8), scratch: n(5), prio: "preemptible", jobs: 1, jobHours: 4}, {cpuOpen: n(60), gpuMax: n(0), scratch: n(20), prio: "guaranteed", jobs: 3, jobHours: 12}}, `{"Release":{"cpu":{"idle_open_minutes":30},"gpu":{"max_hours":8}},"Scratch":5,"Priority":"preemptible","Jobs":{"jobs_per_person":1,"max_hours":4}}`},
		{"stricter above wins", []set{{cpuOpen: n(90), gpuMax: n(0), scratch: n(50), prio: "guaranteed", jobs: 5, jobHours: 20}, {cpuOpen: n(60), gpuMax: n(10), scratch: n(20), prio: "preemptible", jobs: 3, jobHours: 12}}, `{"Release":{"cpu":{"idle_open_minutes":60},"gpu":{"max_hours":10}},"Scratch":20,"Priority":"preemptible","Jobs":{"jobs_per_person":3,"max_hours":12}}`},
		{"mixed per field", []set{{jobs: 2, jobHours: 20}, {jobs: 3, jobHours: 12}}, `{"Release":{"cpu":{},"gpu":{}},"Scratch":0,"Priority":"","Jobs":{"jobs_per_person":2,"max_hours":12}}`},
		{"batch jobs off in between", []set{{jobs: 2, jobHours: 4}, {}, {jobs: 3, jobHours: 12}}, `{"Release":{"cpu":{},"gpu":{}},"Scratch":0,"Priority":"","Jobs":{"jobs_per_person":0,"max_hours":0}}`},
		{"inheriting budget passes batch jobs on", []set{{inherits: true}, {jobs: 3, jobHours: 12}}, `{"Release":{"cpu":{},"gpu":{}},"Scratch":0,"Priority":"","Jobs":{"jobs_per_person":3,"max_hours":12}}`},
		{"inheriting budget still sets the rest", []set{{inherits: true, scratch: n(7), cpuClosed: n(3)}, {scratch: n(9), jobs: 1, jobHours: 1}}, `{"Release":{"cpu":{"idle_closed_minutes":3},"gpu":{}},"Scratch":7,"Priority":"","Jobs":{"jobs_per_person":1,"max_hours":1}}`},
		{"unbounded below a bound", []set{{gpuMax: n(0)}, {gpuMax: n(48)}}, `{"Release":{"cpu":{},"gpu":{"max_hours":48}},"Scratch":0,"Priority":"","Jobs":{"jobs_per_person":0,"max_hours":0}}`},
		{"guaranteed only", []set{{prio: "guaranteed"}, {}}, `{"Release":{"cpu":{},"gpu":{}},"Scratch":0,"Priority":"guaranteed","Jobs":{"jobs_per_person":0,"max_hours":0}}`},
		{"empty chain", nil, `{"Release":{"cpu":{},"gpu":{}},"Scratch":0,"Priority":"","Jobs":{"jobs_per_person":0,"max_hours":0}}`},
	}
	for _, c := range cases {
		var chain []model.Node
		for _, s := range c.chain {
			chain = append(chain, budget(s))
		}
		if got := wireOf(chain); got != c.want {
			t.Errorf("%s:\n got  %s\n want %s", c.name, got, c.want)
		}
	}
}

// budget is a budget node setting s of its own, as the tree stores it: its caps.
func budget(s set) model.Node {
	c := common.Caps{}
	for id, v := range map[string]*int{CapReleaseCPUIdleOpen: s.cpuOpen, CapReleaseCPUIdleClosed: s.cpuClosed, CapReleaseCPUMaxHours: s.cpuMax, CapReleaseGPUIdleOpen: s.gpuOpen, CapReleaseGPUIdleClosed: s.gpuClosed, CapReleaseGPUMaxHours: s.gpuMax, CapWorkspaceScratchGB: s.scratch} {
		if v != nil {
			c[id] = *v
		}
	}
	switch s.prio {
	case "preemptible":
		c[CapBatchPriority] = 0
	case "guaranteed":
		c[CapBatchPriority] = 1
	}
	if s.jobs > 0 {
		c[CapBatchJobsPerPerson], c[CapBatchMaxHours] = s.jobs, s.jobHours
	}
	return model.Node{Kind: model.KindBudget, InheritsLimit: s.inherits, Caps: c.Clone()}
}

// The migration writes the ids of these caps.
func TestLegacyCapIDsAreTheCaps(t *testing.T) {
	for _, id := range nodestore.LegacyCapIDs {
		if _, ok := common.CapCatalog(Caps).Find(id); !ok {
			t.Errorf("%s is no cap", id)
		}
	}
	if len(nodestore.LegacyCapIDs) != len(Caps) {
		t.Errorf("%d legacy ids, %d caps", len(nodestore.LegacyCapIDs), len(Caps))
	}
	if err := common.ValidateCapDefs(Caps); err != nil {
		t.Error(err)
	}
}
