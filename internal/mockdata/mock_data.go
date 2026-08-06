// Package mockdata provides the development/testing seed for the tree model:
// a small university with a root budget, department budgets, a student
// auto-approve budget and example leaves in every lifecycle state.
package mockdata

import (
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/tree"
)

// Mock group tokens.
const (
	RootGroup      = "group:root_uni"
	DeptCSAdmin    = "group:dept_cs_admin"
	DeptCSFaculty  = "group:dept_cs_faculty"
	DeptBioGroup   = "group:dept_bio"
	CSStudentGroup = "group:cs-student"
)

// DefaultMockTreeState returns the seed data used for development/testing.
func DefaultMockTreeState() ([]common.Identity, []tree.Node) {
	now := time.Now().UTC()
	iso := func(t time.Time) string { return t.Format(time.RFC3339) }
	plusDays := func(days int) *string {
		v := iso(now.Add(time.Duration(days) * 24 * time.Hour))
		return &v
	}

	identities := []common.Identity{
		{
			ID:     "mock_root",
			Label:  "Mock Root Admin (root_uni)",
			Email:  "root.admin@uni.example",
			Tokens: common.TokenList{"user:root.admin@uni.example", RootGroup},
		},
		{
			ID:     "mock_cs_admin",
			Label:  "Mock CS Admin (cs-admin)",
			Email:  "admin@cs.example",
			Tokens: common.TokenList{"user:admin@cs.example", DeptCSAdmin},
		},
		{
			ID:     "mock_cs_faculty",
			Label:  "Mock Faculty (cs-faculty)",
			Email:  "faculty@cs.example",
			Tokens: common.TokenList{"user:faculty@cs.example", DeptCSFaculty},
		},
		{
			ID:     "mock_bio_faculty",
			Label:  "Mock Faculty (bio-faculty)",
			Email:  "faculty@bio.example",
			Tokens: common.TokenList{"user:faculty@bio.example", DeptBioGroup},
		},
		{
			ID:     "mock_cs_student",
			Label:  "Mock Student (cs-student)",
			Email:  "cs-student@cs.com",
			Tokens: common.TokenList{"user:cs-student@cs.com", CSStudentGroup},
		},
	}

	rootID := tree.RootNodeID
	unassignedID := tree.UnassignedNodeID
	deptCSID := "b_dept_cs"
	facultyID := "b_cs_faculty"
	studentsID := "b_cs_students"
	bioID := "b_dept_bio"

	created := func(actor string, limit common.ProjectQuota, statusTo string) []tree.HistoryEntry {
		e := tree.HistoryEntry{
			Timestamp: "2026-01-01T00:00:00Z",
			Event:     "created",
			Actor:     actor,
			StatusTo:  statusTo,
			LimitTo:   &limit,
		}
		return []tree.HistoryEntry{e}
	}

	nodes := []tree.Node{
		// ── Structural nodes (same IDs the bootstrap uses) ────────────────────
		{
			ID: rootID, Kind: tree.KindBudget, ParentID: nil, Status: tree.StatusApproved,
			Name:       "University Root",
			AdminScope: common.TokenList{RootGroup},
			// No eligible requesters: the root hands resources down by delegation
			// (a root admin creates the department budgets), it does not take
			// requests. Nothing in the model enforces that — it is a policy
			// decision, and this is what that decision looks like as data.
			Limit:     common.ProjectQuota{"cores": common.UnlimitedQuota, "ram": common.UnlimitedQuota, "storage": common.UnlimitedQuota, "gpu": common.UnlimitedQuota},
			CreatedBy: "System", CreatedAt: "2025-01-01T00:00:00Z",
		},
		{
			ID: unassignedID, Kind: tree.KindBudget, ParentID: &rootID, Status: tree.StatusApproved,
			Name:      "Unassigned OpenStack Imports",
			Limit:     common.ProjectQuota{"cores": 0, "ram": 0, "storage": 0, "gpu": 0},
			CreatedBy: "System", CreatedAt: "2025-01-01T00:00:00Z",
		},

		// ── Budgets ───────────────────────────────────────────────────────────
		{
			ID: deptCSID, Kind: tree.KindBudget, ParentID: &rootID, Status: tree.StatusApproved,
			Name:               "Computer Science Dept",
			AdminScope:         common.TokenList{DeptCSAdmin},
			EligibleRequesters: common.TokenList{DeptCSFaculty, "user:faculty@cs.example", "user:admin@cs.example"},
			Limit:              common.ProjectQuota{"cores": 30, "ram": 100, "storage": 600, "gpu": 4},
			CreatedBy:          "root.admin@uni.example", CreatedAt: "2025-06-15T10:30:00Z",
			TerminationDate: plusDays(365),
		},
		{
			ID: facultyID, Kind: tree.KindBudget, ParentID: &deptCSID, Status: tree.StatusApproved,
			Name:               "CS Faculty Pool",
			AdminScope:         common.TokenList{DeptCSFaculty},
			EligibleRequesters: common.TokenList{CSStudentGroup, "user:cs-student@cs.com", "user:faculty@cs.example"},
			Limit:              common.ProjectQuota{"cores": 20, "ram": 64, "storage": 400, "gpu": 2},
			CreatedBy:          "admin@cs.example", CreatedAt: "2025-08-01T09:00:00Z",
			TerminationDate: plusDays(365),
		},
		{
			// Student auto-approve: managed by FACULTY (not by the students —
			// consumers deliberately hold no admin scope), consumable by students
			// up to 2 cores each, capped at 10 cores overall.
			ID: studentsID, Kind: tree.KindBudget, ParentID: &facultyID, Status: tree.StatusApproved,
			Name:               "CS Students (Small VM)",
			AdminScope:         common.TokenList{DeptCSFaculty},
			EligibleRequesters: common.TokenList{CSStudentGroup},
			AutoApprove:        &tree.AutoApprove{PerRequesterLimit: common.ProjectQuota{"cores": 2, "ram": 4, "storage": 20, "gpu": 0}},
			Limit:              common.ProjectQuota{"cores": 10, "ram": 20, "storage": 100, "gpu": 0},
			CreatedBy:          "faculty@cs.example", CreatedAt: "2025-09-01T09:00:00Z",
		},
		{
			ID: bioID, Kind: tree.KindBudget, ParentID: &rootID, Status: tree.StatusApproved,
			Name:               "Biology Dept",
			AdminScope:         common.TokenList{DeptBioGroup},
			EligibleRequesters: common.TokenList{DeptBioGroup, "user:faculty@bio.example"},
			Limit:              common.ProjectQuota{"cores": 300, "ram": 1000, "storage": 3000, "gpu": 20},
			CreatedBy:          "root.admin@uni.example", CreatedAt: "2025-07-20T14:15:00Z",
		},
		{
			// A pending BUDGET request, awaiting a decision by the CS dept admins.
			// (The old model had to fake this as a project request.)
			//
			// It hangs under the CS dept, not under root: faculty is listed in the
			// dept's eligible requesters, while the root takes no requests at all —
			// so this is a request that could actually have been made. The
			// requester holds the admin scope of what they asked for, which is what
			// the request dialog fills in.
			ID: "b_cs_expansion", Kind: tree.KindBudget, ParentID: &deptCSID, Status: tree.StatusPending,
			Name:       "Robotics Lab WS26",
			Reason:     "A dedicated budget for the robotics lab course next semester",
			AdminScope: common.TokenList{"user:faculty@cs.example"},
			Limit:      common.ProjectQuota{"cores": 8, "ram": 32, "storage": 200, "gpu": 1},
			CreatedBy:  "faculty@cs.example", CreatedAt: "2026-03-01T10:00:00Z",
			TerminationDate: plusDays(365),
			History:         created("faculty@cs.example", common.ProjectQuota{"cores": 8, "ram": 32, "storage": 200, "gpu": 1}, tree.StatusPending),
		},

		// ── Project leaves ────────────────────────────────────────────────────
		{
			ID: "p_001", Kind: tree.KindProject, ParentID: &facultyID, Status: tree.StatusApproved,
			Name:   "Faculty research sandbox",
			Reason: "Faculty research sandbox",
			Owner:  "user:faculty@cs.example",
			Limit:  common.ProjectQuota{"cores": 4, "ram": 16, "storage": 100, "gpu": 0},
			AuthorizedUsers: []common.AuthorizedUser{
				{Token: DeptCSFaculty, OpenstackRole: "member"},
				// A read-only participant: the student may look at the project in
				// OpenStack but cannot change anything. Covers the "reader" role in
				// the member sync and gives the UI a project with mixed roles.
				{Token: "user:cs-student@cs.com", OpenstackRole: "reader"},
			},
			TerminationDate: plusDays(90),
			CreatedBy:       "faculty@cs.example", CreatedAt: "2026-01-20T10:00:00Z",
			History: created("faculty@cs.example", common.ProjectQuota{"cores": 4, "ram": 16, "storage": 100, "gpu": 0}, tree.StatusPending),
		},
		{
			// Exceeds the student per-requester limit → stayed pending for a
			// faculty decision.
			ID: "p_002", Kind: tree.KindProject, ParentID: &facultyID, Status: tree.StatusPending,
			Name:   "Student course project",
			Reason: "Student course project needs compute (exceeds auto-approve limit)",
			Owner:  "user:cs-student@cs.com",
			Limit:  common.ProjectQuota{"cores": 2, "ram": 8, "storage": 50, "gpu": 0},
			AuthorizedUsers: []common.AuthorizedUser{
				{Token: "user:cs-student@cs.com", OpenstackRole: "member"},
			},
			TerminationDate: plusDays(30),
			CreatedBy:       "cs-student@cs.com", CreatedAt: "2026-01-23T08:00:00Z",
			History: created("cs-student@cs.com", common.ProjectQuota{"cores": 2, "ram": 8, "storage": 50, "gpu": 0}, tree.StatusPending),
		},
		{
			ID: "p_003", Kind: tree.KindProject, ParentID: &facultyID, Status: tree.StatusChangePending,
			Name:   "Faculty ML workload",
			Reason: "Expanded faculty ML workload",
			Owner:  "user:faculty@cs.example",
			Limit:  common.ProjectQuota{"cores": 8, "ram": 32, "storage": 200, "gpu": 0},
			Pending: &tree.PendingChanges{
				Limit:           &common.ProjectQuota{"cores": 12, "ram": 48, "storage": 300, "gpu": 0},
				TerminationDate: plusDays(180),
				AuthorizedUsers: &[]common.AuthorizedUser{
					{Token: DeptCSFaculty, OpenstackRole: "member"},
					{Token: "user:newuser@cs.example", OpenstackRole: "reader"},
				},
			},
			AuthorizedUsers: []common.AuthorizedUser{
				{Token: DeptCSFaculty, OpenstackRole: "member"},
			},
			TerminationDate: plusDays(60),
			CreatedBy:       "faculty@cs.example", CreatedAt: "2026-01-15T10:00:00Z",
			History: created("faculty@cs.example", common.ProjectQuota{"cores": 8, "ram": 32, "storage": 200, "gpu": 0}, tree.StatusPending),
		},
		{
			ID: "p_004", Kind: tree.KindProject, ParentID: &bioID, Status: tree.StatusApproved,
			Name:   "Genomics pipeline cluster",
			Reason: "Genomics pipeline cluster",
			Owner:  "user:faculty@bio.example",
			Limit:  common.ProjectQuota{"cores": 16, "ram": 64, "storage": 800, "gpu": 8},
			AuthorizedUsers: []common.AuthorizedUser{
				{Token: DeptBioGroup, OpenstackRole: "member"},
			},
			TerminationDate: plusDays(180),
			CreatedBy:       "faculty@bio.example", CreatedAt: "2026-02-01T09:00:00Z",
			History: created("faculty@bio.example", common.ProjectQuota{"cores": 16, "ram": 64, "storage": 800, "gpu": 8}, tree.StatusApproved),
		},
		{
			// Imported by the reconciler: an OpenStack project unknown to the
			// system, parked under "unassigned" until a root admin promotes it.
			ID: "p_imported_001", Kind: tree.KindProject, ParentID: &unassignedID, Status: tree.StatusImported,
			Name:          "legacy-ml-workload",
			Reason:        "OpenStack project: legacy-ml-workload (os-project-abc-123)",
			OSProjectID:   "os-project-abc-123",
			OSProjectName: "legacy-ml-workload",
			Limit:         common.ProjectQuota{"cores": 9, "ram": 16, "storage": 100, "gpu": 0},
			AuthorizedUsers: []common.AuthorizedUser{
				{Token: "user:faculty@cs.example", OpenstackRole: "member"},
				{Token: DeptCSFaculty, OpenstackRole: "member"},
			},
			ExternalGroupAssignments: []common.ExternalGroupAssignment{
				{GroupID: "os-group-ldap-001", GroupName: "legacy-ldap-researchers", Role: "member"},
			},
			CreatedBy: "System", CreatedAt: "2026-02-15T00:00:00Z",
		},
	}

	return identities, nodes
}
