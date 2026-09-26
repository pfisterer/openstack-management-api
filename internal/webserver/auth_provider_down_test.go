package webserver_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/cloud-self-service-golib/oidcauth"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/webserver"
	"go.uber.org/zap"
)

// What this pins down is the difference between "we reject this token" and "we
// cannot judge this token right now". Both come out of Verify as an error, and
// answering 401 for the second one is what sends a browser into a login loop
// against an identity provider that is not there — the state prod was in on
// 2026-09-25.

const testIssuer = "https://sso.example/realms/test"
const testClientID = "test-client"

// wellFormedToken carries the right issuer, audience and expiry with a nonsense
// signature: go-oidc checks those claims before it fetches a key, so a
// malformed token would never reach the provider at all.
func wellFormedToken(t *testing.T) string {
	t.Helper()
	part := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal token part: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return fmt.Sprintf("%s.%s.%s",
		part(map[string]string{"alg": "RS256", "kid": "test-key"}),
		part(map[string]any{
			"iss":   testIssuer,
			"aud":   testClientID,
			"sub":   "someone",
			"email": "someone@example.org",
			"exp":   time.Now().Add(time.Hour).Unix(),
		}),
		base64.RawURLEncoding.EncodeToString([]byte("not-a-signature")))
}

// routerWithProvider builds the auth middleware against a key set served by the
// given handler, so a test can decide whether the provider is up.
func routerWithProvider(t *testing.T, jwks http.HandlerFunc) http.Handler {
	t.Helper()
	provider := httptest.NewServer(jwks)
	t.Cleanup(provider.Close)

	verifier, err := webserver.NewOIDCAuthVerifier(webserver.OIDCVerifierConfig{
		IssuerURL: testIssuer,
		ClientID:  testClientID,
		JWKSURL:   provider.URL,
	}, zap.NewNop().Sugar())
	if err != nil {
		t.Fatalf("build verifier: %v", err)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(webserver.CombinedAuthMiddleware(verifier,
		func(_ context.Context, _ string) (common.TokenLookupResult, error) {
			return common.TokenLookupResult{}, nil
		},
		func(_ context.Context, _ *common.UserClaims) (common.TokenList, error) {
			return common.TokenList{}, nil
		},
		zap.NewNop().Sugar()))
	router.GET("/v1/probe", func(c *gin.Context) { c.Status(http.StatusOK) })
	return router
}

func request(t *testing.T, h http.Handler, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/probe", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// The identity provider is down: the answer says "try again", not "your token
// is wrong", because the client must keep the session it has.
func TestAuth_ProviderDownAnswers503(t *testing.T) {
	h := routerWithProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>502 Bad Gateway</html>"))
	})

	rr := request(t, h, wellFormedToken(t))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 while the provider is away, got %d (%s)", rr.Code, rr.Body.String())
	}
}

// The provider answers, the token does not check out: that is an ordinary 401.
func TestAuth_BadTokenAnswers401(t *testing.T) {
	h := routerWithProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"keys":[]}`))
	})

	rr := request(t, h, wellFormedToken(t))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for a token nobody signed, got %d (%s)", rr.Code, rr.Body.String())
	}
}

// And the verifier itself is built without touching the provider at all, which
// is what lets the service start while the provider is down.
func TestAuth_StartsWithoutTheProvider(t *testing.T) {
	verifier, err := webserver.NewOIDCAuthVerifier(webserver.OIDCVerifierConfig{
		IssuerURL: testIssuer,
		ClientID:  testClientID,
		// Nothing listens here.
		JWKSURL: "http://127.0.0.1:1/certs",
	}, zap.NewNop().Sugar())
	if err != nil {
		t.Fatalf("building the verifier must not need the provider: %v", err)
	}
	if verifier.KeysUnavailable() {
		t.Fatal("a provider nobody has asked anything of is not known to be broken")
	}
	var _ *oidcauth.Verifier = verifier
}
