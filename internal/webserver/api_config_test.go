package webserver_test

// o27: GET /v1/config — the static UI bootstrap config.

import (
	"net/http"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/webserver"
)

func TestGetConfig_ReturnsStaticConfig(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodGet, "/v1/config", userFaculty, nil)
	assertStatus(t, rr, http.StatusOK)

	var cfg webserver.ProjectConfigResponse
	mustDecode(t, rr, &cfg)

	if len(cfg.DelegationStrategies) != 2 {
		t.Errorf("expected 2 delegation strategies (pool, allowance), got %d", len(cfg.DelegationStrategies))
	}
	if len(cfg.OpenstackRoles) != 3 {
		t.Errorf("expected 3 openstack roles (admin/member/reader), got %v", cfg.OpenstackRoles)
	}
	if cfg.Projects == nil {
		t.Error("projects should be a (possibly empty) array, not null")
	}
}
