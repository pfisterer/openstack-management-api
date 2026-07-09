package webserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/applogic"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/mockdata"
	"github.com/pfisterer/openstack-management-api/internal/roleprovider"
	"github.com/pfisterer/openstack-management-api/internal/storage"
	"github.com/pfisterer/openstack-management-api/internal/webserver"
	"go.uber.org/zap"
)

// Mock users from DefaultMockResourceState.
const (
	userRoot    = "root.admin@uni.example"
	userCSAdmin = "admin@cs.example"
	userFaculty = "faculty@cs.example"
	userBio     = "faculty@bio.example"
	userStudent = "cs-student@cs.com"
)

// quotaResourceIDs matches the resource IDs used in mock quota data.
var quotaResourceIDs = []string{"cores", "ram", "storage", "gpu"}

// rootAdminTokens mirrors the root-level tokens in mock data.
var rootAdminTokens = common.TokenList{"group:root_uni", "user:root.admin@uni.example"}

// setupRouter builds a Gin engine wired with:
//   - DummyAuthMiddleware (X-Dummy-Auth-User header)
//   - In-memory store seeded from DefaultMockResourceState
//   - MockRoleProvider
//   - No reconciler (reconciler endpoints return 503)
func setupRouter(t *testing.T) http.Handler { return setupRouterWith(t, nil) }

// setupRouterWith builds the test router with an injectable ReconcilerAPI
// (nil = reconciler disabled → 503 on the admin endpoints).
func setupRouterWith(t *testing.T, rec webserver.ReconcilerAPI) http.Handler {
	t.Helper()
	store, sugar := newTestStore(t)
	ids, delegations, projects, rules := mockdata.DefaultMockResourceState()
	if err := store.SeedProjectState(context.Background(), ids, delegations, projects, rules); err != nil {
		t.Fatalf("seed mock state: %v", err)
	}
	return routerFromStore(sugar, store, rec)
}

// setupRouterSeeded builds the router with a CUSTOM store seed (own delegation
// tree) — used by the end-to-end scenario suite. Identities still come from
// DefaultMockResourceState because the DummyAuthMiddleware resolves the caller's
// tokens from there.
func setupRouterSeeded(t *testing.T, delegations []common.Delegation, projects []common.Project, rules []common.TokenEligibilityRule) http.Handler {
	t.Helper()
	store, sugar := newTestStore(t)
	ids, _, _, _ := mockdata.DefaultMockResourceState()
	if err := store.SeedProjectState(context.Background(), ids, delegations, projects, rules); err != nil {
		t.Fatalf("seed scenario state: %v", err)
	}
	return routerFromStore(sugar, store, nil)
}

func newTestStore(t *testing.T) (applogic.ProjectStore, *zap.SugaredLogger) {
	t.Helper()
	log, err := zap.NewDevelopment()
	if err != nil {
		t.Fatalf("init logger: %v", err)
	}
	sugar := log.Sugar()
	return storage.NewInMemoryProjectStore(sugar), sugar
}

func routerFromStore(sugar *zap.SugaredLogger, store applogic.ProjectStore, rec webserver.ReconcilerAPI) http.Handler {
	svc := applogic.NewService(
		store,
		roleprovider.NewMockRoleProvider(),
		quotaResourceIDs,
		rootAdminTokens,
		10*time.Second,
		sugar,
	)
	return webserver.SetupGinWebserver(webserver.SetupConfig{
		DevMode:      true,
		Log:          sugar,
		StaticConfig: webserver.StaticConfig{},
		ProjectAPI: webserver.ProjectAPIConfig{
			Service: svc,
			// Role-switch allowlist = the (mixed user+group) root admin tokens,
			// exactly as app.go wires it. canUseRoleSwitch accepts either kind.
			RoleSwitchGroups: rootAdminTokens,
		},
		Reconciler:      rec,
		RootAdminTokens: rootAdminTokens,
		AuthMiddleware:  webserver.DummyAuthMiddleware(),
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

// projectIDs extracts IDs from a project slice for use in error messages.
func projectIDs(ps []common.Project) []string {
	ids := make([]string, len(ps))
	for i, p := range ps {
		ids[i] = p.ID
	}
	return ids
}
