package webserver_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/catalog"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/roleprovider"
	"github.com/pfisterer/openstack-management-api/internal/tree"
	"github.com/pfisterer/openstack-management-api/internal/webserver"
)

func catalogRouter(t *testing.T) http.Handler {
	t.Helper()
	store, sugar := newTestStore(t)
	cat, err := catalog.Load(context.Background(), quotaResources, &catalog.MemoryStore{}, sugar)
	if err != nil {
		t.Fatal(err)
	}
	svc := tree.NewService(store, roleprovider.NewMockRoleProvider(), quotaResources,
		rootAdminTokens, 10*time.Second, common.DefaultMaxAuthorizedUsers, testAccounting, sugar)
	svc.UseCatalog(cat)
	if err := svc.Bootstrap(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	return webserver.SetupGinWebserver(webserver.SetupConfig{
		DevMode: true,
		Log:     sugar,
		API: webserver.APIConfig{
			Service: svc, Catalog: cat, CatalogAdmin: &catalog.Admin{Catalog: cat, Tree: svc},
			RoleSwitchGroups: rootAdminTokens,
		},
		RootAdminTokens: rootAdminTokens,
		AuthMiddleware:  webserver.DummyAuthMiddleware(),
	})
}

func offered(t *testing.T, h http.Handler) []string {
	t.Helper()
	rec := do(t, h, http.MethodGet, "/v1/config", userRoot, nil)
	assertStatus(t, rec, http.StatusOK)
	var cfg struct {
		Resources []struct {
			ID string `json:"id"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range cfg.Resources {
		ids = append(ids, r.ID)
	}
	return ids
}

// Root admins manage availabilities, nobody else; the portal offers what the
// catalogue holds at the moment of asking.
func TestCatalogAdmin_Lifecycle(t *testing.T) {
	h := catalogRouter(t)
	gpu := map[string]any{"id": "gpu-a100", "name": "A100", "group": "GPU", "grant": map[string]string{"type": "flavor", "target": "f-1"}}

	assertStatus(t, do(t, h, http.MethodPost, "/v1/admin/catalog", userFaculty, gpu), http.StatusForbidden)
	assertStatus(t, do(t, h, http.MethodPost, "/v1/admin/catalog", userRoot, gpu), http.StatusCreated)
	assertStatus(t, do(t, h, http.MethodPost, "/v1/admin/catalog", userRoot, gpu), http.StatusConflict)
	if !slices.Contains(offered(t, h), "gpu-a100") {
		t.Error("an added availability must be offered right away")
	}

	assertStatus(t, do(t, h, http.MethodDelete, "/v1/admin/catalog/gpu-a100", userRoot, nil), http.StatusConflict)
	assertStatus(t, do(t, h, http.MethodPost, "/v1/admin/catalog/gpu-a100/withdraw", userRoot, nil), http.StatusOK)
	if slices.Contains(offered(t, h), "gpu-a100") {
		t.Error("a withdrawn availability must not be offered")
	}
	assertStatus(t, do(t, h, http.MethodGet, "/v1/admin/catalog/gpu-a100", userRoot, nil), http.StatusOK)
	assertStatus(t, do(t, h, http.MethodDelete, "/v1/admin/catalog/gpu-a100", userRoot, nil), http.StatusNoContent)
	assertStatus(t, do(t, h, http.MethodGet, "/v1/admin/catalog/gpu-a100", userRoot, nil), http.StatusNotFound)

	rec := do(t, h, http.MethodGet, "/v1/admin/catalog", userRoot, nil)
	assertStatus(t, rec, http.StatusOK)
	var list []catalog.View
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(list, func(v catalog.View) bool { return v.ID == "gpu-a100" }) {
		t.Errorf("list after removal = %+v, still lists it", list)
	}
}
