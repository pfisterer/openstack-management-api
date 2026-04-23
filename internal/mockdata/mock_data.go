package mockdata

import (
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

// DefaultMockResourceState returns the seed data used for development/testing.
func DefaultMockResourceState() ([]common.Identity, []common.Delegation, []common.Project, []common.TokenEligibilityRule) {
	now := time.Now().UTC()
	plusDays := func(days int) *string {
		t := now.Add(time.Duration(days) * 24 * time.Hour).Format(time.RFC3339)
		return &t
	}

	rootGroup := "group:root_uni"
	deptCSAdmin := "group:dept_cs_admin"
	deptCSFaculty := "group:dept_cs_faculty"
	deptBioGroup := "group:dept_bio"
	csStudentGroup := "group:cs-student"

	identities := []common.Identity{
		{
			ID:     "mock_root",
			Label:  "Mock Root Admin (root_uni)",
			Email:  "root.admin@uni.example",
			Tokens: common.TokenList{"user:root.admin@uni.example", rootGroup},
		},
		{
			ID:     "mock_cs_admin",
			Label:  "Mock CS Admin (cs-admin)",
			Email:  "admin@cs.example",
			Tokens: common.TokenList{"user:admin@cs.example", deptCSAdmin},
		},
		{
			ID:     "mock_cs_faculty",
			Label:  "Mock Faculty (cs-faculty)",
			Email:  "faculty@cs.example",
			Tokens: common.TokenList{"user:faculty@cs.example", deptCSFaculty},
		},
		{
			ID:     "mock_bio_faculty",
			Label:  "Mock Faculty (bio-faculty)",
			Email:  "faculty@bio.example",
			Tokens: common.TokenList{"user:faculty@bio.example", deptBioGroup},
		},
		{
			ID:     "mock_cs_student",
			Label:  "Mock Student (cs-student)",
			Email:  "cs-student@cs.com",
			Tokens: common.TokenList{"user:cs-student@cs.com", csStudentGroup},
		},
	}

	delegations := []common.Delegation{
		{
			ID:                 rootGroup,
			Name:               "University Root",
			ParentID:           nil,
			CanDelegate:        true,
			DelegationStrategy: common.DelegationStrategyPool,
			// Root admins manage this delegation.
			// CS dept and Bio dept may request resources from the root pool.
			AdminScope: common.TokenList{rootGroup},
			Quota: common.ProjectResources{
				Limit: common.ProjectQuota{"cores": common.UnlimitedQuota, "ram": common.UnlimitedQuota, "storage": common.UnlimitedQuota, "gpu": common.UnlimitedQuota},
			},
			CreatedBy: "System",
			CreatedAt: "2025-01-01T00:00:00Z",
			EndDate:   nil,
		},
		{
			ID:                 deptCSAdmin,
			Name:               "Computer Science Dept",
			ParentID:           &rootGroup,
			CanDelegate:        true,
			DelegationStrategy: common.DelegationStrategyPool,
			// CS admins manage this pool; CS admins may delegate a sub-pool to faculty.
			AdminScope: common.TokenList{deptCSAdmin},
			Quota: common.ProjectResources{
				Limit: common.ProjectQuota{"cores": 30, "ram": 100, "storage": 600, "gpu": 4},
			},
			CreatedBy: "root.admin@uni.example",
			CreatedAt: "2025-06-15T10:30:00Z",
			EndDate:   plusDays(365),
		},
		{
			ID:                 deptCSFaculty,
			Name:               "CS Faculty Pool",
			ParentID:           &deptCSAdmin,
			CanDelegate:        true,
			DelegationStrategy: common.DelegationStrategyPool,
			// Faculty manage this sub-pool delegated from CS dept; faculty may further delegate to students.
			AdminScope: common.TokenList{deptCSFaculty},
			Quota: common.ProjectResources{
				Limit: common.ProjectQuota{"cores": 20, "ram": 64, "storage": 400, "gpu": 2},
			},
			CreatedBy: "admin@cs.example",
			CreatedAt: "2025-08-01T09:00:00Z",
			EndDate:   plusDays(365),
		},
		{
			ID:                 "dept_cs_students",
			Name:               "CS Students (Small VM)",
			ParentID:           &deptCSFaculty,
			CanDelegate:        false,
			DelegationStrategy: common.DelegationStrategyAllowance,
			AdminScope:         common.TokenList{csStudentGroup},
			Quota: common.ProjectResources{
				Limit: common.ProjectQuota{"cores": 2, "ram": 4, "storage": 20, "gpu": 0},
			},
			CreatedBy: "faculty@cs.example",
			CreatedAt: "2025-09-01T09:00:00Z",
			EndDate:   nil,
		},
		{
			ID:                 deptBioGroup,
			Name:               "Biology Dept",
			ParentID:           &rootGroup,
			CanDelegate:        true,
			DelegationStrategy: common.DelegationStrategyPool,
			// Bio faculty manage and may request from this pool.
			AdminScope: common.TokenList{deptBioGroup},
			Quota: common.ProjectResources{
				Limit: common.ProjectQuota{"cores": 300, "ram": 1000, "storage": 3000, "gpu": 20},
			},
			CreatedBy: "root.admin@uni.example",
			CreatedAt: "2025-07-20T14:15:00Z",
			EndDate:   nil,
		},
	}

	fundedByFaculty := deptCSFaculty
	fundedByBio := deptBioGroup
	requests := []common.Project{
		// req_001: approved faculty research sandbox (funded by CS dept)
		{
			ID:              "req_001",
			Status:          common.ProjectStatusApproved,
			RequesterTokens: common.TokenList{"user:faculty@cs.example", deptCSFaculty},
			Quota:           common.ProjectQuota{"cores": 4, "ram": 16, "storage": 100, "gpu": 0},
			Reason:          "Faculty research sandbox",
			FundedBy:        &fundedByFaculty,
			Pending:         nil,
			TerminationDate: now.Add(90 * 24 * time.Hour).Format(time.RFC3339),
			AuthorizedUsers: []common.AuthorizedUser{
				{Token: "user:faculty@cs.example", OpenstackRole: "admin"},
				{Token: deptCSFaculty, OpenstackRole: "member"},
			},
			History: []common.HistoryEntry{
				{
					Timestamp:       "2026-01-20T10:00:00Z",
					Event:           "created",
					Actor:           "user:faculty@cs.example",
					StatusFrom:      nil,
					StatusTo:        common.ProjectStatusPending,
					QuotaTo:         &common.ProjectQuota{"cores": 4, "ram": 16, "storage": 100, "gpu": 0},
					TerminationDate: mockStrPtr(now.Add(90 * 24 * time.Hour).Format(time.RFC3339)),
					Reason:          mockStrPtr("Initial request for faculty research sandbox"),
				},
				{
					Timestamp:  "2026-01-21T09:00:00Z",
					Event:      "approved",
					Actor:      "user:admin@cs.example",
					Group:      &fundedByFaculty,
					StatusFrom: mockStrPtr(common.ProjectStatusPending),
					StatusTo:   common.ProjectStatusApproved,
					Reason:     mockStrPtr("Approved by CS admin"),
				},
			},
		},
		// req_002: pending student course project — exceeds allowance, needs manual approval
		{
			ID:              "req_002",
			Status:          common.ProjectStatusPending,
			RequesterTokens: common.TokenList{"user:student@cs.example", csStudentGroup},
			Quota:           common.ProjectQuota{"cores": 2, "ram": 8, "storage": 50, "gpu": 0},
			Reason:          "Student course project",
			FundedBy:        &deptCSFaculty,
			Pending:         nil,
			TerminationDate: now.Add(30 * 24 * time.Hour).Format(time.RFC3339),
			AuthorizedUsers: []common.AuthorizedUser{
				{Token: "user:student@cs.example", OpenstackRole: "admin"},
			},
			History: []common.HistoryEntry{{
				Timestamp:       "2026-01-23T08:00:00Z",
				Event:           "created",
				Actor:           "user:student@cs.example",
				StatusFrom:      nil,
				StatusTo:        common.ProjectStatusPending,
				QuotaTo:         &common.ProjectQuota{"cores": 2, "ram": 8, "storage": 50, "gpu": 0},
				TerminationDate: mockStrPtr(now.Add(30 * 24 * time.Hour).Format(time.RFC3339)),
				Reason:          mockStrPtr("Student course project needs compute (exceeds allowance)"),
			}},
		},
		// req_003: change_pending — faculty ML workload expansion (note: uses deptCSFaculty token, not "group:cs-faculty")
		{
			ID:              "req_003",
			Status:          common.ProjectStatusChangePending,
			RequesterTokens: common.TokenList{"user:faculty@cs.example", deptCSFaculty},
			Quota:           common.ProjectQuota{"cores": 8, "ram": 32, "storage": 200, "gpu": 0},
			Reason:          "Expanded faculty ML workload",
			FundedBy:        &fundedByFaculty,
			Pending: &common.PendingChanges{
				Quota:           &common.ProjectQuota{"cores": 12, "ram": 48, "storage": 300, "gpu": 0},
				TerminationDate: mockStrPtr(now.Add(180 * 24 * time.Hour).Format(time.RFC3339)),
				AuthorizedUsers: &[]common.AuthorizedUser{
					{Token: "user:faculty@cs.example", OpenstackRole: "admin"},
					{Token: deptCSFaculty, OpenstackRole: "member"},
					{Token: "user:newuser@cs.example", OpenstackRole: "reader"},
				},
			},
			TerminationDate: now.Add(60 * 24 * time.Hour).Format(time.RFC3339),
			AuthorizedUsers: []common.AuthorizedUser{
				{Token: "user:faculty@cs.example", OpenstackRole: "admin"},
				{Token: deptCSFaculty, OpenstackRole: "member"},
			},
			History: []common.HistoryEntry{
				{
					Timestamp:       "2026-01-15T10:00:00Z",
					Event:           "created",
					Actor:           "user:faculty@cs.example",
					StatusFrom:      nil,
					StatusTo:        common.ProjectStatusPending,
					QuotaTo:         &common.ProjectQuota{"cores": 8, "ram": 32, "storage": 200, "gpu": 0},
					TerminationDate: mockStrPtr(now.Add(60 * 24 * time.Hour).Format(time.RFC3339)),
					Reason:          mockStrPtr("Initial ML workload request"),
				},
				{
					Timestamp:  "2026-01-16T14:00:00Z",
					Event:      "approved",
					Actor:      "user:admin@cs.example",
					Group:      &fundedByFaculty,
					StatusFrom: mockStrPtr(common.ProjectStatusPending),
					StatusTo:   common.ProjectStatusApproved,
					Reason:     mockStrPtr("Approved by CS admin"),
				},
				{
					Timestamp:           "2026-01-25T11:30:00Z",
					Event:               "change_requested",
					Actor:               "user:faculty@cs.example",
					StatusFrom:          mockStrPtr(common.ProjectStatusApproved),
					StatusTo:            common.ProjectStatusChangePending,
					QuotaFrom:           &common.ProjectQuota{"cores": 8, "ram": 32, "storage": 200, "gpu": 0},
					QuotaTo:             &common.ProjectQuota{"cores": 12, "ram": 48, "storage": 300, "gpu": 0},
					TerminationDateFrom: mockStrPtr(now.Add(60 * 24 * time.Hour).Format(time.RFC3339)),
					TerminationDateTo:   mockStrPtr(now.Add(180 * 24 * time.Hour).Format(time.RFC3339)),
					Reason:              mockStrPtr("Need more resources for larger dataset"),
				},
			},
		},
		// req_004: approved bio genomics cluster (funded by bio dept)
		{
			ID:              "req_004",
			Status:          common.ProjectStatusApproved,
			RequesterTokens: common.TokenList{"user:faculty@bio.example", deptBioGroup},
			Quota:           common.ProjectQuota{"cores": 16, "ram": 64, "storage": 800, "gpu": 8},
			Reason:          "Genomics pipeline cluster",
			FundedBy:        &fundedByBio,
			Pending:         nil,
			TerminationDate: now.Add(180 * 24 * time.Hour).Format(time.RFC3339),
			AuthorizedUsers: []common.AuthorizedUser{
				{Token: "user:faculty@bio.example", OpenstackRole: "admin"},
				{Token: deptBioGroup, OpenstackRole: "member"},
			},
			History: []common.HistoryEntry{{
				Timestamp:       "2026-02-01T09:00:00Z",
				Event:           "created",
				Actor:           "user:faculty@bio.example",
				StatusFrom:      nil,
				StatusTo:        common.ProjectStatusApproved,
				QuotaTo:         &common.ProjectQuota{"cores": 16, "ram": 64, "storage": 800, "gpu": 8},
				TerminationDate: mockStrPtr(now.Add(180 * 24 * time.Hour).Format(time.RFC3339)),
				Reason:          mockStrPtr("Genomics pipeline cluster"),
			}},
		},
		// osonly_001: openstack_only — discovered by reconciler, not yet managed.
		// Quota is 9 cores: fits dept_cs_admin (18 free) but exceeds dept_cs_faculty (8 free).
		// Has one external group (a legacy LDAP group not in the delegation system) and one
		// managed group (dept_cs_faculty, which will appear in the promote modal as group: token).
		{
			ID:              "osonly_001",
			Status:          common.ProjectStatusOpenStackOnly,
			OSProjectID:     "os-project-abc-123",
			OSProjectName:   "legacy-ml-workload",
			Reason:          "OpenStack project: legacy-ml-workload (os-project-abc-123)",
			RequesterTokens: common.TokenList{"user:faculty@cs.example"},
			AuthorizedUsers: []common.AuthorizedUser{
				{Token: deptCSFaculty, OpenstackRole: "member"},
			},
			ExternalGroupAssignments: []common.ExternalGroupAssignment{
				{GroupID: "os-group-ldap-001", GroupName: "legacy-ldap-researchers", Role: "member"},
			},
			Quota:   common.ProjectQuota{"cores": 9, "ram": 16, "storage": 100, "gpu": 0},
			History: []common.HistoryEntry{},
		},
		// req_005: pending — CS dept requests extra resources from root (for root admin to manage)
		{
			ID:              "req_005",
			Status:          common.ProjectStatusPending,
			RequesterTokens: common.TokenList{"user:admin@cs.example", deptCSAdmin},
			Quota:           common.ProjectQuota{"cores": 50, "ram": 200, "storage": 1000, "gpu": 8},
			Reason:          "CS dept capacity expansion for next semester",
			FundedBy:        &rootGroup,
			Pending:         nil,
			TerminationDate: now.Add(365 * 24 * time.Hour).Format(time.RFC3339),
			AuthorizedUsers: []common.AuthorizedUser{
				{Token: "user:admin@cs.example", OpenstackRole: "admin"},
				{Token: deptCSAdmin, OpenstackRole: "member"},
			},
			History: []common.HistoryEntry{{
				Timestamp:       "2026-03-01T10:00:00Z",
				Event:           "created",
				Actor:           "user:admin@cs.example",
				StatusFrom:      nil,
				StatusTo:        common.ProjectStatusPending,
				QuotaTo:         &common.ProjectQuota{"cores": 50, "ram": 200, "storage": 1000, "gpu": 8},
				TerminationDate: mockStrPtr(now.Add(365 * 24 * time.Hour).Format(time.RFC3339)),
				Reason:          mockStrPtr("Increased enrollment requires more compute capacity"),
			}},
		},
	}

	eligibilityRules := []common.TokenEligibilityRule{
		{
			// Root admins allow CS and Bio depts to request from the university pool.
			// Root itself is also eligible so root admins can request directly from the root pool.
			OwnerToken:         rootGroup,
			EligibleRequesters: common.TokenList{rootGroup, "user:root.admin@uni.example", deptCSAdmin, deptBioGroup},
			CreatedBy:          "root.admin@uni.example",
			UpdatedAt:          "2025-01-01T00:00:00Z",
		},
		{
			// CS admins allow faculty to request from the CS dept pool.
			OwnerToken:         deptCSAdmin,
			EligibleRequesters: common.TokenList{deptCSFaculty, "user:faculty@cs.example", "user:admin@cs.example"},
			CreatedBy:          "admin@cs.example",
			UpdatedAt:          "2025-06-15T10:30:00Z",
		},
		{
			// CS faculty allow students to request from the faculty pool.
			OwnerToken:         deptCSFaculty,
			EligibleRequesters: common.TokenList{csStudentGroup, "user:student@cs.example"},
			CreatedBy:          "faculty@cs.example",
			UpdatedAt:          "2025-08-01T09:00:00Z",
		},
		{
			// Bio dept allows its own members to request from the bio pool.
			OwnerToken:         deptBioGroup,
			EligibleRequesters: common.TokenList{deptBioGroup, "user:faculty@bio.example"},
			CreatedBy:          "root.admin@uni.example",
			UpdatedAt:          "2025-07-20T14:15:00Z",
		},
	}

	return identities, delegations, requests, eligibilityRules
}

func mockStrPtr(s string) *string {
	return &s
}
