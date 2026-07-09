package webserver_test

// o25: GET /v1/delegations/eligible-for-owner — root-admin only, owner_token required.

import (
	"net/http"
	"testing"
)

func TestEligibleForOwner_MissingOwnerTokenReturns400(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodGet, "/v1/delegations/eligible-for-owner", userRoot, nil)
	assertStatus(t, rr, http.StatusBadRequest)
}

func TestEligibleForOwner_NonRootForbidden(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodGet, "/v1/delegations/eligible-for-owner?owner_token=group:cs-student", userFaculty, nil)
	assertStatus(t, rr, http.StatusForbidden)
}

func TestEligibleForOwner_RootOK(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodGet, "/v1/delegations/eligible-for-owner?owner_token=group:cs-student", userRoot, nil)
	assertStatus(t, rr, http.StatusOK)
}
