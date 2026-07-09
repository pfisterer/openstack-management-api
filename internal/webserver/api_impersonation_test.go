package webserver_test

import (
	"net/http"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

// TestRoleSwitchImpersonation exercises full identity impersonation: a root admin
// assumes a mock user and then sees exactly that user's projects (email-scoped
// views follow the assumed identity); clearing restores the root context.
func TestRoleSwitchImpersonation(t *testing.T) {
	h := setupRouter(t)

	// Root can list assumable identities, and they include the mock users.
	rr := do(t, h, http.MethodGet, "/v1/role-switch/identities", userRoot, nil)
	assertStatus(t, rr, http.StatusOK)
	var idResp struct {
		Identities []common.Identity `json:"identities"`
	}
	mustDecode(t, rr, &idResp)
	foundFaculty := false
	for _, id := range idResp.Identities {
		if id.Email == userFaculty {
			foundFaculty = true
		}
	}
	if !foundFaculty {
		t.Fatalf("expected %s among assumable identities, got %d entries", userFaculty, len(idResp.Identities))
	}

	// A caller who cannot role-switch may not list assumable identities.
	rr = do(t, h, http.MethodGet, "/v1/role-switch/identities", userStudent, nil)
	assertStatus(t, rr, http.StatusForbidden)

	// Root owns no projects of their own.
	rr = do(t, h, http.MethodGet, "/v1/projects/mine", userRoot, nil)
	assertStatus(t, rr, http.StatusOK)
	var rootProjects []common.Project
	mustDecode(t, rr, &rootProjects)
	if len(rootProjects) != 0 {
		t.Fatalf("expected root to own no projects, got %v", projectIDs(rootProjects))
	}

	// Root impersonates faculty@cs.example.
	rr = do(t, h, http.MethodPut, "/v1/role-switch", userRoot, map[string]string{"impersonate_user": userFaculty})
	assertStatus(t, rr, http.StatusOK)

	// While impersonating, "my projects" for the root caller are faculty's.
	rr = do(t, h, http.MethodGet, "/v1/projects/mine", userRoot, nil)
	assertStatus(t, rr, http.StatusOK)
	var asFaculty []common.Project
	mustDecode(t, rr, &asFaculty)
	if len(asFaculty) < 2 {
		t.Fatalf("expected faculty's ≥2 projects while impersonating, got %v", projectIDs(asFaculty))
	}
	for _, p := range asFaculty {
		owned := false
		for _, tok := range p.RequesterTokens {
			if tok == "user:"+userFaculty {
				owned = true
			}
		}
		if !owned {
			t.Errorf("impersonated view returned %s which faculty does not own", p.ID)
		}
	}

	// Impersonating an unknown identity is rejected.
	rr = do(t, h, http.MethodPut, "/v1/role-switch", userRoot, map[string]string{"impersonate_user": "nobody@nowhere.example"})
	assertStatus(t, rr, http.StatusBadRequest)

	// Clearing restores the root context (no owned projects again).
	rr = do(t, h, http.MethodDelete, "/v1/role-switch", userRoot, nil)
	assertStatus(t, rr, http.StatusOK)
	rr = do(t, h, http.MethodGet, "/v1/projects/mine", userRoot, nil)
	assertStatus(t, rr, http.StatusOK)
	var afterClear []common.Project
	mustDecode(t, rr, &afterClear)
	if len(afterClear) != 0 {
		t.Fatalf("expected no projects after clearing impersonation, got %v", projectIDs(afterClear))
	}
}
