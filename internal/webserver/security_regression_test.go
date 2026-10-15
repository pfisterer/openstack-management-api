package webserver_test

import (
	"bytes"
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/cloud-self-service-golib/ginweb"
	"github.com/pfisterer/cloud-self-service-golib/token"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/nodestore"
	"github.com/pfisterer/openstack-management-api/internal/core/tree"
	"github.com/pfisterer/openstack-management-api/internal/dev"
	"github.com/pfisterer/openstack-management-api/internal/identity"
	"github.com/pfisterer/openstack-management-api/internal/webserver"
)

// A student's request for MaxInt cores in the auto-approving student budget used to wrap the budget's usage negative and be approved. It is refused now.
func TestAPI_OverflowRequestRefused(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodPost, "/v1/nodes", userStudent, tree.CreateNodeRequest{
		ParentID: "b_cs_students", Kind: model.KindProject, Name: "vm", Reason: "vm",
		Limit: common.ProjectQuota{"cores": math.MaxInt},
	})
	assertStatus(t, rr, http.StatusBadRequest)
}

// An approval carries the revision the approver saw: missing, the request is malformed; stale, it conflicts.
func TestAPI_ApproveNeedsTheCurrentRevision(t *testing.T) {
	h := setupRouter(t)
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/p_002/approve", userFaculty, map[string]any{}), http.StatusBadRequest)
	current := approval(t, h, "p_002", nil)
	stale := current
	stale.ExpectedRevision = new(*current.ExpectedRevision - 1)
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/p_002/approve", userFaculty, stale), http.StatusConflict)
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/p_002/approve", userFaculty, current), http.StatusOK)
}

// A body beyond the limit is refused before it is read.
func TestAPI_BodyTooLarge(t *testing.T) {
	h := setupRouter(t)
	body := bytes.Repeat([]byte(" "), webserver.MaxRequestBodyBytes+1)
	req := httptest.NewRequest(http.MethodPost, "/v1/nodes", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	assertStatus(t, rr, http.StatusRequestEntityTooLarge)

	// Without a length the read itself stops at the limit.
	req = httptest.NewRequest(http.MethodPost, "/v1/nodes", bytes.NewReader(body))
	req.ContentLength = -1
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code < 400 || rr.Code >= 500 {
		t.Fatalf("an unannounced oversized body: %d", rr.Code)
	}
}

// A failure of the service answers 500 with a generic message; the detail stays in the log.
func TestAPI_InternalErrorsAreNotShown(t *testing.T) {
	store, sugar := newTestStore(t)
	ids, nodes := dev.DefaultMockTreeState()
	if err := store.Seed(context.Background(), ids, nodes); err != nil {
		t.Fatal(err)
	}
	h := routerFromStoreAs(t, "", sugar, brokenNodeStore{Store: store, broken: "p_002"}, nil, testAccounting)
	rr := do(t, h, http.MethodGet, "/v1/nodes/p_002", "", nil)
	assertStatus(t, rr, http.StatusInternalServerError)
	if strings.Contains(rr.Body.String(), "10.0.0.5") || !strings.Contains(rr.Body.String(), "internal error") {
		t.Fatalf("the response shows the detail: %s", rr.Body.String())
	}
	// A validation message is still the caller's to read.
	rr = do(t, h, http.MethodPost, "/v1/nodes", "", tree.CreateNodeRequest{ParentID: "b_cs_faculty", Kind: model.KindProject, Name: " ", Reason: "r", Limit: common.ProjectQuota{}})
	assertStatus(t, rr, http.StatusBadRequest)
	if !strings.Contains(rr.Body.String(), "name is required") {
		t.Fatalf("validation message lost: %s", rr.Body.String())
	}
}

type brokenNodeStore struct {
	nodestore.Store
	broken string
}

func (s brokenNodeStore) GetNode(ctx context.Context, id string) (*model.Node, error) {
	if id == s.broken {
		return nil, errors.New("pq: connection refused to 10.0.0.5:5432")
	}
	return s.Store.GetNode(ctx, id)
}

// tokenRouter is the API behind an authentication that says whether the caller came with an API token, with a token policy that has a maximum lifetime.
func tokenRouter(t *testing.T, viaToken bool) http.Handler {
	t.Helper()
	store, sugar := newTestStore(t)
	svc := tree.NewService(store, dev.NewMockRoleProvider(), quotaResources, rootAdminTokens, 10*time.Second, common.DefaultMaxAuthorizedUsers, testAccounting, sugar)
	if err := svc.Bootstrap(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	auth := func(c *gin.Context) {
		ginweb.SetReadOnly(c, false)
		identity.SetAuthenticated(c, &common.UserClaims{Email: userStudent}, common.TokenList{"user:" + userStudent})
		if viaToken {
			identity.MarkViaAPIToken(c)
		}
		c.Next()
	}
	return webserver.SetupGinWebserver(webserver.SetupConfig{
		DevMode: true,
		Log:     sugar,
		API:     webserver.APIConfig{Service: svc, Identity: testIdentity(store, sugar), Catalog: common.StaticCatalog(quotaResources), ActAsTokens: rootAdminTokens},
		Tokens: webserver.TokenConfig{
			Service: token.NewService("os_mgt_", token.NewMemoryStore()),
			TTL:     token.TTLPolicy{Default: 24 * time.Hour, Max: 365 * 24 * time.Hour},
		},
		RootAdminTokens: rootAdminTokens,
		AuthMiddleware:  auth,
	})
}

// A token does not mint tokens — a leaked one could otherwise renew itself past every expiry — and no token outlives the configured maximum.
func TestTokens_NotFromATokenAndBounded(t *testing.T) {
	rr := do(t, tokenRouter(t, true), http.MethodPost, "/v1/tokens", "", map[string]any{"description": "chain"})
	assertStatus(t, rr, http.StatusForbidden)
	if !strings.Contains(rr.Body.String(), "not with another API token") {
		t.Errorf("the refusal should say why: %s", rr.Body.String())
	}

	h := tokenRouter(t, false)
	assertStatus(t, do(t, h, http.MethodPost, "/v1/tokens", "", map[string]any{"ttl_hours": 366 * 24}), http.StatusBadRequest)
	assertStatus(t, do(t, h, http.MethodPost, "/v1/tokens", "", map[string]any{"ttl_hours": 30 * 24}), http.StatusCreated)
}
