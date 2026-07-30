package webserver_test

// Node CRUD / lifecycle / authorization tests against the mock tree
// (DefaultMockTreeState): root → {unassigned, b_dept_cs → b_cs_faculty →
// b_cs_students(auto), b_dept_bio, b_cs_expansion(pending)} plus leaves
// p_001 (approved), p_002 (pending), p_003 (change_pending), p_004 (approved, bio),
// p_imported_001 (imported under unassigned).

import (
	"net/http"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/tree"
)

// ── Views ─────────────────────────────────────────────────────────────────────

func TestViews_Mine(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodGet, "/v1/nodes/mine", userFaculty, nil)
	assertStatus(t, rr, http.StatusOK)
	var nodes []tree.Node
	mustDecode(t, rr, &nodes)
	want := map[string]bool{"p_001": true, "p_003": true}
	if len(nodes) != len(want) {
		t.Fatalf("faculty should own exactly %d leaves, got %v", len(want), nodeIDs(nodes))
	}
	for _, n := range nodes {
		if !want[n.ID] {
			t.Errorf("unexpected leaf in mine: %s", n.ID)
		}
	}
}

func TestViews_ToManage(t *testing.T) {
	h := setupRouter(t)

	// Faculty administers b_cs_faculty + b_cs_students → sees the pending and
	// change_pending leaves under them, but NOT the pending budget under root.
	rr := do(t, h, http.MethodGet, "/v1/nodes/to-manage", userFaculty, nil)
	assertStatus(t, rr, http.StatusOK)
	var nodes []tree.Node
	mustDecode(t, rr, &nodes)
	got := map[string]bool{}
	for _, n := range nodes {
		got[n.ID] = true
	}
	if !got["p_002"] || !got["p_003"] || got["b_cs_expansion"] || got["p_imported_001"] {
		t.Errorf("faculty to-manage wrong: %v", nodeIDs(nodes))
	}

	// Root administers the root node → sees everything pending/imported via subtree.
	rr = do(t, h, http.MethodGet, "/v1/nodes/to-manage", userRoot, nil)
	assertStatus(t, rr, http.StatusOK)
	nodes = nil
	mustDecode(t, rr, &nodes)
	got = map[string]bool{}
	for _, n := range nodes {
		got[n.ID] = true
	}
	for _, want := range []string{"p_002", "p_003", "b_cs_expansion", "p_imported_001"} {
		if !got[want] {
			t.Errorf("root to-manage should contain %s, got %v", want, nodeIDs(nodes))
		}
	}

	// The student administers nothing.
	rr = do(t, h, http.MethodGet, "/v1/nodes/to-manage", userStudent, nil)
	assertStatus(t, rr, http.StatusOK)
	nodes = nil
	mustDecode(t, rr, &nodes)
	if len(nodes) != 0 {
		t.Errorf("student to-manage should be empty, got %v", nodeIDs(nodes))
	}
}

func TestViews_EligibleForMe(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodGet, "/v1/nodes/eligible-for-me", userStudent, nil)
	assertStatus(t, rr, http.StatusOK)
	var nodes []tree.Node
	mustDecode(t, rr, &nodes)
	// Students are eligible under the faculty pool and the self-service budget.
	got := map[string]bool{}
	for _, n := range nodes {
		got[n.ID] = true
	}
	if !got["b_cs_faculty"] || !got["b_cs_students"] || len(nodes) != 2 {
		t.Errorf("student eligible-for-me wrong: %v", nodeIDs(nodes))
	}
}

func TestViews_EligibleForOwner_RootOnly(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodGet, "/v1/nodes/eligible-for-owner?owner_token=group:cs-student", userFaculty, nil)
	assertStatus(t, rr, http.StatusForbidden)

	rr = do(t, h, http.MethodGet, "/v1/nodes/eligible-for-owner?owner_token=group:cs-student", userRoot, nil)
	assertStatus(t, rr, http.StatusOK)
	var nodes []tree.Node
	mustDecode(t, rr, &nodes)
	if len(nodes) != 2 {
		t.Errorf("eligible-for-owner(student) should list 2 budgets, got %v", nodeIDs(nodes))
	}
}

// ── Read authorization ────────────────────────────────────────────────────────

func TestGetNode_Authorization(t *testing.T) {
	h := setupRouter(t)

	// Owner reads their leaf.
	assertStatus(t, do(t, h, http.MethodGet, "/v1/nodes/p_001", userFaculty, nil), http.StatusOK)
	// Ancestor managers read it too (CS admin via b_dept_cs, root via root).
	assertStatus(t, do(t, h, http.MethodGet, "/v1/nodes/p_001", userCSAdmin, nil), http.StatusOK)
	assertStatus(t, do(t, h, http.MethodGet, "/v1/nodes/p_001", userRoot, nil), http.StatusOK)
	// A foreign branch admin does not.
	assertStatus(t, do(t, h, http.MethodGet, "/v1/nodes/p_001", userBio, nil), http.StatusForbidden)
	// The student cannot read the faculty's leaf either.
	assertStatus(t, do(t, h, http.MethodGet, "/v1/nodes/p_001", userStudent, nil), http.StatusForbidden)
	// Unknown ID → 404 (as a manager of everything, root gets the honest answer).
	assertStatus(t, do(t, h, http.MethodGet, "/v1/nodes/nope", userRoot, nil), http.StatusNotFound)
}

// ── Lifecycle guards ──────────────────────────────────────────────────────────

func TestLifecycle_Guards(t *testing.T) {
	h := setupRouter(t)

	// Approving an already-approved node is rejected.
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/p_001/approve", userCSAdmin, tree.ApproveNodeRequest{}), http.StatusBadRequest)
	// Releasing a pending node is rejected.
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/p_002/release", userFaculty, nil), http.StatusBadRequest)
	// Releasing a budget is rejected.
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/b_cs_faculty/release", userCSAdmin, nil), http.StatusBadRequest)

	// Rejecting a pending leaf is terminal; nothing can revive it.
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/p_002/reject", userFaculty, nil), http.StatusOK)
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/p_002/approve", userFaculty, tree.ApproveNodeRequest{}), http.StatusBadRequest)
	limit := cores(1)
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/p_002/request-change", userStudent, tree.ChangeNodeRequest{Limit: &limit}), http.StatusBadRequest)
}

func TestLifecycle_ImportedIsReadOnly(t *testing.T) {
	h := setupRouter(t)
	limit := cores(1)
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/p_imported_001/request-change", userRoot, tree.ChangeNodeRequest{Limit: &limit}), http.StatusForbidden)
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/p_imported_001/approve", userRoot, tree.ApproveNodeRequest{}), http.StatusBadRequest)
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/p_imported_001/release", userRoot, nil), http.StatusBadRequest)
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/p_imported_001/reparent", userRoot, tree.ReparentNodeRequest{NewParentID: "b_dept_cs"}), http.StatusForbidden)
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/p_imported_001/transfer-owner", userRoot, tree.TransferOwnerRequest{NewOwner: userFaculty}), http.StatusForbidden)
}

// ── Validation ────────────────────────────────────────────────────────────────

func TestValidation_LeafLimits(t *testing.T) {
	h := setupRouter(t)

	// Negative limits and unknown resources are rejected on create.
	rr := do(t, h, http.MethodPost, "/v1/nodes", userStudent, tree.CreateNodeRequest{
		ParentID: "b_cs_students", Kind: tree.KindProject, Reason: "x",
		Limit: common.ProjectQuota{"cores": -1},
	})
	assertStatus(t, rr, http.StatusBadRequest)

	rr = do(t, h, http.MethodPost, "/v1/nodes", userStudent, tree.CreateNodeRequest{
		ParentID: "b_cs_students", Kind: tree.KindProject, Reason: "x",
		Limit: common.ProjectQuota{"warp_cores": 1},
	})
	assertStatus(t, rr, http.StatusBadRequest)

	// Approve with a negative modified limit is rejected.
	neg := common.ProjectQuota{"cores": -5}
	rr = do(t, h, http.MethodPost, "/v1/nodes/p_002/approve", userFaculty, tree.ApproveNodeRequest{ModifiedLimit: &neg})
	assertStatus(t, rr, http.StatusBadRequest)
}

// ── Direct budget edits ───────────────────────────────────────────────────────

func TestUpdateNode_PolicyVsCapacityAuthz(t *testing.T) {
	h := setupRouter(t)

	// The budget's own manager may edit policy fields …
	newEligible := common.TokenList{"group:cs-student", "user:extra@cs.example"}
	rr := do(t, h, http.MethodPut, "/v1/nodes/b_cs_students", userFaculty, tree.UpdateNodeRequest{
		EligibleRequesters: &newEligible,
	})
	assertStatus(t, rr, http.StatusOK)

	// … but NOT their own limit (that is the parent chain's decision).
	// Note: a budget limit is a complete map — missing resources mean 0.
	biggerLimit := common.ProjectQuota{"cores": 25, "ram": 80, "storage": 500, "gpu": 2}
	rr = do(t, h, http.MethodPut, "/v1/nodes/b_cs_faculty", userFaculty, tree.UpdateNodeRequest{Limit: &biggerLimit})
	assertStatus(t, rr, http.StatusForbidden)

	// The parent-chain manager can change the limit — within the parent's cap.
	rr = do(t, h, http.MethodPut, "/v1/nodes/b_cs_faculty", userCSAdmin, tree.UpdateNodeRequest{Limit: &biggerLimit})
	assertStatus(t, rr, http.StatusOK)

	// Raising a child limit beyond the parent's limit is rejected (F-5 regression).
	tooBig := cores(999)
	rr = do(t, h, http.MethodPut, "/v1/nodes/b_cs_faculty", userCSAdmin, tree.UpdateNodeRequest{Limit: &tooBig})
	assertStatus(t, rr, http.StatusBadRequest)

	// Shrinking below the subtree's active usage is rejected.
	tiny := common.ProjectQuota{"cores": 1, "ram": 1, "storage": 1, "gpu": 0}
	rr = do(t, h, http.MethodPut, "/v1/nodes/b_cs_faculty", userCSAdmin, tree.UpdateNodeRequest{Limit: &tiny})
	assertStatus(t, rr, http.StatusBadRequest)

	// The root node's admin scope is owned by the configuration.
	scope := common.TokenList{"group:evil"}
	rr = do(t, h, http.MethodPut, "/v1/nodes/root", userRoot, tree.UpdateNodeRequest{AdminScope: &scope})
	assertStatus(t, rr, http.StatusForbidden)

	// Leaves cannot be edited directly.
	name := "nope"
	rr = do(t, h, http.MethodPut, "/v1/nodes/p_001", userFaculty, tree.UpdateNodeRequest{Name: &name})
	assertStatus(t, rr, http.StatusBadRequest)
}

// ── Delete guards ─────────────────────────────────────────────────────────────

func TestDeleteNode_Guards(t *testing.T) {
	h := setupRouter(t)

	// Structural nodes are protected.
	assertStatus(t, do(t, h, http.MethodDelete, "/v1/nodes/root", userRoot, nil), http.StatusForbidden)
	assertStatus(t, do(t, h, http.MethodDelete, "/v1/nodes/unassigned", userRoot, nil), http.StatusForbidden)

	// A subtree with active/pending leaves cannot be deleted.
	assertStatus(t, do(t, h, http.MethodDelete, "/v1/nodes/b_dept_cs", userRoot, nil), http.StatusBadRequest)

	// Leaves are not deleted via the API.
	assertStatus(t, do(t, h, http.MethodDelete, "/v1/nodes/p_001", userFaculty, nil), http.StatusBadRequest)

	// An empty budget CAN be deleted by its manager (inclusive scope).
	id := mkBudget(t, h, userCSAdmin, tree.CreateNodeRequest{
		ParentID: "b_dept_cs", Name: "Temp", Limit: cores(1),
		AdminScope: common.TokenList{"group:dept_cs_admin"},
	})
	assertStatus(t, do(t, h, http.MethodDelete, "/v1/nodes/"+id, userCSAdmin, nil), http.StatusNoContent)
}

// ── Promotion ─────────────────────────────────────────────────────────────────

func TestPromote_Flow(t *testing.T) {
	h := setupRouter(t)

	// Only root (manager of the unassigned chain) may promote — the target-side
	// manager alone is not enough.
	req := tree.PromoteNodeRequest{
		NewParentID: "b_cs_faculty",
		Owner:       userFaculty,
		Reason:      "adopt legacy workload",
	}
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/p_imported_001/promote", userFaculty, req), http.StatusForbidden)

	// Promotion into the unassigned node itself is nonsense.
	bad := req
	bad.NewParentID = tree.UnassignedNodeID
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/p_imported_001/promote", userRoot, bad), http.StatusBadRequest)

	// A promotion exceeding the target's capacity is rejected up front.
	tooBig := req
	tooBig.Limit = cores(100) // faculty pool limit is 20
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/p_imported_001/promote", userRoot, tooBig), http.StatusBadRequest)

	// Root promotes with a fitting limit override.
	ok := req
	ok.Limit = common.ProjectQuota{"cores": 5, "ram": 16, "storage": 100, "gpu": 0}
	rr := do(t, h, http.MethodPost, "/v1/nodes/p_imported_001/promote", userRoot, ok)
	assertStatus(t, rr, http.StatusOK)
	var n tree.Node
	mustDecode(t, rr, &n)
	if n.Status != tree.StatusImported {
		t.Errorf("status should remain imported until the reconciler tags the OS project, got %q", n.Status)
	}
	if n.ParentID == nil || *n.ParentID != "b_cs_faculty" || n.Owner != "user:"+userFaculty {
		t.Errorf("promotion should set parent + owner, got parent=%v owner=%q", n.ParentID, n.Owner)
	}
	found := false
	for _, f := range n.Flags {
		if f == tree.FlagPromoteOnReconcile {
			found = true
		}
	}
	if !found {
		t.Errorf("promotion should set the promote flag, flags=%v", n.Flags)
	}

	// Only imported leaves can be promoted.
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/p_001/promote", userRoot, req), http.StatusForbidden)
}

// ── Owner transfer ────────────────────────────────────────────────────────────

func TestTransferOwner(t *testing.T) {
	h := setupRouter(t)

	// Owning a leaf does not grant transfer rights (parent-chain managers only):
	// the student owns p_002 but holds no admin scope anywhere.
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/p_002/transfer-owner", userStudent,
		tree.TransferOwnerRequest{NewOwner: userBio}), http.StatusForbidden)
	// A foreign-branch admin cannot transfer either.
	req := tree.TransferOwnerRequest{NewOwner: userStudent}
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/p_001/transfer-owner", userBio, req), http.StatusForbidden)

	// The parent-chain manager can.
	rr := do(t, h, http.MethodPost, "/v1/nodes/p_001/transfer-owner", userCSAdmin, req)
	assertStatus(t, rr, http.StatusOK)
	var n tree.Node
	mustDecode(t, rr, &n)
	if n.Owner != "user:"+userStudent {
		t.Errorf("owner should be the student, got %q", n.Owner)
	}

	// The new owner sees it under "mine" and may act on it; the old owner lost control.
	rr = do(t, h, http.MethodGet, "/v1/nodes/mine", userStudent, nil)
	assertStatus(t, rr, http.StatusOK)
	var mine []tree.Node
	mustDecode(t, rr, &mine)
	found := false
	for _, m := range mine {
		if m.ID == "p_001" {
			found = true
		}
	}
	if !found {
		t.Errorf("p_001 should appear in the student's mine view, got %v", nodeIDs(mine))
	}
	// A user who is neither owner nor a manager of the parent chain cannot release.
	// (The old owner, faculty, retains release rights — not as owner, but as the
	// budget's manager via the parent chain.)
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/p_001/release", userBio, nil), http.StatusForbidden)
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/p_001/release", userStudent, nil), http.StatusOK)
}

// ── F-4 regression: no foreign adoption of pending requests ───────────────────

func TestApprove_ForeignAdminCannotAdoptPendingRequest(t *testing.T) {
	h := setupRouter(t)
	// p_002 is pending under b_cs_faculty. The bio admin manages a different
	// branch and must not be able to approve (or reject) it, even knowing the ID.
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/p_002/approve", userBio, tree.ApproveNodeRequest{}), http.StatusForbidden)
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/p_002/reject", userBio, nil), http.StatusForbidden)
	// Moving it into their own branch also fails: they do not manage the CURRENT parent chain.
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/p_002/reparent", userBio, tree.ReparentNodeRequest{NewParentID: "b_dept_bio"}), http.StatusForbidden)
}

// ── F-3 regression: root can decide everywhere via the ancestor rule ──────────

func TestApprove_RootActsViaAncestorRule(t *testing.T) {
	h := setupRouter(t)
	// Root approves the pending student leaf deep in the CS branch — no special
	// bypass, just the admin scope of the root node.
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/p_002/approve", userRoot, tree.ApproveNodeRequest{}), http.StatusOK)
	// And decides the pending BUDGET request directly under the university root.
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/b_cs_expansion/approve", userRoot, tree.ApproveNodeRequest{}), http.StatusOK)
}
