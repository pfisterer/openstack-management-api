package webserver_test

// End-to-end scenario against the mock: builds a realistic DHBW-shaped budget
// tree via the API as different roles, drives the full node lifecycle (leaf AND
// budget requests), and verifies resource-usage rollup at EVERY level of the tree.
//
// Actor → role (tokens resolved by DummyAuthMiddleware from the 5 mock identities):
//   userRoot    group:root_uni          — root admin (admin scope of the root node)
//   userCSAdmin group:dept_cs_admin     — CS site/department admin
//   userFaculty group:dept_cs_faculty   — CS lecturer
//   userStudent group:cs-student        — student (consumer, holds NO admin scope)
//   userBio     group:dept_bio          — parallel branch (isolation/negative)
//
// All limits use only "cores" to keep the rollup arithmetic legible.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/tree"
)

func cores(n int) common.ProjectQuota { return common.ProjectQuota{"cores": n} }

// ── request helpers ───────────────────────────────────────────────────────────

func postBudget(t *testing.T, h http.Handler, actor string, req tree.CreateNodeRequest) *httptest.ResponseRecorder {
	t.Helper()
	req.Kind = tree.KindBudget
	if req.Reason == "" {
		req.Reason = "scenario"
	}
	return do(t, h, http.MethodPost, "/v1/nodes", actor, req)
}

// mkBudget creates a budget and asserts it comes back approved (manager path).
func mkBudget(t *testing.T, h http.Handler, actor string, req tree.CreateNodeRequest) string {
	t.Helper()
	rr := postBudget(t, h, actor, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create budget %q as %s: want 201, got %d: %s", req.Name, actor, rr.Code, rr.Body.String())
	}
	var n tree.Node
	mustDecode(t, rr, &n)
	if n.Status != tree.StatusApproved {
		t.Fatalf("budget %q created by manager should be approved, got %q", req.Name, n.Status)
	}
	return n.ID
}

// mkLeaf submits a project request and returns (id, status).
func mkLeaf(t *testing.T, h http.Handler, actor, parentID string, c int) (id, status string) {
	t.Helper()
	rr := do(t, h, http.MethodPost, "/v1/nodes", actor, tree.CreateNodeRequest{
		ParentID:        parentID,
		Kind:            tree.KindProject,
		Name:            "scenario leaf",
		Reason:          "scenario",
		Limit:           cores(c),
		TerminationDate: ptrStr(futureDate(90)),
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create leaf (%d cores) as %s under %s: want 201, got %d: %s", c, actor, parentID, rr.Code, rr.Body.String())
	}
	var n tree.Node
	mustDecode(t, rr, &n)
	return n.ID, n.Status
}

func approveNode(t *testing.T, h http.Handler, actor, nodeID string) {
	t.Helper()
	rr := do(t, h, http.MethodPost, "/v1/nodes/"+nodeID+"/approve", actor, tree.ApproveNodeRequest{})
	if rr.Code != http.StatusOK {
		t.Fatalf("approve %s as %s: want 200, got %d: %s", nodeID, actor, rr.Code, rr.Body.String())
	}
}

// usageCores reads a budget's rolled-up cores usage as seen by its admin
// (my-budgets attaches the subtree usage). For budgets the caller does not
// directly administer, pass an actor who does.
func usageCores(t *testing.T, h http.Handler, admin, budgetID string) int {
	t.Helper()
	rr := do(t, h, http.MethodGet, "/v1/nodes/my-budgets", admin, nil)
	assertStatus(t, rr, http.StatusOK)
	ns := decodePage(t, rr)
	for _, n := range ns {
		if n.ID == budgetID {
			return n.Usage.Total(quotaResourceIDs)["cores"]
		}
	}
	t.Fatalf("budget %s not visible to %s via my-budgets (visible: %v)", budgetID, admin, nodeIDs(ns))
	return -1
}

func assertUsage(t *testing.T, h http.Handler, admin, budgetID string, want int) {
	t.Helper()
	if got := usageCores(t, h, admin, budgetID); got != want {
		t.Errorf("usage(%s) as %s = %d cores, want %d", budgetID, admin, got, want)
	}
}

func ptrStr(s string) *string { return &s }

// ── the scenario ──────────────────────────────────────────────────────────────

func TestScenario_DHBWTreeLifecycle(t *testing.T) {
	// Start from the bootstrapped structural nodes only — the entire tree is
	// built through the API.
	h := setupRouterSeeded(t, nil)

	// ── Phase 1: build the tree in different roles ────────────────────────────

	// Root carves the university budget from the (unlimited) root node and keeps
	// it root-managed; departments may request budgets under it.
	dhbw := mkBudget(t, h, userRoot, tree.CreateNodeRequest{
		ParentID:           tree.RootNodeID,
		Name:               "DHBW",
		Limit:              cores(100),
		AdminScope:         common.TokenList{"group:root_uni"},
		EligibleRequesters: common.TokenList{"group:dept_cs_admin", "group:dept_bio"},
	})

	csPool := mkBudget(t, h, userRoot, tree.CreateNodeRequest{
		ParentID:           dhbw,
		Name:               "CS Standort",
		Limit:              cores(40),
		AdminScope:         common.TokenList{"group:dept_cs_admin"},
		EligibleRequesters: common.TokenList{"group:dept_cs_faculty"},
	})
	bioPool := mkBudget(t, h, userRoot, tree.CreateNodeRequest{
		ParentID:   dhbw,
		Name:       "Bio Standort",
		Limit:      cores(30),
		AdminScope: common.TokenList{"group:dept_bio"},
	})

	// child limit must not exceed parent
	if rr := postBudget(t, h, userRoot, tree.CreateNodeRequest{
		ParentID: dhbw, Name: "TooBig", Limit: cores(200), AdminScope: common.TokenList{"group:x"},
	}); rr.Code != http.StatusBadRequest {
		t.Errorf("child limit 200 > parent 100 should be 400, got %d", rr.Code)
	}
	// only a manager (or eligible requester) of the parent may create under it
	if rr := postBudget(t, h, userBio, tree.CreateNodeRequest{
		ParentID: csPool, Name: "Sneaky", Limit: cores(5), AdminScope: common.TokenList{"group:cs-student"},
	}); rr.Code != http.StatusForbidden {
		t.Errorf("bio admin under CS pool should be 403, got %d", rr.Code)
	}

	csFacPool := mkBudget(t, h, userCSAdmin, tree.CreateNodeRequest{
		ParentID:           csPool,
		Name:               "CS Fakultaet",
		Limit:              cores(20),
		AdminScope:         common.TokenList{"group:dept_cs_faculty"},
		EligibleRequesters: common.TokenList{"group:dept_cs_faculty"},
	})
	if rr := postBudget(t, h, userCSAdmin, tree.CreateNodeRequest{
		ParentID: csPool, Name: "OverParent", Limit: cores(50), AdminScope: common.TokenList{"group:x"},
	}); rr.Code != http.StatusBadRequest {
		t.Errorf("child limit 50 > CS pool 40 should be 400, got %d", rr.Code)
	}

	// Student auto-approve: managed by faculty, consumable by students. The
	// per-requester auto-approve cap (2) and the total limit (10) are independent.
	studBudget := mkBudget(t, h, userFaculty, tree.CreateNodeRequest{
		ParentID:           csFacPool,
		Name:               "CS Studi-Selfservice",
		Limit:              cores(10),
		AdminScope:         common.TokenList{"group:dept_cs_faculty"},
		EligibleRequesters: common.TokenList{"group:cs-student"},
		AutoApprove:        &tree.AutoApprove{PerRequesterLimit: cores(2)},
	})

	// checkAll asserts the rolled-up cores usage reported at EVERY level of the
	// tree (each queried as a direct admin of that level) after a mutation.
	// Asserting the Bio branch = 0 also pins the cross-branch sum.
	checkAll := func(stud, fac, cs, bio, uni int) {
		t.Helper()
		assertUsage(t, h, userFaculty, studBudget, stud)
		assertUsage(t, h, userFaculty, csFacPool, fac)
		assertUsage(t, h, userCSAdmin, csPool, cs)
		assertUsage(t, h, userBio, bioPool, bio)
		assertUsage(t, h, userRoot, dhbw, uni)
	}

	// ── Phase 2: auto-approve + cumulative per-requester cap + rollup ─────────
	_, st := mkLeaf(t, h, userStudent, studBudget, 2)
	if st != tree.StatusApproved {
		t.Fatalf("P1 (2 cores, within per-requester cap) should auto-approve, got %q", st)
	}
	checkAll(2, 2, 2, 0, 2) // rollup up the whole chain

	// a second request by the same owner exceeds the cumulative cap (2+1 > 2) → pending
	p2, st := mkLeaf(t, h, userStudent, studBudget, 1)
	if st != tree.StatusPending {
		t.Fatalf("P2 over cumulative per-requester cap should stay pending, got %q", st)
	}
	checkAll(2, 2, 2, 0, 2) // pending consumes nothing

	// F-2 regression: a consumer (student) holds no admin scope — they can
	// neither approve their own over-cap request nor even list the budget's children.
	if rr := do(t, h, http.MethodPost, "/v1/nodes/"+p2+"/approve", userStudent, tree.ApproveNodeRequest{}); rr.Code != http.StatusForbidden {
		t.Errorf("student approving own over-cap request should be 403, got %d: %s", rr.Code, rr.Body.String())
	}
	if rr := do(t, h, http.MethodGet, "/v1/nodes/"+studBudget+"/children", userStudent, nil); rr.Code != http.StatusForbidden {
		t.Errorf("student listing auto-approve children should be 403, got %d", rr.Code)
	}

	// The budget's manager (faculty) approves it — beyond the auto cap, but within
	// the budget's total limit.
	approveNode(t, h, userFaculty, p2)
	checkAll(3, 3, 3, 0, 3)

	// ── Phase 2b: within the per-requester cap but an ancestor is full ────────
	// poolB (limit 3) filled to 2; an auto-approve budget (cap 3) sits under it.
	// A 2-core request fits the cap (0+2<=3) but not poolB's remaining capacity
	// (2+2>3) → not auto-approved. The fill is released afterwards.
	poolB := mkBudget(t, h, userRoot, tree.CreateNodeRequest{
		ParentID: dhbw, Name: "Pool B", Limit: cores(3),
		AdminScope:         common.TokenList{"group:dept_cs_admin"},
		EligibleRequesters: common.TokenList{"group:dept_cs_faculty"},
	})
	allowB := mkBudget(t, h, userCSAdmin, tree.CreateNodeRequest{
		ParentID: poolB, Name: "Selfservice B", Limit: cores(3),
		AdminScope:         common.TokenList{"group:dept_cs_admin"},
		EligibleRequesters: common.TokenList{"group:cs-student"},
		AutoApprove:        &tree.AutoApprove{PerRequesterLimit: cores(3)},
	})
	fill, _ := mkLeaf(t, h, userFaculty, poolB, 2)
	approveNode(t, h, userCSAdmin, fill) // poolB now uses 2 of 3
	if _, st = mkLeaf(t, h, userStudent, allowB, 2); st != tree.StatusPending {
		t.Fatalf("student request (cap ok, ancestor full) should stay pending, got %q", st)
	}
	if rr := do(t, h, http.MethodPost, "/v1/nodes/"+fill+"/release", userFaculty, nil); rr.Code != http.StatusOK {
		t.Fatalf("release fill: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	checkAll(3, 3, 3, 0, 3) // Phase 2b left no residual usage

	// ── Phase 3: pool request + approve / capacity-reject / release ───────────
	p3, st := mkLeaf(t, h, userFaculty, csPool, 10)
	if st != tree.StatusPending {
		t.Fatalf("pool leaf P3 should be pending (manual approval), got %q", st)
	}
	approveNode(t, h, userCSAdmin, p3)
	checkAll(3, 3, 13, 0, 13) // csPool = 10 direct + 3 subtree; dhbw rolls up

	// a request that would overbook the pool cannot be approved
	p4, _ := mkLeaf(t, h, userFaculty, csPool, 100)
	if rr := do(t, h, http.MethodPost, "/v1/nodes/"+p4+"/approve", userCSAdmin, tree.ApproveNodeRequest{}); rr.Code != http.StatusBadRequest {
		t.Errorf("approving 100 cores into a 40-core pool should be 400 (capacity), got %d: %s", rr.Code, rr.Body.String())
	}
	if rr := do(t, h, http.MethodPost, "/v1/nodes/"+p4+"/reject", userCSAdmin, nil); rr.Code != http.StatusOK {
		t.Fatalf("reject P4 as manager: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	checkAll(3, 3, 13, 0, 13) // reject changed nothing

	// owner releases P3 → capacity returns
	if rr := do(t, h, http.MethodPost, "/v1/nodes/"+p3+"/release", userFaculty, nil); rr.Code != http.StatusOK {
		t.Fatalf("release P3 as owner: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	checkAll(3, 3, 3, 0, 3)

	// ── Phase 4: change request counts the OLD limit until re-approval ────────
	p5, _ := mkLeaf(t, h, userFaculty, csPool, 5)
	approveNode(t, h, userCSAdmin, p5)
	checkAll(3, 3, 8, 0, 8) // 5 (P5) + 3 subtree

	// owner proposes cores 5 → 8 (change_pending)
	newLimit := cores(8)
	if rr := do(t, h, http.MethodPost, "/v1/nodes/"+p5+"/request-change", userFaculty,
		tree.ChangeNodeRequest{Limit: &newLimit}); rr.Code != http.StatusOK {
		t.Fatalf("change-request P5 as owner: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	checkAll(3, 3, 8, 0, 8) // still the OLD 5 until re-approved

	// Rejecting the change returns the node to APPROVED with the old limit —
	// the previously approved state stays valid (no change_rejected zombie).
	if rr := do(t, h, http.MethodPost, "/v1/nodes/"+p5+"/reject", userCSAdmin, nil); rr.Code != http.StatusOK {
		t.Fatalf("reject change on P5: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var p5Node tree.Node
	rr := do(t, h, http.MethodGet, "/v1/nodes/"+p5, userFaculty, nil)
	assertStatus(t, rr, http.StatusOK)
	mustDecode(t, rr, &p5Node)
	if p5Node.Status != tree.StatusApproved || p5Node.Limit["cores"] != 5 {
		t.Fatalf("after change-reject P5 should be approved with 5 cores, got %q / %d", p5Node.Status, p5Node.Limit["cores"])
	}
	checkAll(3, 3, 8, 0, 8)

	// propose again and approve → the pending limit applies
	if rr := do(t, h, http.MethodPost, "/v1/nodes/"+p5+"/request-change", userFaculty,
		tree.ChangeNodeRequest{Limit: &newLimit}); rr.Code != http.StatusOK {
		t.Fatalf("second change-request P5: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	approveNode(t, h, userCSAdmin, p5)
	checkAll(3, 3, 11, 0, 11) // now 8 + 3

	// ── Phase 5: budget REQUESTS flow through the same lifecycle ──────────────
	// The CS admin asks root for an expansion budget under the university node
	// (the old model could not express this). Root approves with a modified limit.
	rr = postBudget(t, h, userCSAdmin, tree.CreateNodeRequest{
		ParentID:   dhbw,
		Name:       "CS Expansion",
		Reason:     "more students next semester",
		Limit:      cores(30),
		AdminScope: common.TokenList{"group:dept_cs_admin"},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("budget request as CS admin: want 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var expansion tree.Node
	mustDecode(t, rr, &expansion)
	if expansion.Status != tree.StatusPending {
		t.Fatalf("budget request by an eligible non-manager should be pending, got %q", expansion.Status)
	}
	// The requesting admin cannot approve their own budget request (parent-chain rule).
	if rr := do(t, h, http.MethodPost, "/v1/nodes/"+expansion.ID+"/approve", userCSAdmin, tree.ApproveNodeRequest{}); rr.Code != http.StatusForbidden {
		t.Errorf("CS admin approving own budget request should be 403, got %d", rr.Code)
	}
	modified := cores(20)
	if rr := do(t, h, http.MethodPost, "/v1/nodes/"+expansion.ID+"/approve", userRoot,
		tree.ApproveNodeRequest{ModifiedLimit: &modified}); rr.Code != http.StatusOK {
		t.Fatalf("root approving budget request: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	// Budget limits are caps, not consumption — usage is unchanged.
	checkAll(3, 3, 11, 0, 11)
	// … and the CS admin can now create under the new budget.
	leafX, _ := mkLeaf(t, h, userCSAdmin, expansion.ID, 4)
	var leafXNode tree.Node
	rr = do(t, h, http.MethodGet, "/v1/nodes/"+leafX, userCSAdmin, nil)
	assertStatus(t, rr, http.StatusOK)
	mustDecode(t, rr, &leafXNode)
	if leafXNode.Status != tree.StatusApproved {
		t.Fatalf("leaf by budget manager should be approved directly, got %q", leafXNode.Status)
	}
	assertUsage(t, h, userRoot, dhbw, 15) // 11 + 4

	// ── Phase 6: reparent needs BOTH sides; usage follows the move ────────────
	// Faculty alone cannot push their leaf into the Bio branch.
	if rr := do(t, h, http.MethodPost, "/v1/nodes/"+p5+"/reparent", userFaculty,
		tree.ReparentNodeRequest{NewParentID: bioPool}); rr.Code != http.StatusForbidden {
		t.Errorf("faculty reparenting into bio should be 403, got %d", rr.Code)
	}
	// Root manages both sides.
	if rr := do(t, h, http.MethodPost, "/v1/nodes/"+p5+"/reparent", userRoot,
		tree.ReparentNodeRequest{NewParentID: bioPool}); rr.Code != http.StatusOK {
		t.Fatalf("root reparenting P5 into bio: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	checkAll(3, 3, 3, 8, 15) // 8 moved from CS to Bio; university total unchanged

	// ── Phase 7: owner transfer ───────────────────────────────────────────────
	// Bio admin hands the moved leaf to the student; the student may then release it.
	if rr := do(t, h, http.MethodPost, "/v1/nodes/"+p5+"/transfer-owner", userBio,
		tree.TransferOwnerRequest{NewOwner: userStudent}); rr.Code != http.StatusOK {
		t.Fatalf("transfer owner as bio admin: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if rr := do(t, h, http.MethodPost, "/v1/nodes/"+p5+"/release", userStudent, nil); rr.Code != http.StatusOK {
		t.Fatalf("release by new owner: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	checkAll(3, 3, 3, 0, 7) // Bio back to 0; university = 3 (CS subtree) + 4 (expansion)
}
