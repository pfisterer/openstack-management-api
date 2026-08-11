package webserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/mockdata"
	"github.com/pfisterer/openstack-management-api/internal/roleprovider"
	"github.com/pfisterer/openstack-management-api/internal/tree"
	"github.com/pfisterer/openstack-management-api/internal/webserver"
	"go.uber.org/zap"
)

// Mock users from DefaultMockTreeState.
const (
	userRoot    = "root.admin@uni.example"
	userCSAdmin = "admin@cs.example"
	userFaculty = "faculty@cs.example"
	userBio     = "faculty@bio.example"
	userStudent = "cs-student@cs.com"
)

// quotaResourceIDs matches the resource IDs used in mock data.
var quotaResourceIDs = []string{"cores", "ram", "storage", "gpu"}

// rootAdminTokens mirrors the root-level tokens in mock data.
var rootAdminTokens = common.TokenList{"group:root_uni", "user:root.admin@uni.example"}

// setupRouter builds a Gin engine wired with:
//   - DummyAuthMiddleware (X-Dummy-Auth-User header)
//   - In-memory store seeded from DefaultMockTreeState
//   - MockRoleProvider
//   - No reconciler (reconciler endpoints return 503)
func setupRouter(t *testing.T) http.Handler { return setupRouterWith(t, nil) }

// setupRouterCORS builds the test router with an explicit CORS allowlist, in
// PRODUCTION mode — the CORS rules are what production applies, and dev mode
// deliberately relaxes them for loopback origins.
func setupRouterCORS(t *testing.T, corsOrigins ...string) http.Handler {
	t.Helper()
	return corsRouter(t, false, corsOrigins...)
}

// corsRouter builds a router that only differs in DevMode and the allowlist.
func corsRouter(t *testing.T, devMode bool, corsOrigins ...string) http.Handler {
	t.Helper()
	store, sugar := newTestStore(t)
	svc := tree.NewService(store, roleprovider.NewMockRoleProvider(), quotaResourceIDs,
		rootAdminTokens, 10*time.Second, common.DefaultMaxAuthorizedUsers, true, sugar)
	if err := svc.Bootstrap(context.Background(), nil, nil); err != nil {
		t.Fatalf("bootstrap tree: %v", err)
	}
	return webserver.SetupGinWebserver(webserver.SetupConfig{
		DevMode:            devMode,
		Log:                sugar,
		API:                webserver.APIConfig{Service: svc, RoleSwitchGroups: rootAdminTokens},
		RootAdminTokens:    rootAdminTokens,
		AuthMiddleware:     webserver.DummyAuthMiddleware(),
		CORSAllowedOrigins: corsOrigins,
	})
}

// setupRouterWith builds the test router with an injectable ReconcilerAPI
// (nil = reconciler disabled → 503 on the admin endpoints).
func setupRouterWith(t *testing.T, rec webserver.ReconcilerAPI) http.Handler {
	t.Helper()
	store, sugar := newTestStore(t)
	ids, nodes := mockdata.DefaultMockTreeState()
	if err := store.Seed(context.Background(), ids, nodes); err != nil {
		t.Fatalf("seed mock state: %v", err)
	}
	return routerFromStore(t, sugar, store, rec)
}

// setupRouterSeeded builds the router with a CUSTOM node seed — used by the
// end-to-end scenario suite. Identities still come from DefaultMockTreeState
// because the DummyAuthMiddleware resolves the caller's tokens from there.
// The service bootstrap runs afterwards, so the structural root/unassigned nodes
// always exist and the root admin scope is synced to rootAdminTokens.
func setupRouterSeeded(t *testing.T, nodes []tree.Node) http.Handler {
	t.Helper()
	store, sugar := newTestStore(t)
	ids, _ := mockdata.DefaultMockTreeState()
	if err := store.Seed(context.Background(), ids, nodes); err != nil {
		t.Fatalf("seed scenario state: %v", err)
	}
	return routerFromStore(t, sugar, store, nil)
}

func newTestStore(t *testing.T) (tree.Store, *zap.SugaredLogger) {
	t.Helper()
	log, err := zap.NewDevelopment()
	if err != nil {
		t.Fatalf("init logger: %v", err)
	}
	sugar := log.Sugar()
	return tree.NewInMemoryStore(sugar), sugar
}

func routerFromStore(t *testing.T, sugar *zap.SugaredLogger, store tree.Store, rec webserver.ReconcilerAPI, corsOrigins ...string) http.Handler {
	t.Helper()
	svc := tree.NewService(
		store,
		roleprovider.NewMockRoleProvider(),
		quotaResourceIDs,
		rootAdminTokens,
		10*time.Second,
		common.DefaultMaxAuthorizedUsers,
		true,
		sugar,
	)
	// Same as app.go: ensure the structural nodes exist and the root admin scope
	// is synced from the configured tokens.
	if err := svc.Bootstrap(context.Background(), nil, nil); err != nil {
		t.Fatalf("bootstrap tree: %v", err)
	}
	return webserver.SetupGinWebserver(webserver.SetupConfig{
		DevMode:      true,
		Log:          sugar,
		StaticConfig: webserver.StaticConfig{},
		API: webserver.APIConfig{
			Service: svc,
			// Role-switch allowlist = the (mixed user+group) root admin tokens,
			// exactly as app.go wires it. canUseRoleSwitch accepts either kind.
			RoleSwitchGroups: rootAdminTokens,
		},
		Reconciler:         rec,
		RootAdminTokens:    rootAdminTokens,
		AuthMiddleware:     webserver.DummyAuthMiddleware(),
		CORSAllowedOrigins: corsOrigins,
	})
}

// do sends an HTTP request to the handler as the given user and returns the recorder.
// Set user="" to omit the X-Dummy-Auth-User header (defaults to root.admin@uni.example
// per DummyAuthMiddleware behaviour).
func do(t *testing.T, h http.Handler, method, path, user string, body any) *httptest.ResponseRecorder {
	t.Helper()

	var bodyBytes []byte
	if body != nil {
		var merr error
		bodyBytes, merr = json.Marshal(body)
		if merr != nil {
			t.Fatalf("marshal request body: %v", merr)
		}
	}

	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	if user != "" {
		req.Header.Set("X-Dummy-Auth-User", user)
	}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// mustDecode unmarshals the response body into v; fails the test on error.
func mustDecode[T any](t *testing.T, rr *httptest.ResponseRecorder, v *T) {
	t.Helper()
	if err := json.Unmarshal(rr.Body.Bytes(), v); err != nil {
		t.Fatalf("decode response (status=%d) body %q: %v", rr.Code, rr.Body.String(), err)
	}
}

// assertStatus fails the test when the recorder's status code does not match expected.
func assertStatus(t *testing.T, rr *httptest.ResponseRecorder, expected int) {
	t.Helper()
	if rr.Code != expected {
		t.Errorf("expected HTTP %d, got %d\nbody: %s", expected, rr.Code, rr.Body.String())
	}
}

// futureDate returns an RFC3339 timestamp n days from now (UTC).
func futureDate(days int) string {
	return time.Now().UTC().Add(time.Duration(days) * 24 * time.Hour).Format(time.RFC3339)
}

// decodePage decodes a paginated node listing and returns the page's items.
// Tests that also care about the total decode tree.NodePage themselves.
func decodePage(t *testing.T, rr *httptest.ResponseRecorder) []tree.Node {
	t.Helper()
	var page tree.NodePage
	mustDecode(t, rr, &page)
	return page.Items
}

// nodeIDs extracts IDs from a node slice for use in error messages.
func nodeIDs(ns []tree.Node) []string {
	ids := make([]string, len(ns))
	for i, n := range ns {
		ids[i] = n.ID
	}
	return ids
}
