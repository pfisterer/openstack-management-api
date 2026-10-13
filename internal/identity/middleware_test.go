package identity

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"go.uber.org/zap"
)

type fakeEffective struct {
	tokens common.TokenList
	err    error
}

func (f fakeEffective) TokensFor(string) (common.TokenList, error) { return f.tokens, f.err }

// serve runs one request through EffectiveAuthMiddleware with the given caller and answers 200 with what the handler saw.
func serve(t *testing.T, eff Effective, apiToken bool, actAs string) (*httptest.ResponseRecorder, AuthContext) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	var seen AuthContext
	r.Use(func(c *gin.Context) {
		SetAuthenticated(c, &common.UserClaims{Email: "root@x.de"}, common.TokenList{"user:root@x.de", "group:root"})
		if apiToken {
			c.Set(viaAPITokenKey, true)
		}
	})
	r.Use(EffectiveAuthMiddleware(eff, common.TokenList{"group:root"}, zap.NewNop().Sugar()))
	r.GET("/", func(c *gin.Context) { seen, _ = FromContext(c); c.Status(http.StatusOK) })
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if actAs != "" {
		req.Header.Set("X-Act-As", actAs)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr, seen
}

func TestEffectiveAuth_ActAs(t *testing.T) {
	rr, auth := serve(t, fakeEffective{tokens: common.TokenList{"user:s@x.de"}}, false, "S@x.de")
	if rr.Code != http.StatusOK || auth.UserEmail != "s@x.de" || auth.ActorEmail != "root@x.de" || !auth.OnBehalf() {
		t.Fatalf("got %d %+v", rr.Code, auth)
	}
	if len(auth.EffectiveTokens) != 1 || auth.EffectiveTokens[0] != "user:s@x.de" {
		t.Errorf("effective tokens should be the person's, got %v", auth.EffectiveTokens)
	}
}

func TestEffectiveAuth_NoHeader(t *testing.T) {
	rr, auth := serve(t, fakeEffective{}, false, "")
	if rr.Code != http.StatusOK || auth.OnBehalf() || auth.UserEmail != "root@x.de" {
		t.Fatalf("got %d %+v", rr.Code, auth)
	}
}

func TestEffectiveAuth_APITokenCannotActAs(t *testing.T) {
	rr, _ := serve(t, fakeEffective{tokens: common.TokenList{"user:s@x.de"}}, true, "s@x.de")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("an API token acting on behalf of somebody: want 403, got %d", rr.Code)
	}
}

func TestEffectiveAuth_DirectoryDown(t *testing.T) {
	rr, _ := serve(t, fakeEffective{err: errors.New("down")}, false, "s@x.de")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 rather than a silent fallback to the admin's own rights, got %d", rr.Code)
	}
}
