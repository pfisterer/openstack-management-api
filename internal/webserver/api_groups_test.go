package webserver_test

// o26: group search + /groups/mine. The MockRoleProvider exposes the group
// tokens from DefaultMockResourceState: group:root_uni, group:dept_cs_admin,
// group:dept_cs_faculty, group:dept_bio, group:cs-student.

import (
	"net/http"
	"slices"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/webserver"
)

func TestSearchGroups_MatchesQuery(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodGet, "/v1/groups/search?q=cs", userFaculty, nil)
	assertStatus(t, rr, http.StatusOK)

	var resp webserver.TokenListResponse
	mustDecode(t, rr, &resp)

	for _, want := range []string{"group:dept_cs_admin", "group:dept_cs_faculty", "group:cs-student"} {
		if !slices.Contains(resp.Tokens, want) {
			t.Errorf("search q=cs should include %s; got %v", want, resp.Tokens)
		}
	}
	if slices.Contains(resp.Tokens, "group:dept_bio") {
		t.Errorf("search q=cs should not include group:dept_bio; got %v", resp.Tokens)
	}
}

func TestSearchGroups_LimitClamps(t *testing.T) {
	h := setupRouter(t)
	// empty query matches all 5 groups; limit caps the result.
	rr := do(t, h, http.MethodGet, "/v1/groups/search?limit=2", userFaculty, nil)
	assertStatus(t, rr, http.StatusOK)

	var resp webserver.TokenListResponse
	mustDecode(t, rr, &resp)
	if len(resp.Tokens) != 2 {
		t.Errorf("limit=2 should return 2 tokens, got %d (%v)", len(resp.Tokens), resp.Tokens)
	}
}

func TestListMyGroups_ReturnsCallerTokens(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodGet, "/v1/groups/mine", userFaculty, nil)
	assertStatus(t, rr, http.StatusOK)

	var resp webserver.TokenListResponse
	mustDecode(t, rr, &resp)
	if !slices.Contains(resp.Tokens, "group:dept_cs_faculty") {
		t.Errorf("faculty's groups should include group:dept_cs_faculty; got %v", resp.Tokens)
	}
}
