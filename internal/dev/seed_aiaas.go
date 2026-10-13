package dev

import (
	"maps"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
)

// Ids of the seed's nodes for the AI service.
const (
	AIBudgetID       = "b_ai_basic"
	AICourseBudgetID = "b_ai_course_ml"
)

// The AI service's resource ids the seed uses (the staging catalogue's); one the catalogue does not have is left out.
const (
	resVCPUHours = "ws-vcpu-hours"
	resRAMHours  = "ws-ram-gb-hours"
	resGPUHours  = "ws-gpu-hours-l4-3q"
	resGPUs      = "ws-gpus-l4-3q"
	resLLM       = "llm-mtokens"
	resRPM       = "llm-rpm"
	resTPM       = "llm-tpm"
	resAllModels = "llm-model-all-proxy-models"
	resStorage   = "ws-storage-gb"
)

// WithProviderBudget gives the seed the AI service as a second provider, when the catalogue has its resources: the root holds them without a cap, the CS department and its faculty pool get shares, and there are budgets and projects of every kind worth trying —
//   - "AI basic supply" under the root: any CS person gets a personal AI project on the spot (the start page's case);
//   - "ML course WWI23SEB" under the faculty pool: the course's lecturers manage it, its students request from it;
//   - the course project, shared by the lecturer with the course's students (group members): one LLM pool for all of them;
//   - a student's personal AI project, a research request waiting for a decision, a change waiting for a decision, and a project from before the split that holds cloud and AI resources at once.
//
// Without such resources in the catalogue the seed stays as it is.
func WithProviderBudget(nodes []model.Node, resources []common.Resource) []model.Node {
	known := map[string]bool{}
	switches := map[string]bool{}
	for _, r := range resources {
		if r.Provider != "" && r.Provider != "openstack" {
			known[r.ID] = true
			switches[r.ID] = r.IsBool()
		}
	}
	if len(known) == 0 {
		return nodes
	}
	// q keeps the amounts of resources the catalogue has.
	q := func(amounts common.ProjectQuota) common.ProjectQuota {
		out := common.ProjectQuota{}
		for id, v := range amounts {
			if known[id] {
				out[id] = v
			}
		}
		return out
	}
	// with adds AI amounts to a node's own limit.
	with := func(base, add common.ProjectQuota) common.ProjectQuota {
		out := common.ProjectQuota{}
		maps.Copy(out, base)
		maps.Copy(out, q(add))
		return out
	}
	days := func(n int) *string {
		v := time.Now().UTC().Add(time.Duration(n) * 24 * time.Hour).Format(time.RFC3339)
		return &v
	}
	created := func(actor string, limit common.ProjectQuota, status string) []model.HistoryEntry {
		return []model.HistoryEntry{{Timestamp: "2026-10-01T09:00:00Z", Event: "created", Actor: actor, StatusTo: status, LimitTo: &limit}}
	}

	unlimited := common.ProjectQuota{}
	for id := range known {
		unlimited[id] = common.UnlimitedQuota
		if switches[id] {
			unlimited[id] = 1 // an availability is granted or not
		}
	}
	for i := range nodes {
		switch nodes[i].ID {
		case model.RootNodeID:
			nodes[i].Limit = with(nodes[i].Limit, unlimited)
		case "b_dept_cs":
			nodes[i].Limit = with(nodes[i].Limit, common.ProjectQuota{resVCPUHours: 20000, resRAMHours: 80000, resStorage: 2000, resGPUHours: 1000, resGPUs: 4, resLLM: 3000, resRPM: 3000, resTPM: 10000000, resAllModels: 1})
		case "b_cs_faculty":
			nodes[i].Limit = with(nodes[i].Limit, common.ProjectQuota{resVCPUHours: 12000, resRAMHours: 48000, resStorage: 1000, resGPUHours: 800, resGPUs: 4, resLLM: 1500, resRPM: 1500, resTPM: 5000000, resAllModels: 1})
		}
	}

	rootID := model.RootNodeID
	facultyID := "b_cs_faculty"
	basicID := AIBudgetID
	courseID := AICourseBudgetID

	// Rates per minute for the whole project; extra model groups only where the work needs them (research gets every model directly).
	courseLimit := q(common.ProjectQuota{resVCPUHours: 3000, resRAMHours: 12000, resStorage: 200, resGPUHours: 200, resGPUs: 2, resLLM: 300, resRPM: 600, resTPM: 2000000})
	personalLimit := q(common.ProjectQuota{resVCPUHours: 100, resRAMHours: 400, resStorage: 5, resLLM: 20, resRPM: 30, resTPM: 100000})
	researchLimit := q(common.ProjectQuota{resVCPUHours: 2000, resRAMHours: 8000, resStorage: 100, resGPUHours: 400, resGPUs: 2, resLLM: 200, resRPM: 300, resTPM: 1000000, resAllModels: 1})
	thesisLimit := q(common.ProjectQuota{resVCPUHours: 200, resRAMHours: 800, resStorage: 20, resGPUHours: 20, resLLM: 30, resRPM: 60, resTPM: 200000})
	mixedLimit := with(common.ProjectQuota{"cores": 2, "ram": 4, "storage": 20}, common.ProjectQuota{resLLM: 50, resRPM: 60, resTPM: 200000})

	return append(nodes,
		// ── Budgets ───────────────────────────────────────────────────────────
		model.Node{
			ID: basicID, Kind: model.KindBudget, ParentID: &rootID, Status: model.StatusApproved,
			Name:               "AI basic supply",
			Reason:             "Personal AI projects for everybody at CS: workspaces and LLM tokens, approved on the spot",
			AdminScope:         common.TokenList{RootGroup},
			EligibleRequesters: common.TokenList{DeptCSFaculty, DeptCSAdmin, CSStudentGroup, CourseGroup},
			AutoApprove:        &model.AutoApprove{PerRequesterLimit: personalLimit},
			Limit:              q(common.ProjectQuota{resVCPUHours: 20000, resRAMHours: 80000, resStorage: 1000, resLLM: 4000, resRPM: 3000, resTPM: 10000000}),
			CreatedBy:          "root.admin@uni.example", CreatedAt: "2026-10-01T09:00:00Z",
		},
		model.Node{
			ID: courseID, Kind: model.KindBudget, ParentID: &facultyID, Status: model.StatusApproved,
			Name:               "ML course WWI23SEB",
			Reason:             "Machine learning lab of the course WWI23SEB, winter term",
			AdminScope:         common.TokenList{CourseDozentGroup},
			EligibleRequesters: common.TokenList{CourseGroup},
			AutoApprove:        &model.AutoApprove{PerRequesterLimit: thesisLimit},
			Limit:              q(common.ProjectQuota{resVCPUHours: 5000, resRAMHours: 20000, resGPUHours: 300, resGPUs: 2, resLLM: 500, resRPM: 1000, resTPM: 3000000}),
			CreatedBy:          "faculty@cs.example", CreatedAt: "2026-10-01T09:00:00Z",
			TerminationDate: days(150),
		},

		// ── Projects ──────────────────────────────────────────────────────────
		model.Node{
			// Shared: the lecturer's project, used by every student of the course through the group, and one pool of LLM tokens for all of them.
			ID: "p_ai_course", Kind: model.KindProject, ParentID: &courseID, Status: model.StatusApproved,
			Name:   "ML exercises WWI23SEB",
			Reason: "Exercises of the machine learning lab: notebooks with GPU and the course's LLM tokens",
			Owner:  "user:faculty@cs.example",
			Limit:  courseLimit,
			AuthorizedUsers: []common.AuthorizedUser{
				{Token: CourseStudierendeGroup, OpenstackRole: "member"},
				{Token: "user:cs-student@cs.com", OpenstackRole: "member"},
			},
			TerminationDate: days(120),
			CreatedBy:       "faculty@cs.example", CreatedAt: "2026-10-01T10:00:00Z",
			History: created("faculty@cs.example", courseLimit, model.StatusApproved),
		},
		model.Node{
			// The start page's result: a personal project from the basic supply.
			ID: "p_ai_personal", Kind: model.KindProject, ParentID: &basicID, Status: model.StatusApproved,
			Name:            "AI basic supply (personal)",
			Reason:          "Personal project",
			Owner:           "user:cs-student@cs.com",
			Limit:           personalLimit,
			TerminationDate: days(180),
			CreatedBy:       "cs-student@cs.com", CreatedAt: "2026-10-02T08:00:00Z",
			History: created("cs-student@cs.com", personalLimit, model.StatusApproved),
		},
		model.Node{
			// More than the faculty pool hands out on its own: waits for the faculty.
			ID: "p_ai_research", Kind: model.KindProject, ParentID: &facultyID, Status: model.StatusPending,
			Name:   "LLM fine-tuning research",
			Reason: "Fine-tuning small models on lecture material; needs GPU hours and tokens for evaluation",
			Owner:  "user:admin@cs.example",
			Limit:  researchLimit,
			AuthorizedUsers: []common.AuthorizedUser{
				{Token: "user:faculty@cs.example", OpenstackRole: "member"},
			},
			TerminationDate: days(200),
			CreatedBy:       "admin@cs.example", CreatedAt: "2026-10-05T14:00:00Z",
			History: created("admin@cs.example", researchLimit, model.StatusPending),
		},
		model.Node{
			// Approved, with a request for more tokens and another member waiting for the course's lecturers.
			ID: "p_ai_thesis", Kind: model.KindProject, ParentID: &courseID, Status: model.StatusChangePending,
			Name:   "Bachelor thesis RAG",
			Reason: "Retrieval-augmented generation over the module handbook",
			Owner:  "user:cs-student@cs.com",
			Limit:  thesisLimit,
			Pending: &model.PendingChanges{
				Limit: func() *common.ProjectQuota { l := with(thesisLimit, common.ProjectQuota{resLLM: 80}); return &l }(),
				AuthorizedUsers: &[]common.AuthorizedUser{
					{Token: "user:faculty@cs.example", OpenstackRole: "member"},
				},
			},
			TerminationDate: days(90),
			CreatedBy:       "cs-student@cs.com", CreatedAt: "2026-10-03T11:00:00Z",
			History: created("cs-student@cs.com", thesisLimit, model.StatusApproved),
		},
		model.Node{
			// From before cloud and AI projects were told apart: holds both, shows both.
			ID: "p_mixed", Kind: model.KindProject, ParentID: &facultyID, Status: model.StatusApproved,
			Name:   "Chatbot demo (VM and LLM)",
			Reason: "A demo VM that calls the LLM; from before projects had a kind",
			Owner:  "user:faculty@cs.example",
			Limit:  mixedLimit,
			AuthorizedUsers: []common.AuthorizedUser{
				{Token: "user:cs-student@cs.com", OpenstackRole: "reader"},
			},
			TerminationDate: days(60),
			CreatedBy:       "faculty@cs.example", CreatedAt: "2026-09-20T09:00:00Z",
			History: created("faculty@cs.example", mixedLimit, model.StatusApproved),
		},
	)
}
