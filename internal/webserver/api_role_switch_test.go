package webserver_test

// o24: /v1/role-switch GET/PUT/DELETE. The allowlist is rootAdminTokens, so only
// userRoot may set/clear an override. Setting one replaces group tokens in
// effective_tokens; clearing restores the originals.

import (
	"net/http"
	"slices"
	"testing"
)

// local mirror of the (unexported) roleSwitchStateResponse for decoding.
type roleSwitchState struct {
	Allowed            bool     `json:"allowed"`
	EffectiveTokens    []string `json:"effective_tokens"`
	OverrideGroupToken *string  `json:"override_group_token"`
}

func TestRoleSwitch_GetAllowedReflectsRootAdmin(t *testing.T) {
	h := setupRouter(t)

	rr := do(t, h, http.MethodGet, "/v1/role-switch", userRoot, nil)
	assertStatus(t, rr, http.StatusOK)
	var st roleSwitchState
	mustDecode(t, rr, &st)
	if !st.Allowed {
		t.Error("root admin should be allowed to role-switch")
	}

	rr = do(t, h, http.MethodGet, "/v1/role-switch", userFaculty, nil)
	assertStatus(t, rr, http.StatusOK)
	mustDecode(t, rr, &st)
	if st.Allowed {
		t.Error("non-root user must not be allowed to role-switch")
	}
}

func TestRoleSwitch_SetForbiddenForNonRoot(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodPut, "/v1/role-switch", userFaculty,
		map[string]string{"group_token": "group:dept_bio"})
	assertStatus(t, rr, http.StatusForbidden)
}

func TestRoleSwitch_SetThenClearForRoot(t *testing.T) {
	h := setupRouter(t)

	// set override
	rr := do(t, h, http.MethodPut, "/v1/role-switch", userRoot,
		map[string]string{"group_token": "group:dept_bio"})
	assertStatus(t, rr, http.StatusOK)

	// GET shows the override applied to effective_tokens
	rr = do(t, h, http.MethodGet, "/v1/role-switch", userRoot, nil)
	var st roleSwitchState
	mustDecode(t, rr, &st)
	if st.OverrideGroupToken == nil || *st.OverrideGroupToken != "group:dept_bio" {
		t.Fatalf("override_group_token = %v, want group:dept_bio", st.OverrideGroupToken)
	}
	if !slices.Contains(st.EffectiveTokens, "group:dept_bio") {
		t.Errorf("effective_tokens should contain the override group:dept_bio; got %v", st.EffectiveTokens)
	}

	// clear restores originals
	rr = do(t, h, http.MethodDelete, "/v1/role-switch", userRoot, nil)
	assertStatus(t, rr, http.StatusOK)
	rr = do(t, h, http.MethodGet, "/v1/role-switch", userRoot, nil)
	mustDecode(t, rr, &st)
	if st.OverrideGroupToken != nil {
		t.Errorf("override should be cleared; got %v", *st.OverrideGroupToken)
	}
	if !slices.Contains(st.EffectiveTokens, "group:root_uni") {
		t.Errorf("effective_tokens should be restored to originals (group:root_uni); got %v", st.EffectiveTokens)
	}
}
