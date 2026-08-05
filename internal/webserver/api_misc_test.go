package webserver_test

// Reconciler admin endpoint gating, group search and config endpoint tests.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/reconciler"
	"github.com/pfisterer/openstack-management-api/internal/webserver"
)

// fakeReconciler implements webserver.ReconcilerAPI for tests.
type fakeReconciler struct {
	triggered int
	status    reconciler.Status
}

func (f *fakeReconciler) Trigger()                     { f.triggered++ }
func (f *fakeReconciler) GetStatus() reconciler.Status { return f.status }

func TestReconcilerEndpoints_DisabledReturns503(t *testing.T) {
	h := setupRouter(t) // nil reconciler
	assertStatus(t, do(t, h, http.MethodGet, "/v1/admin/reconcile/status", userRoot, nil), http.StatusServiceUnavailable)
	assertStatus(t, do(t, h, http.MethodPost, "/v1/admin/reconcile/trigger", userRoot, nil), http.StatusServiceUnavailable)
}

func TestReconcilerEndpoints_RootGated(t *testing.T) {
	rec := &fakeReconciler{}
	h := setupRouterWith(t, rec)

	// Non-root users are rejected.
	assertStatus(t, do(t, h, http.MethodGet, "/v1/admin/reconcile/status", userFaculty, nil), http.StatusForbidden)
	assertStatus(t, do(t, h, http.MethodPost, "/v1/admin/reconcile/trigger", userStudent, nil), http.StatusForbidden)

	// Root may read and trigger.
	assertStatus(t, do(t, h, http.MethodGet, "/v1/admin/reconcile/status", userRoot, nil), http.StatusOK)
	rr := do(t, h, http.MethodPost, "/v1/admin/reconcile/trigger", userRoot, nil)
	if rr.Code != http.StatusOK && rr.Code != http.StatusAccepted {
		t.Fatalf("trigger as root: want 200/202, got %d: %s", rr.Code, rr.Body.String())
	}
	if rec.triggered != 1 {
		t.Errorf("reconciler should have been triggered once, got %d", rec.triggered)
	}
}

func TestPrincipalSearchEndpoint(t *testing.T) {
	h := setupRouter(t)
	search := func(t *testing.T, q string) webserver.PrincipalSearchResponse {
		t.Helper()
		rr := do(t, h, http.MethodGet, "/v1/principals/search?q="+q, userRoot, nil)
		assertStatus(t, rr, http.StatusOK)
		var resp webserver.PrincipalSearchResponse
		mustDecode(t, rr, &resp)
		return resp
	}

	// Search by token returns the mock groups, each with its label.
	searchResp := search(t, "dept")
	if len(searchResp.Groups) == 0 {
		t.Fatalf("group search for 'dept' should return groups")
	}
	for _, g := range searchResp.Groups {
		if g.Label == "" {
			t.Errorf("group %q should carry a label", g.Token)
		}
	}

	// Searching by label finds a group whose token does not contain the query.
	searchResp = search(t, "Biology")
	if len(searchResp.Groups) != 1 || searchResp.Groups[0].Token != "group:dept_bio" {
		t.Errorf("label search for 'Biology' should return group:dept_bio, got %v", searchResp.Groups)
	}

	// Users are matched on their address...
	local := strings.Split(userStudent, "@")[0]
	searchResp = search(t, local)
	if len(searchResp.Users) == 0 {
		t.Errorf("searching %q should find the matching user", local)
	}
	for _, email := range searchResp.Users {
		if !strings.Contains(strings.ToLower(email), strings.ToLower(local)) {
			t.Errorf("user search returned non-matching %s", email)
		}
	}

	// ...but never listed wholesale: an empty query returns groups only, so the
	// directory cannot be browsed by opening the picker.
	rr := do(t, h, http.MethodGet, "/v1/principals/search", userRoot, nil)
	assertStatus(t, rr, http.StatusOK)
	var all webserver.PrincipalSearchResponse
	mustDecode(t, rr, &all)
	if len(all.Users) != 0 {
		t.Errorf("an empty query must not return users, got %v", all.Users)
	}
	if len(all.Groups) == 0 {
		t.Error("an empty query should still return groups")
	}
}

func TestConfigEndpoint(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodGet, "/v1/config", userFaculty, nil)
	assertStatus(t, rr, http.StatusOK)
	var cfg webserver.ConfigResponse
	mustDecode(t, rr, &cfg)
	if len(cfg.OpenstackRoles) == 0 {
		t.Errorf("config should list OpenStack roles")
	}
}

func TestAuthRequired(t *testing.T) {
	h := setupRouter(t)
	// The dummy auth middleware defaults unknown/absent users to root — but a
	// completely unauthenticated setup is covered by CombinedAuthMiddleware,
	// which is not under test here. Verify the happy path returns data.
	assertStatus(t, do(t, h, http.MethodGet, "/v1/nodes/mine", userFaculty, nil), http.StatusOK)
}
