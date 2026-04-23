package webserver_test

// Quota state established by DefaultMockResourceState for dept_cs_faculty:
//
//	Limit:  {cores:20, ram:64, storage:400, gpu:2}
//	Active: req_001 approved  {cores:4,  ram:16, storage:100, gpu:0}
//	        req_003 change_pending {cores:8, ram:32, storage:200, gpu:0}
//	Used:   {cores:12, ram:48, storage:300, gpu:0}
//	Free:   {cores:8,  ram:16, storage:100, gpu:2}
//
// Approvers (must hold a token in delegation.AdminScope):
//
//	dept_cs_faculty  → faculty@cs.example  (token: group:dept_cs_faculty)
//	dept_cs_admin    → admin@cs.example    (token: group:dept_cs_admin)
//	group:root_uni   → root.admin@…        (token: group:root_uni)

import (
	"net/http"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/webserver"
)

// ── List my projects ──────────────────────────────────────────────────────────

func TestListMyProjects_ReturnOwnProjects(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodGet, "/v1/projects/mine", userFaculty, nil)
	assertStatus(t, rr, http.StatusOK)

	var projects []common.Project
	mustDecode(t, rr, &projects)

	// faculty@cs.example owns req_001 (approved) and req_003 (change_pending)
	if len(projects) < 2 {
		t.Fatalf("expected ≥2 projects for faculty, got %d: %v", len(projects), projectIDs(projects))
	}
	for _, p := range projects {
		hasToken := false
		for _, tok := range p.RequesterTokens {
			if tok == "user:faculty@cs.example" {
				hasToken = true
				break
			}
		}
		if !hasToken {
			t.Errorf("project %s should not appear in faculty's list (wrong requester)", p.ID)
		}
	}
}

func TestListMyProjects_DoesNotReturnOtherUsersProjects(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodGet, "/v1/projects/mine", userBio, nil)
	assertStatus(t, rr, http.StatusOK)

	var projects []common.Project
	mustDecode(t, rr, &projects)

	for _, p := range projects {
		for _, tok := range p.RequesterTokens {
			if tok == "user:faculty@cs.example" {
				t.Errorf("bio faculty should not see CS faculty project %s", p.ID)
			}
		}
	}
}

// ── Get project by ID ─────────────────────────────────────────────────────────

func TestGetProject_RequesterCanRead(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodGet, "/v1/projects/req_001", userFaculty, nil)
	assertStatus(t, rr, http.StatusOK)

	var p common.Project
	mustDecode(t, rr, &p)
	if p.ID != "req_001" {
		t.Errorf("expected project req_001, got %s", p.ID)
	}
}

func TestGetProject_DelegationAncestorAdminCanRead(t *testing.T) {
	h := setupRouter(t)
	// req_001 is funded by dept_cs_faculty; admin@cs.example is admin of dept_cs_admin
	// (the parent delegation), so they should see it via the ancestor walk.
	rr := do(t, h, http.MethodGet, "/v1/projects/req_001", userCSAdmin, nil)
	assertStatus(t, rr, http.StatusOK)
}

func TestGetProject_UnrelatedUserGetsForbidden(t *testing.T) {
	h := setupRouter(t)
	// bio faculty has no connection to req_001 (CS faculty project)
	rr := do(t, h, http.MethodGet, "/v1/projects/req_001", userBio, nil)
	assertStatus(t, rr, http.StatusForbidden)
}

func TestGetProject_NotFound(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodGet, "/v1/projects/does-not-exist", userRoot, nil)
	assertStatus(t, rr, http.StatusNotFound)
}

// ── List projects to manage ───────────────────────────────────────────────────

func TestListProjectsToManage_RootAdminSeesRootPending(t *testing.T) {
	h := setupRouter(t)
	// req_005 is pending, funded by group:root_uni — only root admin sees it
	rr := do(t, h, http.MethodGet, "/v1/projects/manage", userRoot, nil)
	assertStatus(t, rr, http.StatusOK)

	var projects []common.Project
	mustDecode(t, rr, &projects)

	found := false
	for _, p := range projects {
		if p.ID == "req_005" {
			found = true
		}
	}
	if !found {
		t.Errorf("root admin should see req_005 in manage list; got %v", projectIDs(projects))
	}
}

func TestListProjectsToManage_FacultySeesOwnDelegationPending(t *testing.T) {
	h := setupRouter(t)
	// faculty is admin of dept_cs_faculty; req_002 (pending) and req_003 (change_pending)
	// are both funded by dept_cs_faculty
	rr := do(t, h, http.MethodGet, "/v1/projects/manage", userFaculty, nil)
	assertStatus(t, rr, http.StatusOK)

	var projects []common.Project
	mustDecode(t, rr, &projects)

	ids := projectIDs(projects)
	for _, want := range []string{"req_002", "req_003"} {
		found := false
		for _, id := range ids {
			if id == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("faculty should see %s in manage list; got %v", want, ids)
		}
	}
}

func TestListProjectsToManage_NonAdminSeesEmpty(t *testing.T) {
	h := setupRouter(t)
	// bio faculty is admin of dept_bio, but req_004 is approved — no pending projects in that delegation
	rr := do(t, h, http.MethodGet, "/v1/projects/manage", userBio, nil)
	assertStatus(t, rr, http.StatusOK)

	var projects []common.Project
	mustDecode(t, rr, &projects)

	if len(projects) != 0 {
		t.Errorf("bio faculty should not see any pending projects to manage, got %v", projectIDs(projects))
	}
}

// ── Create project ────────────────────────────────────────────────────────────

func TestCreateProject_PoolDelegation_CreatesPending(t *testing.T) {
	h := setupRouter(t)
	// faculty@cs.example is in the eligible-requesters of group:dept_cs_admin (pool strategy)
	// → project must start as pending (no auto-approval for pool delegations)
	body := webserver.CreateProjectRequest{
		Quota:               common.ProjectQuota{"cores": 2, "ram": 4, "storage": 20, "gpu": 0},
		Reason:              "test pool project",
		TerminationDate:     futureDate(30),
		FundingDelegationID: "group:dept_cs_admin",
		AuthorizedUsers:     []common.AuthorizedUser{{Token: "user:faculty@cs.example", OpenstackRole: "admin"}},
	}
	rr := do(t, h, http.MethodPost, "/v1/projects", userFaculty, body)
	assertStatus(t, rr, http.StatusCreated)

	var p common.Project
	mustDecode(t, rr, &p)
	if p.Status != common.ProjectStatusPending {
		t.Errorf("pool-funded project should start as pending, got %q", p.Status)
	}
	if p.ID == "" {
		t.Error("created project should have an ID")
	}
}

func TestCreateProject_AllowanceDelegation_AutoApprovedWithinLimit(t *testing.T) {
	h := setupRouter(t)
	// dept_cs_students: allowance, limit={cores:2, ram:4, storage:20, gpu:0}
	// cs-student is in admin_scope → eligible; quota fits → auto-approved
	body := webserver.CreateProjectRequest{
		Quota:               common.ProjectQuota{"cores": 2, "ram": 4, "storage": 10, "gpu": 0},
		Reason:              "student project within allowance",
		TerminationDate:     futureDate(30),
		FundingDelegationID: "dept_cs_students",
		AuthorizedUsers:     []common.AuthorizedUser{{Token: "user:cs-student@cs.com", OpenstackRole: "member"}},
	}
	rr := do(t, h, http.MethodPost, "/v1/projects", userStudent, body)
	assertStatus(t, rr, http.StatusCreated)

	var p common.Project
	mustDecode(t, rr, &p)
	if p.Status != common.ProjectStatusApproved {
		t.Errorf("allowance project within limit should be auto-approved, got %q", p.Status)
	}
}

func TestCreateProject_AllowanceDelegation_StaysPendingWhenOverLimit(t *testing.T) {
	h := setupRouter(t)
	// cores=3 exceeds the allowance limit of 2 → no auto-approval
	body := webserver.CreateProjectRequest{
		Quota:               common.ProjectQuota{"cores": 3, "ram": 4, "storage": 10, "gpu": 0},
		Reason:              "student project over allowance",
		TerminationDate:     futureDate(30),
		FundingDelegationID: "dept_cs_students",
		AuthorizedUsers:     []common.AuthorizedUser{{Token: "user:cs-student@cs.com", OpenstackRole: "member"}},
	}
	rr := do(t, h, http.MethodPost, "/v1/projects", userStudent, body)
	assertStatus(t, rr, http.StatusCreated)

	var p common.Project
	mustDecode(t, rr, &p)
	if p.Status != common.ProjectStatusPending {
		t.Errorf("allowance project over limit should stay pending for manual review, got %q", p.Status)
	}
}

func TestCreateProject_IneligibleDelegation_Rejected(t *testing.T) {
	h := setupRouter(t)
	// bio faculty has no eligibility rule for dept_cs_faculty
	body := webserver.CreateProjectRequest{
		Quota:               common.ProjectQuota{"cores": 2, "ram": 4, "storage": 20, "gpu": 0},
		Reason:              "cross-dept request",
		TerminationDate:     futureDate(30),
		FundingDelegationID: "group:dept_cs_faculty",
		AuthorizedUsers:     []common.AuthorizedUser{{Token: "user:faculty@bio.example", OpenstackRole: "admin"}},
	}
	rr := do(t, h, http.MethodPost, "/v1/projects", userBio, body)
	assertStatus(t, rr, http.StatusBadRequest)
}

// ── Approve project ───────────────────────────────────────────────────────────

func TestApproveProject_SucceedsWithinQuota(t *testing.T) {
	h := setupRouter(t)
	// req_002: pending, funded by dept_cs_faculty, quota={cores:2,...}
	// 12 cores already active; adding 2 → 14 ≤ 20 limit → passes
	// faculty@cs.example is admin of dept_cs_faculty → may approve
	rr := do(t, h, http.MethodPost, "/v1/projects/req_002/approve", userFaculty,
		webserver.ApproveProjectRequest{DelegationID: "group:dept_cs_faculty"})
	assertStatus(t, rr, http.StatusOK)

	var p common.Project
	mustDecode(t, rr, &p)
	if p.Status != common.ProjectStatusApproved {
		t.Errorf("expected approved, got %q", p.Status)
	}
}

func TestApproveProject_FailsWhenDirectPoolCapacityExceeded(t *testing.T) {
	h := setupRouter(t)

	// cs-student is in the eligible-requesters of group:dept_cs_faculty.
	// Create a pending project requesting 9 cores — 12 already active + 9 = 21 > 20 limit.
	// faculty (admin of dept_cs_faculty) approving it must fail.
	createBody := webserver.CreateProjectRequest{
		Quota:               common.ProjectQuota{"cores": 9, "ram": 4, "storage": 10, "gpu": 0},
		Reason:              "over-quota attempt",
		TerminationDate:     futureDate(30),
		FundingDelegationID: "group:dept_cs_faculty",
		AuthorizedUsers:     []common.AuthorizedUser{{Token: "user:cs-student@cs.com", OpenstackRole: "member"}},
	}
	createRR := do(t, h, http.MethodPost, "/v1/projects", userStudent, createBody)
	assertStatus(t, createRR, http.StatusCreated)

	var created common.Project
	mustDecode(t, createRR, &created)

	rr := do(t, h, http.MethodPost, "/v1/projects/"+created.ID+"/approve", userFaculty,
		webserver.ApproveProjectRequest{DelegationID: "group:dept_cs_faculty"})
	assertStatus(t, rr, http.StatusBadRequest)
}

func TestApproveProject_FailsWhenAncestorPoolExceeded(t *testing.T) {
	h := setupRouter(t)
	// dept_cs_admin limit: 30 cores.
	// Subtree already has 12 cores active (from faculty projects, rolled up to dept_cs_admin).
	//
	// Step 1: approve a 17-core project in dept_cs_admin → subtree total = 12+17 = 29 ≤ 30 ✓
	// Step 2: try to approve a 2-core project in dept_cs_admin → 29+2 = 31 > 30 ✗

	createAndApprove := func(cores int) (common.Project, int) {
		t.Helper()
		body := webserver.CreateProjectRequest{
			Quota:               common.ProjectQuota{"cores": cores, "ram": 4, "storage": 10, "gpu": 0},
			Reason:              "ancestor capacity test",
			TerminationDate:     futureDate(30),
			FundingDelegationID: "group:dept_cs_admin",
			AuthorizedUsers:     []common.AuthorizedUser{{Token: "user:admin@cs.example", OpenstackRole: "admin"}},
		}
		createRR := do(t, h, http.MethodPost, "/v1/projects", userCSAdmin, body)
		var created common.Project
		mustDecode(t, createRR, &created)
		approveRR := do(t, h, http.MethodPost, "/v1/projects/"+created.ID+"/approve", userCSAdmin,
			webserver.ApproveProjectRequest{DelegationID: "group:dept_cs_admin"})
		return created, approveRR.Code
	}

	_, code1 := createAndApprove(17)
	if code1 != http.StatusOK {
		t.Fatalf("first approval (17 cores, subtotal 29/30) should succeed, got %d", code1)
	}

	_, code2 := createAndApprove(2)
	if code2 != http.StatusBadRequest {
		t.Errorf("second approval (2 cores, would reach 31/30) should fail with 400, got %d", code2)
	}
}

func TestApproveProject_ExactlyAtCapacity_Succeeds(t *testing.T) {
	h := setupRouter(t)
	// cs-student requests exactly the 8 remaining cores in dept_cs_faculty (12 active + 8 = 20 = limit).
	createBody := webserver.CreateProjectRequest{
		Quota:               common.ProjectQuota{"cores": 8, "ram": 4, "storage": 10, "gpu": 0},
		Reason:              "exactly at capacity",
		TerminationDate:     futureDate(30),
		FundingDelegationID: "group:dept_cs_faculty",
		AuthorizedUsers:     []common.AuthorizedUser{{Token: "user:cs-student@cs.com", OpenstackRole: "member"}},
	}
	createRR := do(t, h, http.MethodPost, "/v1/projects", userStudent, createBody)
	assertStatus(t, createRR, http.StatusCreated)

	var created common.Project
	mustDecode(t, createRR, &created)

	rr := do(t, h, http.MethodPost, "/v1/projects/"+created.ID+"/approve", userFaculty,
		webserver.ApproveProjectRequest{DelegationID: "group:dept_cs_faculty"})
	assertStatus(t, rr, http.StatusOK)
}

func TestApproveProject_NonAdminGetsForbidden(t *testing.T) {
	h := setupRouter(t)
	// bio faculty is not in dept_cs_faculty.AdminScope
	rr := do(t, h, http.MethodPost, "/v1/projects/req_002/approve", userBio,
		webserver.ApproveProjectRequest{DelegationID: "group:dept_cs_faculty"})
	assertStatus(t, rr, http.StatusForbidden)
}

func TestApproveProject_ChangePending_SubtractsCurrentQuotaBeforeCheck(t *testing.T) {
	h := setupRouter(t)
	// req_003 is change_pending with current quota {cores:8,...} and pending quota {cores:12,...}.
	// When approving the change, the 8 cores already committed must not be double-counted.
	// Effective check: (12 total active - 8 current) + 12 pending = 4 + 12 = 16 ≤ 20 → passes.
	rr := do(t, h, http.MethodPost, "/v1/projects/req_003/approve", userFaculty,
		webserver.ApproveProjectRequest{DelegationID: "group:dept_cs_faculty"})
	assertStatus(t, rr, http.StatusOK)

	var p common.Project
	mustDecode(t, rr, &p)
	if p.Status != common.ProjectStatusApproved {
		t.Errorf("expected approved, got %q", p.Status)
	}
	if p.Quota["cores"] != 12 {
		t.Errorf("expected final cores=12 (from pending quota), got %d", p.Quota["cores"])
	}
}

func TestApproveProject_WithModifiedQuota_UsesOverride(t *testing.T) {
	h := setupRouter(t)
	// Approve req_002 with a quota override smaller than the request.
	override := common.ProjectQuota{"cores": 1, "ram": 2, "storage": 10, "gpu": 0}
	rr := do(t, h, http.MethodPost, "/v1/projects/req_002/approve", userFaculty,
		webserver.ApproveProjectRequest{
			DelegationID:  "group:dept_cs_faculty",
			ModifiedQuota: &override,
		})
	assertStatus(t, rr, http.StatusOK)

	var p common.Project
	mustDecode(t, rr, &p)
	if p.Quota["cores"] != 1 {
		t.Errorf("expected modified quota cores=1, got %d", p.Quota["cores"])
	}
}

// ── Reject project ────────────────────────────────────────────────────────────

func TestRejectProject_PendingBecomesRejected(t *testing.T) {
	h := setupRouter(t)
	reason := "not needed"
	rr := do(t, h, http.MethodPost, "/v1/projects/req_002/reject", userFaculty,
		webserver.RejectProjectRequest{Reason: &reason})
	assertStatus(t, rr, http.StatusOK)

	var p common.Project
	mustDecode(t, rr, &p)
	if p.Status != common.ProjectStatusRejected {
		t.Errorf("expected rejected, got %q", p.Status)
	}
}

func TestRejectProject_ChangePendingBecomesChangeRejected(t *testing.T) {
	h := setupRouter(t)
	// req_003 is change_pending — rejection produces change_rejected, not rejected
	rr := do(t, h, http.MethodPost, "/v1/projects/req_003/reject", userFaculty,
		webserver.RejectProjectRequest{})
	assertStatus(t, rr, http.StatusOK)

	var p common.Project
	mustDecode(t, rr, &p)
	if p.Status != common.ProjectStatusChangeRejected {
		t.Errorf("expected change_rejected, got %q", p.Status)
	}
}

// ── Update project ────────────────────────────────────────────────────────────

func TestUpdateProject_ApprovedTransitionsToChangePending(t *testing.T) {
	h := setupRouter(t)
	newQuota := common.ProjectQuota{"cores": 6, "ram": 24, "storage": 150, "gpu": 0}
	rr := do(t, h, http.MethodPut, "/v1/projects/req_001", userFaculty,
		webserver.UpdateProjectRequest{Quota: &newQuota})
	assertStatus(t, rr, http.StatusOK)

	var p common.Project
	mustDecode(t, rr, &p)
	if p.Status != common.ProjectStatusChangePending {
		t.Errorf("expected change_pending, got %q", p.Status)
	}
	if p.Pending == nil || p.Pending.Quota == nil {
		t.Fatal("expected pending.quota to be set")
	}
	if (*p.Pending.Quota)["cores"] != 6 {
		t.Errorf("expected pending cores=6, got %d", (*p.Pending.Quota)["cores"])
	}
}

// ── Release project ───────────────────────────────────────────────────────────

func TestReleaseProject_ApprovedBecomesReleased(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodPost, "/v1/projects/req_001/release", userFaculty, nil)
	assertStatus(t, rr, http.StatusOK)

	var p common.Project
	mustDecode(t, rr, &p)
	if p.Status != common.ProjectStatusReleased {
		t.Errorf("expected released, got %q", p.Status)
	}
}

func TestReleaseProject_PendingFails(t *testing.T) {
	h := setupRouter(t)
	// req_002 is pending — only approved projects can be released
	rr := do(t, h, http.MethodPost, "/v1/projects/req_002/release", userFaculty, nil)
	assertStatus(t, rr, http.StatusBadRequest)
}

func TestReleaseProject_ReleasedCapacityIsReclaimed(t *testing.T) {
	h := setupRouter(t)
	// dept_cs_faculty: 12 cores active, 8 remaining.
	// cs-student requests 9 cores → would exceed limit (12+9=21>20) without release.
	// After releasing req_001 (4 cores): 8 cores active, 12 remaining → 8+9=17 ≤ 20 → passes.

	releaseRR := do(t, h, http.MethodPost, "/v1/projects/req_001/release", userFaculty, nil)
	assertStatus(t, releaseRR, http.StatusOK)

	createBody := webserver.CreateProjectRequest{
		Quota:               common.ProjectQuota{"cores": 9, "ram": 4, "storage": 10, "gpu": 0},
		Reason:              "reclaimed capacity test",
		TerminationDate:     futureDate(30),
		FundingDelegationID: "group:dept_cs_faculty",
		AuthorizedUsers:     []common.AuthorizedUser{{Token: "user:cs-student@cs.com", OpenstackRole: "member"}},
	}
	createRR := do(t, h, http.MethodPost, "/v1/projects", userStudent, createBody)
	assertStatus(t, createRR, http.StatusCreated)

	var created common.Project
	mustDecode(t, createRR, &created)

	approveRR := do(t, h, http.MethodPost, "/v1/projects/"+created.ID+"/approve", userFaculty,
		webserver.ApproveProjectRequest{DelegationID: "group:dept_cs_faculty"})
	assertStatus(t, approveRR, http.StatusOK)
}
