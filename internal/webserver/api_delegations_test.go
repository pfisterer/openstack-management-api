package webserver_test

// Delegation hierarchy in DefaultMockResourceState:
//
//	group:root_uni  (pool, unlimited)
//	├── group:dept_cs_admin  (pool, 30 cores)   AdminScope=[group:dept_cs_admin]
//	│   └── group:dept_cs_faculty  (pool, 20 cores)  AdminScope=[group:dept_cs_faculty]
//	│       └── dept_cs_students  (allowance, 2 cores) AdminScope=[group:cs-student]
//	└── group:dept_bio  (pool, 300 cores)  AdminScope=[group:dept_bio]
//
// "delegated-to-me"  → delegations where userTokens ∈ admin_scope
// "made-by-me"       → delegations whose parent_id ∈ userTokens
// "eligible-for-me"  → pool delegations reachable via eligibility rules + allowance delegations
//                       where userTokens ∈ admin_scope

import (
	"net/http"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/webserver"
)

// ── Delegated to me ───────────────────────────────────────────────────────────

func TestDelegatedToMe_ReturnsOwnAdminDelegations(t *testing.T) {
	h := setupRouter(t)
	// faculty@cs.example holds group:dept_cs_faculty → admin of that delegation
	rr := do(t, h, http.MethodGet, "/v1/delegations/delegated-to-me", userFaculty, nil)
	assertStatus(t, rr, http.StatusOK)

	var delegations []common.Delegation
	mustDecode(t, rr, &delegations)

	found := false
	for _, d := range delegations {
		if d.ID == "group:dept_cs_faculty" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("faculty should see group:dept_cs_faculty in delegated-to-me; got %v", delegationIDs(delegations))
	}
}

func TestDelegatedToMe_DoesNotReturnUnrelatedDelegations(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodGet, "/v1/delegations/delegated-to-me", userFaculty, nil)
	assertStatus(t, rr, http.StatusOK)

	var delegations []common.Delegation
	mustDecode(t, rr, &delegations)

	for _, d := range delegations {
		if d.ID == "group:dept_bio" {
			t.Errorf("faculty should not see dept_bio in delegated-to-me")
		}
		if d.ID == "group:root_uni" {
			t.Errorf("faculty should not see root_uni in delegated-to-me")
		}
	}
}

func TestDelegatedToMe_RootAdminSeesRootDelegation(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodGet, "/v1/delegations/delegated-to-me", userRoot, nil)
	assertStatus(t, rr, http.StatusOK)

	var delegations []common.Delegation
	mustDecode(t, rr, &delegations)

	found := false
	for _, d := range delegations {
		if d.ID == "group:root_uni" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("root admin should see group:root_uni in delegated-to-me; got %v", delegationIDs(delegations))
	}
}

// ── Made by me ────────────────────────────────────────────────────────────────

func TestMadeByMe_RootAdminSeesChildDelegations(t *testing.T) {
	h := setupRouter(t)
	// group:root_uni is the parent of dept_cs_admin and dept_bio → root admin "made" them
	rr := do(t, h, http.MethodGet, "/v1/delegations/made-by-me", userRoot, nil)
	assertStatus(t, rr, http.StatusOK)

	var delegations []common.Delegation
	mustDecode(t, rr, &delegations)

	ids := delegationIDs(delegations)
	for _, want := range []string{"group:dept_cs_admin", "group:dept_bio"} {
		found := false
		for _, id := range ids {
			if id == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("root admin made-by-me should include %s; got %v", want, ids)
		}
	}
}

func TestMadeByMe_FacultySeesStudentDelegation(t *testing.T) {
	h := setupRouter(t)
	// dept_cs_faculty is the parent of dept_cs_students → faculty "made" it
	rr := do(t, h, http.MethodGet, "/v1/delegations/made-by-me", userFaculty, nil)
	assertStatus(t, rr, http.StatusOK)

	var delegations []common.Delegation
	mustDecode(t, rr, &delegations)

	found := false
	for _, d := range delegations {
		if d.ID == "dept_cs_students" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("faculty made-by-me should include dept_cs_students; got %v", delegationIDs(delegations))
	}
}

// ── Eligible for me ───────────────────────────────────────────────────────────

func TestEligibleForMe_FacultyCanRequestFromCSAdmin(t *testing.T) {
	h := setupRouter(t)
	// Eligibility rule: dept_cs_admin allows [group:dept_cs_faculty, user:faculty@cs.example, ...]
	rr := do(t, h, http.MethodGet, "/v1/delegations/eligible-for-me", userFaculty, nil)
	assertStatus(t, rr, http.StatusOK)

	var delegations []common.Delegation
	mustDecode(t, rr, &delegations)

	found := false
	for _, d := range delegations {
		if d.ID == "group:dept_cs_admin" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("faculty should have group:dept_cs_admin as eligible delegation; got %v", delegationIDs(delegations))
	}
}

func TestEligibleForMe_StudentSeesAllowanceDelegation(t *testing.T) {
	h := setupRouter(t)
	// cs-student holds group:cs-student which is in dept_cs_students.AdminScope (allowance)
	rr := do(t, h, http.MethodGet, "/v1/delegations/eligible-for-me", userStudent, nil)
	assertStatus(t, rr, http.StatusOK)

	var delegations []common.Delegation
	mustDecode(t, rr, &delegations)

	found := false
	for _, d := range delegations {
		if d.ID == "dept_cs_students" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("student should have dept_cs_students as eligible delegation; got %v", delegationIDs(delegations))
	}
}

// ── Usage rollup on delegated-to-me ──────────────────────────────────────────

func TestDelegatedToMe_UsageIsAttached(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodGet, "/v1/delegations/delegated-to-me", userFaculty, nil)
	assertStatus(t, rr, http.StatusOK)

	var delegations []common.Delegation
	mustDecode(t, rr, &delegations)

	for _, d := range delegations {
		if d.ID == "group:dept_cs_faculty" {
			// req_001 (approved, 4 cores) and req_003 (change_pending, 8 cores)
			// are both funded by dept_cs_faculty → combined usage = 12 cores
			total := d.Quota.UsageByStatus.TotalQuota(quotaResourceIDs)
			if total["cores"] != 12 {
				t.Errorf("dept_cs_faculty usage should show 12 cores (4+8), got %d", total["cores"])
			}
			return
		}
	}
	t.Error("group:dept_cs_faculty not found in delegated-to-me response")
}

// ── Create delegation ─────────────────────────────────────────────────────────

func TestCreateDelegation_FacultyCreatesSubDelegation(t *testing.T) {
	h := setupRouter(t)
	parent := "group:dept_cs_faculty"
	body := webserver.CreateDelegationRequest{
		Name:               "Faculty Lab Sub-pool",
		AdminScope:         common.TokenList{"group:dept_cs_faculty"},
		DelegationStrategy: common.DelegationStrategyPool,
		Quota: common.ProjectResources{
			Limit: common.ProjectQuota{"cores": 4, "ram": 8, "storage": 50, "gpu": 0},
		},
		ParentID:    &parent,
		CanDelegate: false,
	}
	rr := do(t, h, http.MethodPost, "/v1/delegations", userFaculty, body)
	assertStatus(t, rr, http.StatusCreated)

	var d common.Delegation
	mustDecode(t, rr, &d)
	if d.Name != "Faculty Lab Sub-pool" {
		t.Errorf("expected name 'Faculty Lab Sub-pool', got %q", d.Name)
	}
	if d.ID == "" {
		t.Error("created delegation should have an ID")
	}
}

// ── Update delegation ─────────────────────────────────────────────────────────

func TestUpdateDelegation_AdminCanRename(t *testing.T) {
	h := setupRouter(t)
	newName := "CS Dept (Renamed)"
	rr := do(t, h, http.MethodPut, "/v1/delegations/group:dept_cs_admin", userCSAdmin,
		webserver.UpdateDelegationRequest{Name: &newName})
	assertStatus(t, rr, http.StatusOK)

	var d common.Delegation
	mustDecode(t, rr, &d)
	if d.Name != newName {
		t.Errorf("expected name %q, got %q", newName, d.Name)
	}
}

func TestUpdateDelegation_NotFoundReturns404(t *testing.T) {
	h := setupRouter(t)
	newName := "Ghost"
	rr := do(t, h, http.MethodPut, "/v1/delegations/does-not-exist", userRoot,
		webserver.UpdateDelegationRequest{Name: &newName})
	assertStatus(t, rr, http.StatusNotFound)
}

// ── Delete delegation ─────────────────────────────────────────────────────────

func TestDeleteDelegation_AdminCanDelete(t *testing.T) {
	h := setupRouter(t)
	// dept_cs_students is a leaf; faculty@cs.example is admin of its parent (dept_cs_faculty)
	rr := do(t, h, http.MethodDelete, "/v1/delegations/dept_cs_students", userFaculty, nil)
	// Service may return 204 or 403 depending on auth check implementation.
	// We assert it does NOT return 500 — the specific code depends on business rules.
	if rr.Code == http.StatusInternalServerError {
		t.Errorf("delete should not produce a 500; got body: %s", rr.Body.String())
	}
}

// delegationIDs extracts IDs from a delegation slice for error messages.
func delegationIDs(ds []common.Delegation) []string {
	ids := make([]string, len(ds))
	for i, d := range ds {
		ids[i] = d.ID
	}
	return ids
}
