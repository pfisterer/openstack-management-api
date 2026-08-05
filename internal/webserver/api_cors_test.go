package webserver_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func preflight(t *testing.T, h http.Handler, origin string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodOptions, "/v1/nodes/mine", nil)
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", "GET")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// A configured origin gets the credentialed CORS headers; anything else gets
// none, so the browser refuses to hand the response to the calling page.
func TestCORS_OnlyConfiguredOriginsAreAllowed(t *testing.T) {
	h := setupRouterCORS(t, "https://selfservice.example.cloud")

	rr := preflight(t, h, "https://selfservice.example.cloud")
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "https://selfservice.example.cloud" {
		t.Errorf("allowed origin: expected it echoed back, got %q", got)
	}
	if got := rr.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("allowed origin: expected credentials allowed, got %q", got)
	}

	rr = preflight(t, h, "https://evil.example")
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("foreign origin: expected no Allow-Origin header, got %q", got)
	}
}

// The default (nothing configured) allows no cross-origin access at all — the
// case that matters, because behind the BFF a foreign page's credentialed
// request would otherwise be answered readably.
func TestCORS_EmptyAllowlistRefusesEveryOrigin(t *testing.T) {
	h := setupRouterCORS(t)

	rr := preflight(t, h, "https://selfservice.example.cloud")
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("expected no Allow-Origin header, got %q", got)
	}
	// Loopback is allowed in development only — production must refuse it too,
	// otherwise a page on the developer's machine could talk to production.
	rr = preflight(t, h, "http://localhost:8084")
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("production: expected localhost refused, got %q", got)
	}
}

// Running the UI locally means a real cross-origin call from the Vite dev server
// to the API. Development mode allows loopback so that works without configuring
// anything; a foreign origin is still refused.
func TestCORS_DevModeAllowsLoopback(t *testing.T) {
	h := corsRouter(t, true)

	for _, origin := range []string{"http://localhost:8084", "http://127.0.0.1:5173"} {
		rr := preflight(t, h, origin)
		if got := rr.Header().Get("Access-Control-Allow-Origin"); got != origin {
			t.Errorf("dev mode: expected %q allowed, got %q", origin, got)
		}
	}

	rr := preflight(t, h, "https://evil.example")
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("dev mode: expected foreign origin refused, got %q", got)
	}
}
