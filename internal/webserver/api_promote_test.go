package webserver_test

// Promote endpoint contract (POST /v1/projects/{id}/promote):
//
//   - Only root admins may call it; anyone else gets 403.
//   - The funding delegation must be in the *owner's* eligible delegations (derived from
//     req.OwnerTokens), not the caller's.
//   - The selected delegation (and its pool ancestors) must have enough remaining capacity
//     for the project's current quota; otherwise 400.
//   - On success: status stays openstack_only, promote_on_reconcile flag is set,
//     funded_by / reason / termination_date are updated.
//
// Mock quota for osonly_001: {cores:9, ...}
//   dept_cs_admin  limit=30, used=12 → 18 free → fits (12+9=21 ≤ 30)
//   dept_cs_faculty limit=20, used=12 →  8 free → too small (12+9=21 > 20)
//
// Faculty owner tokens: [user:faculty@cs.example, group:dept_cs_faculty]
//   eligible for dept_cs_admin (via eligibility rule) → happy path uses dept_cs_admin
//
// Student owner tokens: [user:cs-student@cs.com, group:cs-student]
//   eligible for dept_cs_faculty (via eligibility rule) → capacity-exceeded test uses dept_cs_faculty

import (
	"net/http"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/webserver"
)

const osOnlyID = "osonly_001"

// facultyOwnerTokens mirrors mock identity mock_cs_faculty.
var facultyOwnerTokens = common.TokenList{"user:faculty@cs.example", "group:dept_cs_faculty"}

// studentOwnerTokens mirrors mock identity mock_cs_student.
var studentOwnerTokens = common.TokenList{"user:cs-student@cs.com", "group:cs-student"}

// validPromoteBody returns a correct PromoteProjectRequest for the happy path:
// root admin promoting osonly_001 on behalf of faculty, funded by dept_cs_admin.
func validPromoteBody() webserver.PromoteProjectRequest {
	return webserver.PromoteProjectRequest{
		OwnerTokens:         facultyOwnerTokens,
		FundingDelegationID: "group:dept_cs_admin",
		Reason:              "Adopt existing OS project",
		TerminationDate:     futureDate(90),
		AuthorizedUsers:     []common.AuthorizedUser{{Token: "user:faculty@cs.example", OpenstackRole: "admin"}},
	}
}

// ── Successful promotion ──────────────────────────────────────────────────────

func TestPromote_RootAdminSucceeds(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodPost, "/v1/projects/"+osOnlyID+"/promote", userRoot, validPromoteBody())
	assertStatus(t, rr, http.StatusOK)
}

func TestPromote_StatusRemainsOpenstackOnly(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodPost, "/v1/projects/"+osOnlyID+"/promote", userRoot, validPromoteBody())
	assertStatus(t, rr, http.StatusOK)

	var p common.Project
	mustDecode(t, rr, &p)
	if p.Status != common.ProjectStatusOpenStackOnly {
		t.Errorf("status should remain openstack_only after promote, got %q", p.Status)
	}
}

func TestPromote_PromoteOnReconcileFlagIsSet(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodPost, "/v1/projects/"+osOnlyID+"/promote", userRoot, validPromoteBody())
	assertStatus(t, rr, http.StatusOK)

	var p common.Project
	mustDecode(t, rr, &p)

	found := false
	for _, f := range p.Flags {
		if f == common.ProjectFlagPromoteOnReconcile {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected %q flag; got flags %v", common.ProjectFlagPromoteOnReconcile, p.Flags)
	}
}

func TestPromote_FundingAndMetadataAreUpdated(t *testing.T) {
	h := setupRouter(t)
	body := validPromoteBody()
	rr := do(t, h, http.MethodPost, "/v1/projects/"+osOnlyID+"/promote", userRoot, body)
	assertStatus(t, rr, http.StatusOK)

	var p common.Project
	mustDecode(t, rr, &p)

	if p.FundedBy == nil || *p.FundedBy != body.FundingDelegationID {
		t.Errorf("expected funded_by=%q, got %v", body.FundingDelegationID, p.FundedBy)
	}
	if p.Reason != body.Reason {
		t.Errorf("expected reason=%q, got %q", body.Reason, p.Reason)
	}
	if p.TerminationDate != body.TerminationDate {
		t.Errorf("expected termination_date=%q, got %q", body.TerminationDate, p.TerminationDate)
	}
}

func TestPromote_OwnerTokensBecomesRequesterTokens(t *testing.T) {
	h := setupRouter(t)
	body := validPromoteBody()
	rr := do(t, h, http.MethodPost, "/v1/projects/"+osOnlyID+"/promote", userRoot, body)
	assertStatus(t, rr, http.StatusOK)

	var p common.Project
	mustDecode(t, rr, &p)

	set := common.NewTokenSet(p.RequesterTokens)
	for _, tok := range body.OwnerTokens {
		if !set.Contains(tok) {
			t.Errorf("expected owner token %q in requester_tokens; got %v", tok, p.RequesterTokens)
		}
	}
}

// ── Authorization ─────────────────────────────────────────────────────────────

func TestPromote_NonRootAdminFails(t *testing.T) {
	h := setupRouter(t)
	// faculty is not a root admin
	rr := do(t, h, http.MethodPost, "/v1/projects/"+osOnlyID+"/promote", userFaculty, validPromoteBody())
	assertStatus(t, rr, http.StatusForbidden)
}

func TestPromote_CSAdminFails(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodPost, "/v1/projects/"+osOnlyID+"/promote", userCSAdmin, validPromoteBody())
	assertStatus(t, rr, http.StatusForbidden)
}

// ── Wrong project status ──────────────────────────────────────────────────────

func TestPromote_NonOpenstackOnlyProjectFails(t *testing.T) {
	h := setupRouter(t)
	// req_001 is approved — only openstack_only projects may be promoted
	rr := do(t, h, http.MethodPost, "/v1/projects/req_001/promote", userRoot, validPromoteBody())
	assertStatus(t, rr, http.StatusForbidden)
}

// ── Not found / bad input ─────────────────────────────────────────────────────

func TestPromote_NotFoundReturns404(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodPost, "/v1/projects/does-not-exist/promote", userRoot, validPromoteBody())
	assertStatus(t, rr, http.StatusNotFound)
}

func TestPromote_MissingRequiredFieldsReturns400(t *testing.T) {
	h := setupRouter(t)
	// Empty body — all required fields absent
	rr := do(t, h, http.MethodPost, "/v1/projects/"+osOnlyID+"/promote", userRoot,
		webserver.PromoteProjectRequest{})
	assertStatus(t, rr, http.StatusBadRequest)
}

// ── Owner eligibility ─────────────────────────────────────────────────────────

func TestPromote_DelegationNotInOwnerEligibleSetFails(t *testing.T) {
	h := setupRouter(t)
	// bio faculty tokens have no eligibility for group:dept_cs_admin
	body := webserver.PromoteProjectRequest{
		OwnerTokens:         common.TokenList{"user:faculty@bio.example", "group:dept_bio"},
		FundingDelegationID: "group:dept_cs_admin",
		Reason:              "cross-dept attempt",
		TerminationDate:     futureDate(90),
		AuthorizedUsers:     []common.AuthorizedUser{{Token: "user:faculty@bio.example", OpenstackRole: "admin"}},
	}
	rr := do(t, h, http.MethodPost, "/v1/projects/"+osOnlyID+"/promote", userRoot, body)
	assertStatus(t, rr, http.StatusBadRequest)
}

// ── Capacity check ────────────────────────────────────────────────────────────

func TestPromote_CapacityExceededFails(t *testing.T) {
	h := setupRouter(t)
	// student tokens → eligible for dept_cs_faculty (via eligibility rule for deptCSFaculty owner token).
	// dept_cs_faculty: limit=20 cores, used=12 → 8 free.
	// osonly_001 quota = 9 cores → 12+9=21 > 20 → should fail.
	body := webserver.PromoteProjectRequest{
		OwnerTokens:         studentOwnerTokens,
		FundingDelegationID: "group:dept_cs_faculty",
		Reason:              "capacity test",
		TerminationDate:     futureDate(90),
		AuthorizedUsers:     []common.AuthorizedUser{{Token: "user:cs-student@cs.com", OpenstackRole: "member"}},
	}
	rr := do(t, h, http.MethodPost, "/v1/projects/"+osOnlyID+"/promote", userRoot, body)
	assertStatus(t, rr, http.StatusBadRequest)
}

// ── Quota override ────────────────────────────────────────────────────────────

func TestPromote_QuotaOverrideIsApplied(t *testing.T) {
	h := setupRouter(t)
	// Override osonly_001's existing quota (9 cores) with a smaller value (5 cores).
	// dept_cs_admin has 18 cores free, so 5 fits easily.
	body := validPromoteBody()
	body.Quota = common.ProjectQuota{"cores": 5, "ram": 8, "storage": 50, "gpu": 0}
	rr := do(t, h, http.MethodPost, "/v1/projects/"+osOnlyID+"/promote", userRoot, body)
	assertStatus(t, rr, http.StatusOK)

	var p common.Project
	mustDecode(t, rr, &p)
	if p.Quota["cores"] != 5 {
		t.Errorf("expected overridden cores=5, got %d", p.Quota["cores"])
	}
}

func TestPromote_QuotaOverrideOmittedKeepsExistingQuota(t *testing.T) {
	h := setupRouter(t)
	// No quota in the request → project keeps its existing quota (9 cores).
	rr := do(t, h, http.MethodPost, "/v1/projects/"+osOnlyID+"/promote", userRoot, validPromoteBody())
	assertStatus(t, rr, http.StatusOK)

	var p common.Project
	mustDecode(t, rr, &p)
	if p.Quota["cores"] != 9 {
		t.Errorf("expected original cores=9, got %d", p.Quota["cores"])
	}
}

func TestPromote_QuotaOverrideCapacityExceededFails(t *testing.T) {
	h := setupRouter(t)
	// Override to 20 cores against dept_cs_admin (18 free) → should fail.
	body := validPromoteBody()
	body.Quota = common.ProjectQuota{"cores": 20, "ram": 16, "storage": 100, "gpu": 0}
	rr := do(t, h, http.MethodPost, "/v1/projects/"+osOnlyID+"/promote", userRoot, body)
	assertStatus(t, rr, http.StatusBadRequest)
}
