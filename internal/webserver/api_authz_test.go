package webserver_test

// Negative authorization tests for the mutating endpoints hardened in Tier 0.
// Each exercises a caller who is NOT in the required admin_scope / not the
// requester, and asserts 403. On the pre-hardening code (no authz check) these
// same requests returned 200/204, so these tests lock the fix in place.
//
// Delegation hierarchy (see api_delegations_test.go):
//   group:root_uni
//   ├── group:dept_cs_admin      AdminScope=[group:dept_cs_admin]
//   │   └── group:dept_cs_faculty  AdminScope=[group:dept_cs_faculty]
//   │       └── dept_cs_students   AdminScope=[group:cs-student]
//   └── group:dept_bio           AdminScope=[group:dept_bio]
// userBio holds group:dept_bio (unrelated to the CS-funded projects);
// userStudent holds group:cs-student (unrelated to dept_bio / dept_cs_admin).

import (
	"net/http"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/webserver"
)

// ── Delegations ───────────────────────────────────────────────────────────────

func TestDeleteDelegation_NonManagerForbidden(t *testing.T) {
	h := setupRouter(t)
	// student is not in dept_bio's admin_scope chain (dept_bio -> root_uni).
	rr := do(t, h, http.MethodDelete, "/v1/delegations/group:dept_bio", userStudent, nil)
	assertStatus(t, rr, http.StatusForbidden)
}

func TestUpdateDelegation_NonManagerForbidden(t *testing.T) {
	h := setupRouter(t)
	newName := "hijacked"
	rr := do(t, h, http.MethodPut, "/v1/delegations/group:dept_bio", userStudent,
		webserver.UpdateDelegationRequest{Name: &newName})
	assertStatus(t, rr, http.StatusForbidden)
}

func TestCreateDelegation_NonParentManagerForbidden(t *testing.T) {
	h := setupRouter(t)
	parent := "group:dept_bio"
	body := webserver.CreateDelegationRequest{
		Name:               "carve-out",
		AdminScope:         common.TokenList{"group:cs-student"},
		DelegationStrategy: common.DelegationStrategyPool,
		Quota: common.ProjectResources{
			Limit: common.ProjectQuota{"cores": 1, "ram": 1, "storage": 1, "gpu": 0},
		},
		ParentID:    &parent,
		CanDelegate: false,
	}
	// student is not in the parent's (dept_bio) admin_scope chain.
	rr := do(t, h, http.MethodPost, "/v1/delegations", userStudent, body)
	assertStatus(t, rr, http.StatusForbidden)
}

// ── Projects ──────────────────────────────────────────────────────────────────
// req_001 (approved) and req_003 (change_pending) are both funded by
// group:dept_cs_faculty. userBio (group:dept_bio) is neither their requester nor
// a manager of their funding delegation, and is not a root admin.

func TestUpdateProject_NonRequesterForbidden(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodPut, "/v1/projects/req_001", userBio,
		webserver.UpdateProjectRequest{})
	assertStatus(t, rr, http.StatusForbidden)
}

func TestRejectProject_NonManagerForbidden(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodPost, "/v1/projects/req_003/reject", userBio, nil)
	assertStatus(t, rr, http.StatusForbidden)
}

func TestReleaseProject_NonManagerForbidden(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodPost, "/v1/projects/req_001/release", userBio, nil)
	assertStatus(t, rr, http.StatusForbidden)
}
