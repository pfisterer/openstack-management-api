package webserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/pfisterer/cloud-self-service-golib/token"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/identity"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/nodestore"
	"github.com/pfisterer/openstack-management-api/internal/core/tree"
	"github.com/pfisterer/openstack-management-api/internal/dev"
	osclient "github.com/pfisterer/openstack-management-api/internal/provider/openstack/client"
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

// quotaResourceIDs matches the resource IDs used in mock data. Kept as plain
// ids for the places that sum (UsageByStatus.Total takes the arithmetic set).
var quotaResourceIDs = []string{"cores", "ram", "storage"}

// managedResources is the same set as the deployment configures it, with the
// OpenStack side; quotaResources is what the tree sees of it.
var managedResources = func() []osclient.ManagedResource {
	defs := make([]osclient.ManagedResource, 0, len(quotaResourceIDs)+1)
	for _, id := range quotaResourceIDs {
		defs = append(defs, osclient.ManagedResource{ID: id, Name: id, Group: "Compute", ShowOnUI: true})
	}
	// One availability, so the catalogue the handlers see is the mixed kind the
	// deployment actually carries — a fixture of quantities alone would let a
	// bug in the availability path pass every test here.
	defs = append(defs, osclient.ManagedResource{
		ID: "dhbw-ipv4", Name: "DHBW IPv4", Kind: common.KindBool, Group: "Networks",
		ShowOnUI: true,
		Grant:    &osclient.Grant{Type: osclient.GrantNetwork, Target: "net-uuid-not-for-browsers"},
	})
	return defs
}()

var quotaResources = osclient.CoreResources(managedResources)

// testAccounting mirrors the production defaults, so a test that passes here
// says something about the deployment rather than about a lenient fixture.
var testAccounting = tree.Accounting{ChargeInUse: true, ChargeReleased: true}

// rootAdminTokens mirrors the root-level tokens in mock data.
var rootAdminTokens = common.TokenList{"group:root_uni", "user:root.admin@uni.example"}

// setupRouter builds a Gin engine wired with:
//   - DummyAuthMiddleware: signed in as the root admin, other people via X-Act-As
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
	svc := tree.NewService(store, dev.NewMockRoleProvider(), quotaResources,
		rootAdminTokens, 10*time.Second, common.DefaultMaxAuthorizedUsers, testAccounting, sugar)
	if err := svc.Bootstrap(context.Background(), nil, nil); err != nil {
		t.Fatalf("bootstrap tree: %v", err)
	}
	return webserver.SetupGinWebserver(webserver.SetupConfig{
		DevMode:            devMode,
		Log:                sugar,
		API:                webserver.APIConfig{Service: svc, Identity: testIdentity(store, sugar), ActAsTokens: rootAdminTokens},
		RootAdminTokens:    rootAdminTokens,
		AuthMiddleware:     dev.DummyAuthMiddleware(""),
		CORSAllowedOrigins: corsOrigins,
	})
}

// setupRouterWith builds the test router with an injectable ReconcilerAPI
// (nil = reconciler disabled → 503 on the admin endpoints).
func setupRouterWith(t *testing.T, rec webserver.ReconcilerAPI) http.Handler {
	t.Helper()
	store, sugar := newTestStore(t)
	ids, nodes := dev.DefaultMockTreeState()
	if err := store.Seed(context.Background(), ids, nodes); err != nil {
		t.Fatalf("seed mock state: %v", err)
	}
	return routerFromStore(t, sugar, store, rec, testAccounting)
}

// setupRouterSeeded builds the router with a CUSTOM node seed — used by the
// end-to-end scenario suite. Identities still come from DefaultMockTreeState
// because the DummyAuthMiddleware resolves the caller's tokens from there.
// The service bootstrap runs afterwards, so the structural root/unassigned nodes
// always exist and the root admin scope is synced to rootAdminTokens.
//
// The accounting policy is a parameter because it changes what the same
// lifecycle costs — see the scenario suite for why it does not just take the
// production default.
func setupRouterSeeded(t *testing.T, nodes []model.Node, acc tree.Accounting) http.Handler {
	t.Helper()
	store, sugar := newTestStore(t)
	ids, _ := dev.DefaultMockTreeState()
	if err := store.Seed(context.Background(), ids, nodes); err != nil {
		t.Fatalf("seed scenario state: %v", err)
	}
	return routerFromStore(t, sugar, store, nil, acc)
}

func newTestStore(t *testing.T) (nodestore.Store, *zap.SugaredLogger) {
	t.Helper()
	log, err := zap.NewDevelopment()
	if err != nil {
		t.Fatalf("init logger: %v", err)
	}
	sugar := log.Sugar()
	return nodestore.NewInMemoryStore(sugar), sugar
}

func routerFromStore(t *testing.T, sugar *zap.SugaredLogger, store nodestore.Store, rec webserver.ReconcilerAPI, acc tree.Accounting, corsOrigins ...string) http.Handler {
	t.Helper()
	return routerFromStoreAs(t, "", sugar, store, rec, acc, corsOrigins...)
}

// setupRouterSignedInAs builds the seeded test router with somebody other than the root admin signed in — for what a caller may NOT do, which acting on behalf of them cannot show.
func setupRouterSignedInAs(t *testing.T, devUser string) http.Handler {
	t.Helper()
	store, sugar := newTestStore(t)
	ids, nodes := dev.DefaultMockTreeState()
	if err := store.Seed(context.Background(), ids, nodes); err != nil {
		t.Fatalf("seed mock state: %v", err)
	}
	return routerFromStoreAs(t, devUser, sugar, store, nil, testAccounting)
}

func routerFromStoreAs(t *testing.T, devUser string, sugar *zap.SugaredLogger, store nodestore.Store, rec webserver.ReconcilerAPI, acc tree.Accounting, corsOrigins ...string) http.Handler {
	t.Helper()
	svc := tree.NewService(
		store,
		dev.NewMockRoleProvider(),
		quotaResources,
		rootAdminTokens,
		10*time.Second,
		common.DefaultMaxAuthorizedUsers,
		acc,
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
			Service:  svc,
			Identity: testIdentity(store, sugar),
			Catalog:  common.StaticCatalog(quotaResources),
			// Role-switch allowlist = the (mixed user+group) root admin tokens,
			// exactly as app.go wires it. canUseRoleSwitch accepts either kind.
			ActAsTokens: rootAdminTokens,
		},
		Reconciler:         rec,
		RootAdminTokens:    rootAdminTokens,
		Tokens:             webserver.TokenConfig{Service: token.NewService("os_mgt_", token.NewMemoryStore()), TTL: token.TTLPolicy{Default: 24 * time.Hour}},
		AuthMiddleware:     dev.DummyAuthMiddleware(devUser),
		CORSAllowedOrigins: corsOrigins,
	})
}

// do sends an HTTP request to the handler as the given user and returns the recorder.
// The root admin is signed in; any other user is reached by acting on their behalf (X-Act-As). Set user="" to send no header.
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
		req.Header.Set("X-Act-As", user)
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
func decodePage(t *testing.T, rr *httptest.ResponseRecorder) []model.Node {
	t.Helper()
	var page tree.NodePage
	mustDecode(t, rr, &page)
	return page.Items
}

// nodeIDs extracts IDs from a node slice for use in error messages.
func nodeIDs(ns []model.Node) []string {
	ids := make([]string, len(ns))
	for i, n := range ns {
		ids[i] = n.ID
	}
	return ids
}

// testIdentity is who callers are, over the same store and mock role provider
// as the tree in these tests.
func testIdentity(store nodestore.Store, sugar *zap.SugaredLogger) *identity.Service {
	return identity.NewService(store, dev.NewMockRoleProvider(), 10*time.Second, sugar)
}

// approval is the body of an approval of node id as the UI sends it: with the revision the node has now (read as the root admin) and, where given, a modified limit.
func approval(t *testing.T, h http.Handler, id string, modified *common.ProjectQuota) tree.ApproveNodeRequest {
	t.Helper()
	rr := do(t, h, http.MethodGet, "/v1/nodes/"+id, "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("read %s for its revision: %d %s", id, rr.Code, rr.Body.String())
	}
	var n model.Node
	mustDecode(t, rr, &n)
	return tree.ApproveNodeRequest{ModifiedLimit: modified, ExpectedRevision: new(n.Revision)}
}
