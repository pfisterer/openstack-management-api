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
	nodes := decodePage(t, rr)
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
	nodes := decodePage(t, rr)
	got := map[string]bool{}
	for _, n := range nodes {
		got[n.ID] = true
	}
	if !got["p_002"] || !got["p_003"] || got["b_cs_expansion"] || got["p_imported_001"] {
		t.Errorf("faculty to-manage wrong: %v", nodeIDs(nodes))
	}

	// Root administers the root node. By default it sees only what nobody else is
	// responsible for: the import under the (unmanaged) imports node, but not the
	// requests inside the delegated department budgets.
	rr = do(t, h, http.MethodGet, "/v1/nodes/to-manage", userRoot, nil)
	assertStatus(t, rr, http.StatusOK)
	nodes = decodePage(t, rr)
	got = map[string]bool{}
	for _, n := range nodes {
		got[n.ID] = true
	}
	if !got["p_imported_001"] || got["p_002"] || got["p_003"] || got["b_cs_expansion"] {
		t.Errorf("root to-manage (direct) wrong: %v", nodeIDs(nodes))
	}

	// scope=subtree answers the other question — everything below root.
	rr = do(t, h, http.MethodGet, "/v1/nodes/to-manage?scope=subtree", userRoot, nil)
	assertStatus(t, rr, http.StatusOK)
	nodes = decodePage(t, rr)
	got = map[string]bool{}
	for _, n := range nodes {
		got[n.ID] = true
	}
	for _, want := range []string{"p_002", "p_003", "b_cs_expansion", "p_imported_001"} {
		if !got[want] {
			t.Errorf("root to-manage (subtree) should contain %s, got %v", want, nodeIDs(nodes))
		}
	}

	// An unknown scope is a client error, not a silently different list.
	rr = do(t, h, http.MethodGet, "/v1/nodes/to-manage?scope=everything", userRoot, nil)
	assertStatus(t, rr, http.StatusBadRequest)

	// The student administers nothing.
	rr = do(t, h, http.MethodGet, "/v1/nodes/to-manage", userStudent, nil)
	assertStatus(t, rr, http.StatusOK)
	nodes = decodePage(t, rr)
	if len(nodes) != 0 {
		t.Errorf("student to-manage should be empty, got %v", nodeIDs(nodes))
	}
}

func TestViews_EligibleForMe(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodGet, "/v1/nodes/eligible-for-me", userStudent, nil)
	assertStatus(t, rr, http.StatusOK)
	nodes := decodePage(t, rr)
	// Students are eligible under the faculty pool and the auto-approve budget.
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
	nodes := decodePage(t, rr)
	if len(nodes) != 2 {
		t.Errorf("eligible-for-owner(student) should list 2 budgets, got %v", nodeIDs(nodes))
	}
}

// ── Pagination ────────────────────────────────────────────────────────────────

// Every page carries the number of matches it was cut from. Without it a client
// cannot tell a complete list from a truncated one — which is how the budget
// tree used to drop everything past its page size without saying so.
func TestPagination_ChildrenReportTotal(t *testing.T) {
	h := setupRouter(t)

	rr := do(t, h, http.MethodGet, "/v1/nodes/b_cs_faculty/children", userFaculty, nil)
	assertStatus(t, rr, http.StatusOK)
	all := decodePage(t, rr)

	rr = do(t, h, http.MethodGet, "/v1/nodes/b_cs_faculty/children?limit=1", userFaculty, nil)
	assertStatus(t, rr, http.StatusOK)
	var first tree.NodePage
	mustDecode(t, rr, &first)
	if len(first.Items) != 1 {
		t.Fatalf("limit=1 should return one child, got %v", nodeIDs(first.Items))
	}
	if first.Total != len(all) {
		t.Errorf("total should count every child (%d), got %d", len(all), first.Total)
	}
	if first.Limit != 1 || first.Offset != 0 {
		t.Errorf("the page should echo its bounds, got limit=%d offset=%d", first.Limit, first.Offset)
	}

	// The next page continues where the first ended, so paging through a budget
	// visits every child exactly once.
	rr = do(t, h, http.MethodGet, "/v1/nodes/b_cs_faculty/children?limit=1&offset=1", userFaculty, nil)
	assertStatus(t, rr, http.StatusOK)
	second := decodePage(t, rr)
	if len(second) != 1 || second[0].ID == first.Items[0].ID {
		t.Errorf("offset=1 should return the next child, got %v after %v",
			nodeIDs(second), nodeIDs(first.Items))
	}
}

// ── Search ────────────────────────────────────────────────────────────────────

// Search replaces the client-side filter the paginated tree can no longer do.
// It must stay inside what the caller manages: the faculty's search finds their
// own ML project, not the import hanging under the root's unassigned node.
func TestSearchNodes_ScopedToManagedSubtree(t *testing.T) {
	h := setupRouter(t)

	rr := do(t, h, http.MethodGet, "/v1/nodes/search?q=ml+workload", userFaculty, nil)
	assertStatus(t, rr, http.StatusOK)
	found := decodePage(t, rr)
	ids := map[string]bool{}
	for _, n := range found {
		ids[n.ID] = true
	}
	if !ids["p_003"] {
		t.Errorf("faculty should find their own ML project, got %v", nodeIDs(found))
	}
	if ids["p_imported_001"] {
		t.Errorf("faculty must not see the import under the root, got %v", nodeIDs(found))
	}

	// The root administers everything, so the same query reaches the import.
	rr = do(t, h, http.MethodGet, "/v1/nodes/search?q=legacy-ml-workload", userRoot, nil)
	assertStatus(t, rr, http.StatusOK)
	found = decodePage(t, rr)
	if len(found) != 1 || found[0].ID != "p_imported_001" {
		t.Errorf("root should find the import by its OpenStack name, got %v", nodeIDs(found))
	}
}

// Matching goes beyond the label the row shows: people search by owner and by
// the OpenStack project too.
func TestSearchNodes_MatchesOwnerAndOpenStackProject(t *testing.T) {
	h := setupRouter(t)

	rr := do(t, h, http.MethodGet, "/v1/nodes/search?q=os-project-abc-123", userRoot, nil)
	assertStatus(t, rr, http.StatusOK)
	if found := decodePage(t, rr); len(found) != 1 || found[0].ID != "p_imported_001" {
		t.Errorf("searching the OpenStack project ID should find the import, got %v", nodeIDs(found))
	}

	// An email finds what that person owns — and, deliberately, what they were
	// granted access to: "where does this person appear?" is the question a
	// manager actually asks.
	rr = do(t, h, http.MethodGet, "/v1/nodes/search?q="+userStudent, userFaculty, nil)
	assertStatus(t, rr, http.StatusOK)
	found := decodePage(t, rr)
	ids := map[string]bool{}
	for _, n := range found {
		ids[n.ID] = true
	}
	if !ids["p_002"] {
		t.Errorf("searching a person's email should find the project they own, got %v", nodeIDs(found))
	}
}

// An empty query would mean "everything", which is what the tree is for.
func TestSearchNodes_RejectsEmptyQuery(t *testing.T) {
	h := setupRouter(t)
	assertStatus(t, do(t, h, http.MethodGet, "/v1/nodes/search?q=%20%20", userFaculty, nil),
		http.StatusBadRequest)
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
	// The student is a read-only member of p_001 in the seed and therefore reads it.
	assertStatus(t, do(t, h, http.MethodGet, "/v1/nodes/p_001", userStudent, nil), http.StatusOK)
	// On a leaf they have no relation to, the same student is denied.
	assertStatus(t, do(t, h, http.MethodGet, "/v1/nodes/p_004", userStudent, nil), http.StatusForbidden)
	// Unknown ID → 404 (as a manager of everything, root gets the honest answer).
	assertStatus(t, do(t, h, http.MethodGet, "/v1/nodes/nope", userRoot, nil), http.StatusNotFound)
}

// ── Lifecycle guards ──────────────────────────────────────────────────────────

func TestLifecycle_Guards(t *testing.T) {
	h := setupRouter(t)

	// A status guard is a CONFLICT (409), not a bad request: the caller sent a
	// well-formed request against a node that had moved on. The client can tell
	// this apart from "your input is wrong" and reload instead.

	// Approving an already-approved node is rejected.
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/p_001/approve", userCSAdmin, tree.ApproveNodeRequest{}), http.StatusConflict)
	// Releasing a pending node is rejected.
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/p_002/release", userFaculty, nil), http.StatusConflict)
	// Releasing a budget stays a 400: no status makes a budget releasable, so
	// this is a malformed request, not a stale view.
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/b_cs_faculty/release", userCSAdmin, nil), http.StatusBadRequest)

	// Rejecting a pending leaf is terminal; nothing can revive it.
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/p_002/reject", userFaculty, nil), http.StatusOK)
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/p_002/approve", userFaculty, tree.ApproveNodeRequest{}), http.StatusConflict)
	limit := cores(1)
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/p_002/request-change", userStudent, tree.ChangeNodeRequest{Limit: &limit}), http.StatusConflict)
}

func TestLifecycle_ImportedIsReadOnly(t *testing.T) {
	h := setupRouter(t)
	limit := cores(1)
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/p_imported_001/request-change", userRoot, tree.ChangeNodeRequest{Limit: &limit}), http.StatusForbidden)
	// These two have no dedicated "imported" check and fall through to the
	// status guard, which now answers 409 (the imported status is what blocks
	// them). The explicit checks above stay 403.
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/p_imported_001/approve", userRoot, tree.ApproveNodeRequest{}), http.StatusConflict)
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/p_imported_001/release", userRoot, nil), http.StatusConflict)
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/p_imported_001/reparent", userRoot, tree.ReparentNodeRequest{NewParentID: "b_dept_cs"}), http.StatusForbidden)
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/p_imported_001/transfer-owner", userRoot, tree.TransferOwnerRequest{NewOwner: userFaculty}), http.StatusForbidden)
}

// ── Validation ────────────────────────────────────────────────────────────────

func TestValidation_LeafLimits(t *testing.T) {
	h := setupRouter(t)

	// Negative limits and unknown resources are rejected on create.
	rr := do(t, h, http.MethodPost, "/v1/nodes", userStudent, tree.CreateNodeRequest{
		ParentID: "b_cs_students", Kind: tree.KindProject, Name: "x", Reason: "x",
		Limit: common.ProjectQuota{"cores": -1},
	})
	assertStatus(t, rr, http.StatusBadRequest)

	rr = do(t, h, http.MethodPost, "/v1/nodes", userStudent, tree.CreateNodeRequest{
		ParentID: "b_cs_students", Kind: tree.KindProject, Name: "x", Reason: "x",
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

	// A leaf accepts exactly one direct edit: its owner renaming it.
	name := "renamed"
	rr = do(t, h, http.MethodPut, "/v1/nodes/p_001", userFaculty, tree.UpdateNodeRequest{Name: &name})
	assertStatus(t, rr, http.StatusOK)

	// Anything else on a leaf still goes through request-change, even when a
	// rename rides along.
	rr = do(t, h, http.MethodPut, "/v1/nodes/p_001", userFaculty, tree.UpdateNodeRequest{Name: &name, Limit: &tiny})
	assertStatus(t, rr, http.StatusBadRequest)

	// Renaming somebody else's project is not a stranger's business.
	rr = do(t, h, http.MethodPut, "/v1/nodes/p_001", userBio, tree.UpdateNodeRequest{Name: &name})
	assertStatus(t, rr, http.StatusForbidden)
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
	mine := decodePage(t, rr)
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
