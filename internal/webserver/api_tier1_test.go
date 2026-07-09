package webserver_test

// Tier 1 model-integrity guard tests. Broader lifecycle + resource-usage
// scenarios are covered by the end-to-end scenario suite (todo o29).

import (
	"net/http"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/webserver"
)

// o10: a negative quota value must be rejected (previously it passed quotaFits,
// auto-approved, lowered tracked pool usage, and -1 mapped to Nova "unlimited").
func TestCreateProject_NegativeQuotaRejected(t *testing.T) {
	h := setupRouter(t)
	body := webserver.CreateProjectRequest{
		Quota:               common.ProjectQuota{"cores": -1, "ram": 1, "storage": 1, "gpu": 0},
		Reason:              "negative quota",
		TerminationDate:     futureDate(30),
		FundingDelegationID: "dept_cs_students",
	}
	rr := do(t, h, http.MethodPost, "/v1/projects", userStudent, body)
	assertStatus(t, rr, http.StatusBadRequest)
}

// o14: approving a change_pending project must target the project's current
// funding delegation, else the "subtract current quota" capacity math is applied
// to the wrong pool. req_003 (change_pending) is funded by dept_cs_faculty;
// userBio is an admin of the unrelated dept_bio and tries to approve it there.
func TestApproveProject_ChangeApprovalWrongDelegationRejected(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodPost, "/v1/projects/req_003/approve", userBio,
		webserver.ApproveProjectRequest{DelegationID: "group:dept_bio"})
	assertStatus(t, rr, http.StatusBadRequest)
}
