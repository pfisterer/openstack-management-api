package webserver_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/cloud-self-service-golib/ginweb"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/webserver"
	"go.uber.org/zap"
)

const readOnlySecret = "os_mgt_readonly"
const writeSecret = "os_mgt_write"

// readOnlyRouter wires the real auth middleware against a stub token store, so
// the test exercises the same path a request from curl takes.
func readOnlyRouter(t *testing.T, useRESTRule bool) http.Handler {
	t.Helper()
	gin.SetMode(gin.TestMode)
	log := zap.NewNop().Sugar()

	lookup := func(_ context.Context, secret string) (common.TokenLookupResult, error) {
		switch secret {
		case readOnlySecret:
			return common.TokenLookupResult{Found: true, Subject: "a@b.c", ReadOnly: true}, nil
		case writeSecret:
			return common.TokenLookupResult{Found: true, Subject: "a@b.c"}, nil
		}
		return common.TokenLookupResult{Found: false}, nil
	}
	resolve := func(_ context.Context, claims *common.UserClaims) (common.TokenList, error) {
		return common.TokenList{"user:" + claims.Email}, nil
	}

	r := gin.New()
	g := r.Group("/v1")
	g.Use(webserver.CombinedAuthMiddleware(nil, lookup, resolve, log))
	if useRESTRule {
		g.Use(ginweb.RejectWritesForReadOnlyTokens(log))
	}
	g.GET("/thing", func(c *gin.Context) { c.Status(http.StatusOK) })
	g.POST("/thing", func(c *gin.Context) { c.Status(http.StatusOK) })
	// Stands in for /mcp: a POST that is a read, and answers the read-only
	// question itself instead of letting the method answer it.
	g.POST("/tool", func(c *gin.Context) {
		if ginweb.IsReadOnly(c) {
			c.JSON(http.StatusOK, gin.H{"read_only": true})
			return
		}
		c.JSON(http.StatusOK, gin.H{"read_only": false})
	})
	return r
}

func callWithToken(t *testing.T, h http.Handler, method, path, secret string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// The REST rule must behave exactly as it did while it lived inside the auth
// middleware: GET passes, anything else is refused for a read-only token.
func TestReadOnlyToken_RESTRuleUnchanged(t *testing.T) {
	h := readOnlyRouter(t, true)

	for _, tc := range []struct {
		name, method, secret string
		want                 int
	}{
		{"read-only GET is allowed", http.MethodGet, readOnlySecret, http.StatusOK},
		{"read-only POST is refused", http.MethodPost, readOnlySecret, http.StatusForbidden},
		{"write token may POST", http.MethodPost, writeSecret, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rr := callWithToken(t, h, tc.method, "/v1/thing", tc.secret); rr.Code != tc.want {
				t.Errorf("got %d, want %d: %s", rr.Code, tc.want, rr.Body.String())
			}
		})
	}
}

// The reason the rule moved out of the auth middleware: a group without it must
// be able to serve a POST to a read-only token and decide for itself. With the
// old code this was a 403 before any handler ran — every MCP tool call, reads
// included.
func TestReadOnlyToken_GroupWithoutTheRESTRuleDecidesForItself(t *testing.T) {
	h := readOnlyRouter(t, false)

	rr := callWithToken(t, h, http.MethodPost, "/v1/tool", readOnlySecret)
	if rr.Code != http.StatusOK {
		t.Fatalf("a read POST must reach its handler, got %d: %s", rr.Code, rr.Body.String())
	}
	if body := rr.Body.String(); body != `{"read_only":true}` {
		t.Errorf("handler saw %s, want the token reported as read-only", body)
	}
}

// A write token must be distinguishable there too, or the handler cannot gate
// its mutating tools.
func TestReadOnlyToken_WriteTokenIsVisibleToTheHandler(t *testing.T) {
	h := readOnlyRouter(t, false)

	rr := callWithToken(t, h, http.MethodPost, "/v1/tool", writeSecret)
	if body := rr.Body.String(); body != `{"read_only":false}` {
		t.Errorf("handler saw %s, want the token reported as writable", body)
	}
}
