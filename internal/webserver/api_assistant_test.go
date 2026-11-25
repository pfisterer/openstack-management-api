package webserver_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/assistant"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/identity"
	"github.com/pfisterer/openstack-management-api/internal/webserver"
)

// scriptedModel plays a model: one reply per turn, and it remembers the tools it was offered and the messages it got.
type scriptedModel struct {
	mu      sync.Mutex
	replies []assistant.Reply
	offered [][]string
	sent    [][]assistant.Message
}

func (m *scriptedModel) Chat(_ context.Context, messages []assistant.Message, tools []assistant.ToolDef) (assistant.Reply, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := []string{}
	for _, t := range tools {
		names = append(names, t.Name)
	}
	m.offered = append(m.offered, names)
	m.sent = append(m.sent, messages)
	if len(m.replies) == 0 {
		return assistant.Reply{Content: "Fertig."}, nil
	}
	r := m.replies[0]
	m.replies = m.replies[1:]
	return r, nil
}

func toolCall(id, name, args string) assistant.ToolCall {
	tc := assistant.ToolCall{ID: id, Type: "function"}
	tc.Function.Name = name
	tc.Function.Arguments = args
	return tc
}

func assistantRouter(t *testing.T, model assistant.ChatModel, limiter *assistant.Limiter) http.Handler {
	t.Helper()
	tr := newTestTree(t, mockNodes(), testAccounting)
	return tr.router(func(c *webserver.SetupConfig) {
		c.API.Assistant = &webserver.AssistantConfig{Agent: &assistant.Agent{Model: model, ModelName: "assistant"}, Limiter: limiter}
	})
}

func TestAssistant_OffWithoutModel(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodPost, "/v1/assistant/ask", "", map[string]any{"question": "Hallo"})
	assertStatus(t, rr, http.StatusServiceUnavailable)
	var cfg webserver.ConfigResponse
	mustDecode(t, do(t, h, http.MethodGet, "/v1/config", "", nil), &cfg)
	if cfg.Assistant != "" {
		t.Errorf("config says assistant %q while it is off", cfg.Assistant)
	}
}

// The whole path: the model reads the person's projects through the in-process MCP tools, as that person; a write it asks for (here prompted by a planted instruction) is not even there to call, and nothing changes.
func TestAssistant_ReadsAsThePersonAndCannotWrite(t *testing.T) {
	llm := &scriptedModel{replies: []assistant.Reply{
		{ToolCalls: []assistant.ToolCall{toolCall("1", "list_my_projects", `{}`)}},
		{ToolCalls: []assistant.ToolCall{toolCall("2", "rename_project", `{"id":"p_001","name":"gekapert"}`), toolCall("3", "get_project", `{"id":"p_004"}`)}},
		{Content: "Du hast das Projekt Faculty research sandbox."},
	}}
	h := assistantRouter(t, llm, nil)
	const faculty = "faculty@cs.example"

	var cfg webserver.ConfigResponse
	mustDecode(t, do(t, h, http.MethodGet, "/v1/config", faculty, nil), &cfg)
	if cfg.Assistant != "read_only" {
		t.Errorf("config says assistant %q", cfg.Assistant)
	}

	rr := do(t, h, http.MethodPost, "/v1/assistant/ask", faculty, map[string]any{"question": "Welche Projekte habe ich?"})
	assertStatus(t, rr, http.StatusOK)
	var ans assistant.Answer
	mustDecode(t, rr, &ans)

	if ans.Answer != "Du hast das Projekt Faculty research sandbox." || ans.Stage != "read_only" || ans.Model != "assistant" {
		t.Errorf("answer %+v", ans)
	}
	if len(ans.ToolCalls) != 3 {
		t.Fatalf("tool calls %+v", ans.ToolCalls)
	}
	if c := ans.ToolCalls[0]; !c.OK || !strings.Contains(c.Result, "Faculty research sandbox") {
		t.Errorf("list_my_projects: %+v", c)
	}
	if c := ans.ToolCalls[1]; c.OK || c.Error != "not offered" {
		t.Errorf("rename_project must be refused: %+v", c)
	}
	// A budget of another faculty: the tool answers with the person's rights, i.e. not at all.
	if c := ans.ToolCalls[2]; c.OK {
		t.Errorf("get_project of another faculty's project must fail for this person: %+v", c)
	}

	// Only read tools were ever offered.
	for _, names := range llm.offered[:1] {
		if len(names) != len(assistant.ReadTools) {
			t.Errorf("offered %v, want exactly the read tools %v", names, assistant.ReadTools)
		}
		for _, n := range names {
			if n == "rename_project" || n == "create_project" || n == "approve_request" {
				t.Errorf("write tool %s offered", n)
			}
		}
	}

	var node model.Node
	mustDecode(t, do(t, h, http.MethodGet, "/v1/nodes/p_001", "", nil), &node)
	if node.Name != "Faculty research sandbox" {
		t.Errorf("p_001 was renamed to %q", node.Name)
	}
}

func TestAssistant_EmptyQuestion(t *testing.T) {
	h := assistantRouter(t, &scriptedModel{}, nil)
	assertStatus(t, do(t, h, http.MethodPost, "/v1/assistant/ask", "", map[string]any{"question": "   "}), http.StatusBadRequest)
}

func TestAssistant_QuotaPerPerson(t *testing.T) {
	h := assistantRouter(t, &scriptedModel{}, assistant.NewLimiter(1, 0))
	assertStatus(t, do(t, h, http.MethodPost, "/v1/assistant/ask", "", map[string]any{"question": "eins"}), http.StatusOK)
	rr := do(t, h, http.MethodPost, "/v1/assistant/ask", "", map[string]any{"question": "zwei"})
	assertStatus(t, rr, http.StatusTooManyRequests)
	if rr.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
}

// A read-only API token may ask (decided 25.11.2026): the assistant builds its tools read-only whatever the token, so the POST is a read. Every other POST stays refused for it.
func TestAssistant_ReadOnlyTokenMayAsk(t *testing.T) {
	tr := newTestTree(t, mockNodes(), testAccounting)
	lookup := func(_ context.Context, secret string) (identity.TokenLookupResult, error) {
		if secret == readOnlySecret {
			return identity.TokenLookupResult{Found: true, Subject: "root.admin@uni.example", ReadOnly: true}, nil
		}
		return identity.TokenLookupResult{Found: false}, nil
	}
	resolve := func(_ context.Context, _ *common.UserClaims) (common.TokenList, error) { return rootAdminTokens, nil }
	h := tr.router(func(c *webserver.SetupConfig) {
		c.AuthMiddleware = identity.CombinedAuthMiddleware(nil, lookup, resolve, tr.log)
		c.API.Assistant = &webserver.AssistantConfig{Agent: &assistant.Agent{Model: &scriptedModel{}}}
	})
	post := func(path string, body string) int {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+readOnlySecret)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}
	if got := post("/v1/assistant/ask", `{"question":"Welche Projekte habe ich?"}`); got != http.StatusOK {
		t.Errorf("read-only token asking: %d, want 200", got)
	}
	if got := post("/v1/nodes/split", `{"limit":{"cores":1}}`); got != http.StatusForbidden {
		t.Errorf("read-only token on another POST: %d, want 403", got)
	}
}
