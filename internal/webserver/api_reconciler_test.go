package webserver_test

// The reconciler is nil in the test router, so authorised callers receive 503
// Service Unavailable (reconciler disabled) rather than a successful response.
// Non-root-admin callers must receive 403 before the handler is reached.
//
// Root admin tokens: group:root_uni, user:root.admin@uni.example

import (
	"net/http"
	"testing"
)

// ── GET /v1/admin/reconcile/status ────────────────────────────────────────────

func TestReconcileStatus_RootAdminGets503WhenDisabled(t *testing.T) {
	h := setupRouter(t)
	// Root admin is authorised; reconciler is nil → 503
	rr := do(t, h, http.MethodGet, "/v1/admin/reconcile/status", userRoot, nil)
	assertStatus(t, rr, http.StatusServiceUnavailable)
}

func TestReconcileStatus_RegularUserGetsForbidden(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodGet, "/v1/admin/reconcile/status", userFaculty, nil)
	assertStatus(t, rr, http.StatusForbidden)
}

func TestReconcileStatus_CSAdminGetsForbidden(t *testing.T) {
	h := setupRouter(t)
	// admin@cs.example is admin of dept_cs_admin but not a root admin
	rr := do(t, h, http.MethodGet, "/v1/admin/reconcile/status", userCSAdmin, nil)
	assertStatus(t, rr, http.StatusForbidden)
}

func TestReconcileStatus_BioFacultyGetsForbidden(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodGet, "/v1/admin/reconcile/status", userBio, nil)
	assertStatus(t, rr, http.StatusForbidden)
}

// ── POST /v1/admin/reconcile/trigger ─────────────────────────────────────────

func TestReconcileTrigger_RootAdminGets503WhenDisabled(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodPost, "/v1/admin/reconcile/trigger", userRoot, nil)
	assertStatus(t, rr, http.StatusServiceUnavailable)
}

func TestReconcileTrigger_RegularUserGetsForbidden(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodPost, "/v1/admin/reconcile/trigger", userFaculty, nil)
	assertStatus(t, rr, http.StatusForbidden)
}

func TestReconcileTrigger_StudentGetsForbidden(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodPost, "/v1/admin/reconcile/trigger", userStudent, nil)
	assertStatus(t, rr, http.StatusForbidden)
}
