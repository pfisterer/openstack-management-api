package webserver_test

// o29: end-to-end scenario against the mock. Builds a realistic DHBW-shaped
// delegation tree via the API as different roles, drives the full project
// lifecycle, and verifies resource-usage rollup at EVERY level of the tree.
//
// Actor → role (tokens resolved by DummyAuthMiddleware from the 5 mock identities):
//   userRoot    group:root_uni          — DHBW root admin
//   userCSAdmin group:dept_cs_admin     — CS site/department admin
//   userFaculty group:dept_cs_faculty   — CS lecturer
//   userStudent group:cs-student        — student
//   userBio     group:dept_bio          — parallel branch (isolation/negative)
//
// All quotas use only "cores" to keep the rollup arithmetic legible.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/webserver"
)

const (
	poolStrategy      = common.DelegationStrategyPool
	allowanceStrategy = common.DelegationStrategyAllowance
)

func cores(n int) common.ProjectQuota { return common.ProjectQuota{"cores": n} }

// ── request helpers ───────────────────────────────────────────────────────────

func postDelegation(t *testing.T, h http.Handler, actor, parentID, name, strategy string, scope common.TokenList, limit int, canDelegate bool) *httptest.ResponseRecorder {
	t.Helper()
	body := webserver.CreateDelegationRequest{
		Name:               name,
		AdminScope:         scope,
		DelegationStrategy: strategy,
		Quota:              common.ProjectResources{Limit: cores(limit)},
		ParentID:           &parentID,
		CanDelegate:        canDelegate,
	}
	return do(t, h, http.MethodPost, "/v1/delegations", actor, body)
}

func mkDelegation(t *testing.T, h http.Handler, actor, parentID, name, strategy string, scope common.TokenList, limit int, canDelegate bool) string {
	t.Helper()
	rr := postDelegation(t, h, actor, parentID, name, strategy, scope, limit, canDelegate)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create delegation %q as %s: want 201, got %d: %s", name, actor, rr.Code, rr.Body.String())
	}
	var d common.Delegation
	mustDecode(t, rr, &d)
	return d.ID
}

func mkProject(t *testing.T, h http.Handler, actor, fundingID string, c int) (id, status string) {
	t.Helper()
	body := webserver.CreateProjectRequest{
		Quota:               cores(c),
		Reason:              "scenario",
		TerminationDate:     futureDate(90),
		FundingDelegationID: fundingID,
	}
	rr := do(t, h, http.MethodPost, "/v1/projects", actor, body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create project (%d cores) as %s from %s: want 201, got %d: %s", c, actor, fundingID, rr.Code, rr.Body.String())
	}
	var p common.Project
	mustDecode(t, rr, &p)
	return p.ID, p.Status
}

func approve(t *testing.T, h http.Handler, actor, projectID, delegationID string) {
	t.Helper()
	rr := do(t, h, http.MethodPost, "/v1/projects/"+projectID+"/approve", actor,
		webserver.ApproveProjectRequest{DelegationID: delegationID})
	if rr.Code != http.StatusOK {
		t.Fatalf("approve %s as %s: want 200, got %d: %s", projectID, actor, rr.Code, rr.Body.String())
	}
}

func setEligibility(t *testing.T, h http.Handler, actor, ownerToken string, eligible common.TokenList) {
	t.Helper()
	rr := do(t, h, http.MethodPut, "/v1/eligibility/"+ownerToken, actor,
		webserver.SetEligibilityRuleRequest{EligibleRequesters: eligible})
	if rr.Code != http.StatusOK {
		t.Fatalf("set eligibility %s as %s: want 200, got %d: %s", ownerToken, actor, rr.Code, rr.Body.String())
	}
}

// usageCores reads a delegation's rolled-up cores usage as seen by its admin
// (delegated-to-me attaches the subtree usage).
func usageCores(t *testing.T, h http.Handler, admin, delegationID string) int {
	t.Helper()
	rr := do(t, h, http.MethodGet, "/v1/delegations/delegated-to-me", admin, nil)
	assertStatus(t, rr, http.StatusOK)
	var ds []common.Delegation
	mustDecode(t, rr, &ds)
	for _, d := range ds {
		if d.ID == delegationID {
			return d.Quota.UsageByStatus.TotalQuota(quotaResourceIDs)["cores"]
		}
	}
	t.Fatalf("delegation %s not visible to %s via delegated-to-me", delegationID, admin)
	return -1
}

func assertUsage(t *testing.T, h http.Handler, admin, delegationID string, want int) {
	t.Helper()
	if got := usageCores(t, h, admin, delegationID); got != want {
		t.Errorf("usage(%s) as %s = %d cores, want %d", delegationID, admin, got, want)
	}
}

// ── the scenario ──────────────────────────────────────────────────────────────

func TestScenario_DHBWDelegationLifecycle(t *testing.T) {
	rootID := "group:root_uni"
	root := common.Delegation{
		ID:                 rootID,
		Name:               "DHBW Root",
		CanDelegate:        true,
		DelegationStrategy: poolStrategy,
		AdminScope:         common.TokenList{"group:root_uni"},
		Quota:              common.ProjectResources{Limit: cores(100)},
	}
	h := setupRouterSeeded(t, []common.Delegation{root}, nil, nil)

	// ── Phase 1: build the tree in different roles ────────────────────────────
	csPool := mkDelegation(t, h, userRoot, rootID, "CS Standort", poolStrategy, common.TokenList{"group:dept_cs_admin"}, 40, true)
	bioPool := mkDelegation(t, h, userRoot, rootID, "Bio Standort", poolStrategy, common.TokenList{"group:dept_bio"}, 30, true)

	// child limit must not exceed parent
	if rr := postDelegation(t, h, userRoot, rootID, "TooBig", poolStrategy, common.TokenList{"group:x"}, 200, false); rr.Code != http.StatusBadRequest {
		t.Errorf("child limit 200 > parent 100 should be 400, got %d", rr.Code)
	}
	// only a manager of the parent scope may carve a sub-delegation
	if rr := postDelegation(t, h, userBio, csPool, "Sneaky", poolStrategy, common.TokenList{"group:cs-student"}, 5, false); rr.Code != http.StatusForbidden {
		t.Errorf("bio admin under CS pool should be 403, got %d", rr.Code)
	}

	csFacPool := mkDelegation(t, h, userCSAdmin, csPool, "CS Fakultaet", poolStrategy, common.TokenList{"group:dept_cs_faculty"}, 20, true)
	if rr := postDelegation(t, h, userCSAdmin, csPool, "OverParent", poolStrategy, common.TokenList{"group:x"}, 50, false); rr.Code != http.StatusBadRequest {
		t.Errorf("child limit 50 > CS pool 40 should be 400, got %d", rr.Code)
	}
	studAllow := mkDelegation(t, h, userFaculty, csFacPool, "CS Studi-Allowance", allowanceStrategy, common.TokenList{"group:cs-student"}, 2, false)

	// eligibility: faculty may request from any pool scoped to group:dept_cs_admin
	setEligibility(t, h, userCSAdmin, "group:dept_cs_admin", common.TokenList{"group:dept_cs_faculty"})

	// checkAll asserts the rolled-up cores usage reported at EVERY level of the
	// tree (each queried as that level's admin) after a mutation. Asserting the
	// Bio branch = 0 also pins the cross-branch sum: root == csPool + bio (+0).
	checkAll := func(studA, fac, cs, bio, rt int) {
		t.Helper()
		assertUsage(t, h, userStudent, studAllow, studA)
		assertUsage(t, h, userFaculty, csFacPool, fac)
		assertUsage(t, h, userCSAdmin, csPool, cs)
		assertUsage(t, h, userBio, bioPool, bio)
		assertUsage(t, h, userRoot, rootID, rt)
	}

	// ── Phase 2: allowance auto-approve + cumulative cap + usage rollup ────────
	_, st := mkProject(t, h, userStudent, studAllow, 2)
	if st != common.ProjectStatusApproved {
		t.Fatalf("P1 (2 cores, within allowance) should auto-approve, got %q", st)
	}
	checkAll(2, 2, 2, 0, 2) // rollup up the whole chain

	// a second student request exceeds the per-user cap (2+1 > 2) -> stays pending
	_, st = mkProject(t, h, userStudent, studAllow, 1)
	if st != common.ProjectStatusPending {
		t.Fatalf("P2 over per-user allowance should stay pending, got %q", st)
	}
	checkAll(2, 2, 2, 0, 2) // pending consumes nothing

	// ── Phase 2b: within the allowance cap but the ancestor pool is full ──────
	// poolB (limit 3, child<=parent ok) filled to 2 by a pool project; an
	// allowance (cap 3) sits under it. A 2-core student request fits the cap
	// (0+2<=3) but not the pool's remaining capacity (2+2>3) -> not auto-approved.
	// poolB is a sibling of csPool under root; the fill is released afterwards so
	// it does not pollute the root-level usage assertions below.
	poolB := mkDelegation(t, h, userRoot, rootID, "Pool B", poolStrategy, common.TokenList{"group:dept_cs_admin"}, 3, true)
	allowB := mkDelegation(t, h, userCSAdmin, poolB, "Allowance B", allowanceStrategy, common.TokenList{"group:cs-student"}, 3, false)
	fill, _ := mkProject(t, h, userFaculty, poolB, 2)
	approve(t, h, userCSAdmin, fill, poolB) // poolB now uses 2 of 3
	if _, st = mkProject(t, h, userStudent, allowB, 2); st != common.ProjectStatusPending {
		t.Fatalf("student request (cap ok, pool full) should stay pending, got %q", st)
	}
	if rr := do(t, h, http.MethodPost, "/v1/projects/"+fill+"/release", userFaculty, nil); rr.Code != http.StatusOK {
		t.Fatalf("release fill: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	checkAll(2, 2, 2, 0, 2) // Phase 2b left no residual usage

	// ── Phase 3: pool request + approve / capacity-reject / release ───────────
	p3, st := mkProject(t, h, userFaculty, csPool, 10)
	if st != common.ProjectStatusPending {
		t.Fatalf("pool project P3 should be pending (manual approval), got %q", st)
	}
	approve(t, h, userCSAdmin, p3, csPool)
	checkAll(2, 2, 12, 0, 12) // csPool = 10 direct + 2 child; root rolls up

	// a request that would overbook the pool cannot be approved
	p4, _ := mkProject(t, h, userFaculty, csPool, 100)
	if rr := do(t, h, http.MethodPost, "/v1/projects/"+p4+"/approve", userCSAdmin,
		webserver.ApproveProjectRequest{DelegationID: csPool}); rr.Code != http.StatusBadRequest {
		t.Errorf("approving 100 cores into a 40-core pool should be 400 (capacity), got %d: %s", rr.Code, rr.Body.String())
	}
	if rr := do(t, h, http.MethodPost, "/v1/projects/"+p4+"/reject", userCSAdmin, nil); rr.Code != http.StatusOK {
		t.Fatalf("reject P4 as manager: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	checkAll(2, 2, 12, 0, 12) // reject changed nothing

	// requester releases P3 -> capacity returns
	if rr := do(t, h, http.MethodPost, "/v1/projects/"+p3+"/release", userFaculty, nil); rr.Code != http.StatusOK {
		t.Fatalf("release P3 as requester: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	checkAll(2, 2, 2, 0, 2) // back to child rollup only

	// ── Phase 4: change-request counts old quota until re-approval ────────────
	p5, _ := mkProject(t, h, userFaculty, csPool, 5)
	approve(t, h, userCSAdmin, p5, csPool)
	checkAll(2, 2, 7, 0, 7) // 5 (P5) + 2 (child)

	// faculty proposes cores 5 -> 8 (change_pending)
	newQ := cores(8)
	if rr := do(t, h, http.MethodPut, "/v1/projects/"+p5, userFaculty,
		webserver.UpdateProjectRequest{Quota: &newQ}); rr.Code != http.StatusOK {
		t.Fatalf("change-request P5 as requester: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	checkAll(2, 2, 7, 0, 7) // still the OLD 5 until re-approved

	approve(t, h, userCSAdmin, p5, csPool) // re-approve applies the pending quota
	checkAll(2, 2, 10, 0, 10)              // now 8 + 2
}
