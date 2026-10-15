package webserver_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/dev"
	"github.com/pfisterer/openstack-management-api/internal/webserver"
)

// The catalogue reaches the browser; the cloud behind it does not.
//
// Every server-side field of a resource — the grant target, the OpenStack quota
// field, the multiplier — carries a json tag, because the catalogue is PARSED
// from configuration and a field without one cannot be configured. That makes
// the response the place where the line is drawn, and this is the test that
// draws it.
func TestGetConfig_ShipsOnlyDisplayFields(t *testing.T) {
	router := setupRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/config", nil)
	req.Header.Set("X-Act-As", "someone@dhbw.de")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/config = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	for _, leaked := range []string{"grant", "os_quota_field", "os_multiplier", "os_linked_field", "os_overcommit_check", "static"} {
		if strings.Contains(rec.Body.String(), `"`+leaked+`"`) {
			t.Errorf("the config response carries the server-side field %q:\n%s", leaked, rec.Body.String())
		}
	}

	// Belt and braces: the resources themselves still arrive, so a response that
	// simply lost its resource list cannot pass this test.
	var payload struct {
		Resources []map[string]any `json:"resources"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(payload.Resources) == 0 {
		t.Fatal("no resources in the config response at all")
	}
	// And the display fields DID make it, so a response that lost everything
	// cannot pass by being empty.
	first := payload.Resources[0]
	if first["id"] == nil || first["name"] == nil {
		t.Errorf("a resource arrived without id or name: %v", first)
	}
}

// A GPU class's resources carry pool, tier, GPU types and lending to the browser: the forms arrange GPUs by pool from them, and without them every pool read as not lending.
func TestGetConfig_ShipsTheGPUClass(t *testing.T) {
	store, sugar := newTestStore(t)
	class := &common.GPUClassInfo{Pool: "mannheim", PoolName: "Mannheim", Tier: "large", TierName: "large", Products: "NVIDIA-RTX-PRO-6000", Lends: true}
	cat := common.StaticCatalog{{ID: "ws-gpus-mannheim-large", Name: "Mannheim · large: GPUs at the same time", ShowOnUI: true, GPUClass: class}}
	router := webserver.SetupGinWebserver(webserver.SetupConfig{
		DevMode:         true,
		Log:             sugar,
		API:             webserver.APIConfig{Identity: testIdentity(store, sugar), Catalog: cat, ActAsTokens: rootAdminTokens},
		RootAdminTokens: rootAdminTokens,
		AuthMiddleware:  dev.DummyAuthMiddleware(""),
	})

	rec := do(t, router, http.MethodGet, "/v1/config", userRoot, nil)
	assertStatus(t, rec, http.StatusOK)
	var payload struct {
		Resources []struct {
			ID       string               `json:"id"`
			GPUClass *common.GPUClassInfo `json:"gpu_class"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(payload.Resources) != 1 || payload.Resources[0].GPUClass == nil || *payload.Resources[0].GPUClass != *class {
		t.Fatalf("gpu_class lost on the way: %s", rec.Body.String())
	}
}
