package webserver_test

// Role-switch and impersonation tests. The allowlist is rootAdminTokens, exactly
// as app.go wires it.

import (
	"net/http"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/tree"
)

type roleSwitchState struct {
	Enabled            bool     `json:"enabled"`
	Allowed            bool     `json:"allowed"`
	EffectiveTokens    []string `json:"effective_tokens"`
	OverrideGroupToken *string  `json:"override_group_token"`
	ImpersonatedUser   *string  `json:"impersonated_user"`
}

func TestRoleSwitch_OnlyAllowlistedUsers(t *testing.T) {
	h := setupRouter(t)

	// A non-root user may read the state but not switch.
	rr := do(t, h, http.MethodGet, "/v1/role-switch", userFaculty, nil)
	assertStatus(t, rr, http.StatusOK)
	var st roleSwitchState
	mustDecode(t, rr, &st)
	if st.Allowed {
		t.Errorf("faculty should not be allowed to role-switch")
	}
	assertStatus(t, do(t, h, http.MethodPut, "/v1/role-switch", userFaculty,
		map[string]string{"group_token": "group:root_uni"}), http.StatusForbidden)
}

func TestRoleSwitch_GroupOverride(t *testing.T) {
	h := setupRouter(t)

	// Root switches into the faculty group…
	rr := do(t, h, http.MethodPut, "/v1/role-switch", userRoot,
		map[string]string{"group_token": "group:dept_cs_faculty"})
	assertStatus(t, rr, http.StatusOK)

	// …and now manages the faculty pool but no longer the whole tree — BUT the
	// user token survives a group override, so the root user token still grants
	// root rights. Verify the effective tokens contain the switched group.
	var st roleSwitchState
	rr = do(t, h, http.MethodGet, "/v1/role-switch", userRoot, nil)
	assertStatus(t, rr, http.StatusOK)
	mustDecode(t, rr, &st)
	found := false
	for _, tok := range st.EffectiveTokens {
		if tok == "group:dept_cs_faculty" {
			found = true
		}
		if tok == "group:root_uni" {
			t.Errorf("group override should drop the original group tokens, got %v", st.EffectiveTokens)
		}
	}
	if !found {
		t.Errorf("effective tokens should contain the override group, got %v", st.EffectiveTokens)
	}

	// Clear restores the original state.
	assertStatus(t, do(t, h, http.MethodDelete, "/v1/role-switch", userRoot, nil), http.StatusOK)
	rr = do(t, h, http.MethodGet, "/v1/role-switch", userRoot, nil)
	mustDecode(t, rr, &st)
	if st.OverrideGroupToken != nil || st.ImpersonatedUser != nil {
		t.Errorf("override should be cleared, got %+v", st)
	}
}

func TestRoleSwitch_ImpersonationDropsRoot(t *testing.T) {
	h := setupRouter(t)

	// Root fully impersonates the student.
	rr := do(t, h, http.MethodPut, "/v1/role-switch", userRoot,
		map[string]string{"impersonate_user": userStudent})
	assertStatus(t, rr, http.StatusOK)

	// Email-scoped views follow the assumed user: "mine" shows the student's
	// leaves (p_002 in the mock seed).
	rr = do(t, h, http.MethodGet, "/v1/nodes/mine", userRoot, nil)
	assertStatus(t, rr, http.StatusOK)
	mine := decodePage(t, rr)
	if len(mine) != 1 || mine[0].ID != "p_002" {
		t.Errorf("impersonated student should own exactly [p_002], got %v", nodeIDs(mine))
	}

	// Faithful impersonation: the actor LOSES root management rights — the
	// student cannot decide the pending leaf, so neither can root-as-student.
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/p_002/approve", userRoot, tree.ApproveNodeRequest{}), http.StatusForbidden)

	// But auto-approve works exactly as it would for the student.
	rr = do(t, h, http.MethodPost, "/v1/nodes", userRoot, tree.CreateNodeRequest{
		ParentID: "b_cs_students", Kind: tree.KindProject, Name: "impersonated request",
		Reason: "impersonated request", Limit: cores(1),
	})
	assertStatus(t, rr, http.StatusCreated)
	var n tree.Node
	mustDecode(t, rr, &n)
	if n.Status != tree.StatusApproved {
		t.Errorf("impersonated request should auto-approve, got %q", n.Status)
	}
	if n.Owner != "user:"+userStudent {
		t.Errorf("owner should be the impersonated student, got %q", n.Owner)
	}

	// Clearing the switch restores root rights.
	assertStatus(t, do(t, h, http.MethodDelete, "/v1/role-switch", userRoot, nil), http.StatusOK)
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/p_002/approve", userRoot, tree.ApproveNodeRequest{}), http.StatusOK)
}
