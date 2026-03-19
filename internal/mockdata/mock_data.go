package mockdata

import (
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/webserver"
)

// DefaultMockResourceState returns the seed data used for development/testing.
func DefaultMockResourceState(now time.Time) ([]webserver.Identity, []webserver.Delegation, []webserver.Request) {
	now = now.UTC()
	plusDays := func(days int) *string {
		t := now.Add(time.Duration(days) * 24 * time.Hour).Format(time.RFC3339)
		return &t
	}

	rootGroup := "group:root_uni"
	deptCSAdmin := "group:dept_cs_admin"
	deptCSFaculty := "group:dept_cs_faculty"
	deptBioGroup := "group:dept_bio"
	csStudentGroup := "group:cs-student"

	identities := []webserver.Identity{
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
			ID:     "mock_student",
			Label:  "Mock CS Student (cs-student)",
			Email:  "student@cs.example",
			Tokens: common.TokenList{"user:student@cs.example", csStudentGroup},
		},
		{
			ID:     "mock_bio_faculty",
			Label:  "Mock Faculty (bio-faculty)",
			Email:  "faculty@bio.example",
			Tokens: common.TokenList{"user:faculty@bio.example", deptBioGroup},
		},
	}

	delegations := []webserver.Delegation{
		{
			ID:                 rootGroup,
			Name:               "University Root",
			ParentID:           nil,
			CanDelegate:        true,
			DelegationStrategy: webserver.DelegationStrategyPool,
			DelegationScope:    common.TokenList{rootGroup},
			Resources: webserver.Resources{
				Limit: webserver.ResourceQuota{"cores": 999999, "ram": 999999, "storage": 999999, "gpu": 999999},
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
			DelegationStrategy: webserver.DelegationStrategyPool,
			DelegationScope:    common.TokenList{deptCSAdmin},
			Resources: webserver.Resources{
				Limit: webserver.ResourceQuota{"cores": 500, "ram": 2000, "storage": 5000, "gpu": 50},
			},
			CreatedBy: "root.admin@uni.example",
			CreatedAt: "2025-06-15T10:30:00Z",
			EndDate:   plusDays(365),
		},
		{
			ID:                 "dept_cs_students",
			Name:               "CS Students (Small VM)",
			ParentID:           &deptCSAdmin,
			CanDelegate:        false,
			DelegationStrategy: webserver.DelegationStrategyAllowance,
			DelegationScope:    common.TokenList{csStudentGroup},
			Resources: webserver.Resources{
				Limit: webserver.ResourceQuota{"cores": 2, "ram": 4, "storage": 20, "gpu": 0},
			},
			CreatedBy: "faculty@cs.example",
			CreatedAt: "2025-09-01T09:00:00Z",
			EndDate:   nil,
		},
		{
			ID:                 "dept_bio",
			Name:               "Biology Dept",
			ParentID:           &rootGroup,
			CanDelegate:        true,
			DelegationStrategy: webserver.DelegationStrategyPool,
			DelegationScope:    common.TokenList{deptBioGroup},
			Resources: webserver.Resources{
				Limit: webserver.ResourceQuota{"cores": 300, "ram": 1000, "storage": 3000, "gpu": 20},
			},
			CreatedBy: "root.admin@uni.example",
			CreatedAt: "2025-07-20T14:15:00Z",
			EndDate:   nil,
		},
	}

	fundedByCS := deptCSAdmin
	requests := []webserver.Request{
		{
			ID:              "req_001",
			Status:          "approved",
			RequesterTokens: common.TokenList{"user:faculty@cs.example", deptCSFaculty},
			Resources:       webserver.ResourceQuota{"cores": 4, "ram": 16, "storage": 100, "gpu": 0},
			Reason:          "Faculty research sandbox",
			FundedBy:        &fundedByCS,
			Pending:         nil,
			TerminationDate: now.Add(90 * 24 * time.Hour).Format(time.RFC3339),
			AuthorizedUsers: []webserver.AuthorizedUser{
				{Token: "user:faculty@cs.example", GroupRole: "admin", OpenstackRole: "admin"},
				{Token: deptCSFaculty, GroupRole: "member", OpenstackRole: "member"},
			},
			History: []webserver.HistoryEntry{
				{
					Timestamp:       "2026-01-20T10:00:00Z",
					Event:           "created",
					Actor:           "user:faculty@cs.example",
					StatusFrom:      nil,
					StatusTo:        "pending",
					QuotaFrom:       nil,
					QuotaTo:         &webserver.ResourceQuota{"cores": 4, "ram": 16, "storage": 100, "gpu": 0},
					TerminationDate: mockStrPtr(now.Add(90 * 24 * time.Hour).Format(time.RFC3339)),
					Reason:          mockStrPtr("Initial request for faculty research sandbox"),
				},
				{
					Timestamp:  "2026-01-21T09:00:00Z",
					Event:      "approved",
					Actor:      "admin:root.admin@uni.example",
					Group:      &fundedByCS,
					StatusFrom: mockStrPtr("pending"),
					StatusTo:   "approved",
					QuotaFrom:  nil,
					QuotaTo:    nil,
					Reason:     mockStrPtr("Approved by root admin"),
				},
			},
		},
		{
			ID:              "req_002",
			Status:          "pending",
			RequesterTokens: common.TokenList{"user:student@cs.example", csStudentGroup},
			Resources:       webserver.ResourceQuota{"cores": 2, "ram": 8, "storage": 50, "gpu": 0},
			Reason:          "Student course project",
			FundedBy:        nil,
			Pending:         nil,
			TerminationDate: now.Add(30 * 24 * time.Hour).Format(time.RFC3339),
			AuthorizedUsers: []webserver.AuthorizedUser{{Token: "user:student@cs.example", GroupRole: "admin", OpenstackRole: "admin"}},
			History: []webserver.HistoryEntry{{
				Timestamp:       "2026-01-23T08:00:00Z",
				Event:           "created",
				Actor:           "user:student@cs.example",
				StatusFrom:      nil,
				StatusTo:        "pending",
				QuotaFrom:       nil,
				QuotaTo:         &webserver.ResourceQuota{"cores": 2, "ram": 8, "storage": 50, "gpu": 0},
				TerminationDate: mockStrPtr(now.Add(30 * 24 * time.Hour).Format(time.RFC3339)),
				Reason:          mockStrPtr("Student course project needs compute"),
			}},
		},
		{
			ID:              "req_003",
			Status:          "change_pending",
			RequesterTokens: common.TokenList{"user:faculty@cs.example", "group:cs-faculty"},
			Resources:       webserver.ResourceQuota{"cores": 8, "ram": 32, "storage": 200, "gpu": 0},
			Reason:          "Expanded faculty ML workload",
			FundedBy:        &fundedByCS,
			Pending: &webserver.PendingChanges{
				Quota:           &webserver.ResourceQuota{"cores": 12, "ram": 48, "storage": 300, "gpu": 0},
				TerminationDate: mockStrPtr(now.Add(180 * 24 * time.Hour).Format(time.RFC3339)),
				AuthorizedUsers: &[]webserver.AuthorizedUser{{Token: "user:faculty@cs.example", GroupRole: "admin", OpenstackRole: "admin"}, {Token: "group:cs-faculty", GroupRole: "member", OpenstackRole: "member"}, {Token: "user:newuser@cs.example", GroupRole: "viewer", OpenstackRole: "reader"}},
			},
			TerminationDate: now.Add(60 * 24 * time.Hour).Format(time.RFC3339),
			AuthorizedUsers: []webserver.AuthorizedUser{
				{Token: "user:faculty@cs.example", GroupRole: "admin", OpenstackRole: "admin"},
				{Token: "group:cs-faculty", GroupRole: "member", OpenstackRole: "member"},
			},
			History: []webserver.HistoryEntry{
				{
					Timestamp:       "2026-01-15T10:00:00Z",
					Event:           "created",
					Actor:           "user:faculty@cs.example",
					StatusFrom:      nil,
					StatusTo:        "pending",
					QuotaFrom:       nil,
					QuotaTo:         &webserver.ResourceQuota{"cores": 8, "ram": 32, "storage": 200, "gpu": 0},
					TerminationDate: mockStrPtr(now.Add(60 * 24 * time.Hour).Format(time.RFC3339)),
					Reason:          mockStrPtr("Initial ML workload request"),
				},
				{
					Timestamp:  "2026-01-16T14:00:00Z",
					Event:      "approved",
					Actor:      "admin:root.admin@uni.example",
					Group:      &fundedByCS,
					StatusFrom: mockStrPtr("pending"),
					StatusTo:   "approved",
					QuotaFrom:  nil,
					QuotaTo:    nil,
					Reason:     mockStrPtr("Approved by root admin"),
				},
				{
					Timestamp:           "2026-01-25T11:30:00Z",
					Event:               "change_requested",
					Actor:               "user:faculty@cs.example",
					StatusFrom:          mockStrPtr("approved"),
					StatusTo:            "change_pending",
					QuotaFrom:           &webserver.ResourceQuota{"cores": 8, "ram": 32, "storage": 200, "gpu": 0},
					QuotaTo:             &webserver.ResourceQuota{"cores": 12, "ram": 48, "storage": 300, "gpu": 0},
					TerminationDateFrom: mockStrPtr(now.Add(60 * 24 * time.Hour).Format(time.RFC3339)),
					TerminationDateTo:   mockStrPtr(now.Add(180 * 24 * time.Hour).Format(time.RFC3339)),
					Reason:              mockStrPtr("Need more resources for larger dataset"),
				},
			},
		},
	}

	return identities, delegations, requests
}

func mockStrPtr(s string) *string {
	return &s
}
