package webserver_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pfisterer/cloud-self-service-golib/mcpserve/mcptest"
	"github.com/pfisterer/openstack-management-api/internal/provider/aiaas"
	"github.com/pfisterer/openstack-management-api/internal/webserver"
)

// languages are the Accept-Language headers a test sends, and which of the two texts each must get: German without one and for anything that names neither language, English when the caller asks for it.
var languages = []struct {
	header  string
	english bool
}{
	{"", false},
	{"de-DE,de;q=0.9", false},
	{"fr", false},
	{"en", true},
	{"en-US,en;q=0.9,de;q=0.8", true},
}

// doLang is do with an Accept-Language header (none when empty).
func doLang(t *testing.T, h http.Handler, method, path, lang string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("X-Act-As", userRoot)
	if lang != "" {
		req.Header.Set("Accept-Language", lang)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func errorBody(t *testing.T, rr *httptest.ResponseRecorder) (text, code string) {
	t.Helper()
	var body struct{ Error, Code string }
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", rr.Body, err)
	}
	return body.Error, body.Code
}

// The same request answers in German without Accept-Language and in English with Accept-Language: en; the status and the code do not change.
func TestREST_AnswersInTheCallersLanguage(t *testing.T) {
	h := setupRouter(t)
	for _, tc := range []struct {
		name, method, path string
		status             int
		code, de, en       string
	}{
		{"validation of the API layer", http.MethodGet, "/v1/nodes/search?q=%20", http.StatusBadRequest, "invalid", "q darf nicht leer sein", "q must not be empty"},
		{"a filter of a list", http.MethodGet, "/v1/nodes/to-manage?scope=all", http.StatusBadRequest, "invalid", `scope muss "direct" oder "subtree" sein`, `scope must be "direct" or "subtree"`},
		{"paging of golib", http.MethodGet, "/v1/nodes/mine?limit=0", http.StatusBadRequest, "invalid", "Ungültige Angaben zum Blättern (limit, offset): limit muss größer als 0 sein.", "Invalid paging (limit, offset): limit must be greater than 0."},
		{"a node that does not exist", http.MethodGet, "/v1/nodes/b_does_not_exist", http.StatusNotFound, "not_found", "Budget oder Projekt nicht gefunden.", "Budget or project not found."},
		{"the reconciler's refusal", http.MethodGet, "/v1/admin/reconcile/status", http.StatusServiceUnavailable, "reconciler_disabled", "Der Abgleich mit OpenStack ist ausgeschaltet.", "The reconciliation with OpenStack is switched off."},
	} {
		for _, l := range languages {
			rr := doLang(t, h, tc.method, tc.path, l.header)
			text, code := errorBody(t, rr)
			want := tc.de
			if l.english {
				want = tc.en
			}
			if rr.Code != tc.status || code != tc.code || text != want {
				t.Errorf("%s, Accept-Language %q: got %d %q %q, want %d %q %q", tc.name, l.header, rr.Code, code, text, tc.status, tc.code, want)
			}
			if vary := rr.Header().Get("Vary"); vary != "Accept-Language" {
				t.Errorf("%s: Vary = %q", tc.name, vary)
			}
		}
	}
}

// Without a credential the 401 is in the caller's language too.
func TestREST_UnauthenticatedInTheCallersLanguage(t *testing.T) {
	srv := mcpTestServer(t)
	for _, l := range languages {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/config", nil)
		if l.header != "" {
			req.Header.Set("Accept-Language", l.header)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var body struct{ Error, Code string }
		_ = json.NewDecoder(res.Body).Decode(&body)
		_ = res.Body.Close()
		want := "Anmeldung erforderlich."
		if l.english {
			want = "Sign-in required."
		}
		if res.StatusCode != http.StatusUnauthorized || body.Code != "unauthenticated" || body.Error != want {
			t.Errorf("Accept-Language %q: got %d %+v, want 401 %q", l.header, res.StatusCode, body, want)
		}
	}
}

// /v1/config hands out the caps' names and messages in the caller's language; ids, groups and choices stay.
func TestREST_ConfigCapsInTheCallersLanguage(t *testing.T) {
	h := newTestTree(t, mockNodes(), testAccounting).router(func(c *webserver.SetupConfig) { c.API.Caps = aiaas.Caps })
	for _, l := range languages {
		rr := doLang(t, h, http.MethodGet, "/v1/config", l.header)
		assertStatus(t, rr, http.StatusOK)
		var cfg struct {
			Caps []map[string]any `json:"caps"`
		}
		mustDecode(t, rr, &cfg)
		if len(cfg.Caps) != len(aiaas.Caps) {
			t.Fatalf("%d caps, want %d", len(cfg.Caps), len(aiaas.Caps))
		}
		got := cfg.Caps[len(cfg.Caps)-1]
		want := map[string]any{"id": aiaas.CapBatchMaxHours, "name": "Laufzeit eines Batch-Jobs", "message": "Wie lange ein Batch-Job höchstens läuft", "group": "Batch jobs"}
		if l.english {
			want["name"], want["message"] = "Run time of a batch job", "How long a batch job runs at most"
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("Accept-Language %q: %s = %v, want %v", l.header, k, got[k], v)
			}
		}
	}
}

// An MCP tool's refusal is in the language of the HTTP request that carried the call.
func TestMCP_AnswersInTheCallersLanguage(t *testing.T) {
	srv := mcpTestServer(t)
	for _, l := range languages {
		session := mcptest.DialLang(t, srv.URL+"/mcp", writeSecret, l.header)
		id, name := firstBudget(t, session)
		for _, tc := range []struct {
			tool   string
			args   map[string]any
			de, en string
		}{
			{"get_project", map[string]any{"id": "b_does_not_exist"}, `kein Projekt und kein Budget mit der ID "b_does_not_exist", oder es ist nicht sichtbar`, `no project and no budget with the ID "b_does_not_exist", or it is not visible`},
			{"delete_budget", map[string]any{"id": id, "confirm_name": name + " (wrong)"},
				`confirm_name "` + name + ` (wrong)" stimmt nicht: "` + id + `" heißt "` + name + `". Erst lesen, dann den aktuellen Namen zurückgeben.`,
				`confirm_name "` + name + ` (wrong)" does not match: "` + id + `" is named "` + name + `". Read it first, then pass back its current name.`},
			{"rename_project", map[string]any{"id": id, "name": ""}, "name darf nicht leer sein", "name must not be empty"},
			// A refusal of the tree, behind the operation it refused.
			{"rename_project", map[string]any{"id": id, "name": " "}, `"` + id + `" umbenennen: Ein Name ist erforderlich.`, `rename "` + id + `": A name is required.`},
		} {
			res := mcptest.Call(t, session, tc.tool, tc.args)
			want := tc.de
			if l.english {
				want = tc.en
			}
			if got := mcptest.Text(res); !res.IsError || got != want {
				t.Errorf("%s, Accept-Language %q: got %q (error %v), want %q", tc.tool, l.header, got, res.IsError, want)
			}
		}
	}
}
