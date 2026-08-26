package webserver_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/mockdata"
	"github.com/pfisterer/openstack-management-api/internal/roleprovider"
	"github.com/pfisterer/openstack-management-api/internal/tree"
	"github.com/pfisterer/openstack-management-api/internal/webserver"
)

// mcpTestServer serves the real router — /mcp included — behind the real token
// auth middleware, so the test goes through the same path an MCP client does.
func mcpTestServer(t *testing.T) *httptest.Server {
	srv, _ := mcpTestServerWithService(t)
	return srv
}

// mcpTestServerWithService is the same fixture, handing back the service too —
// which is how a test looks at what a tool actually wrote.
func mcpTestServerWithService(t *testing.T) (*httptest.Server, *tree.Service) {
	t.Helper()
	store, sugar := newTestStore(t)
	ids, nodes := mockdata.DefaultMockTreeState()
	if err := store.Seed(context.Background(), ids, nodes); err != nil {
		t.Fatalf("seed: %v", err)
	}
	svc := tree.NewService(store, roleprovider.NewMockRoleProvider(), quotaResourceIDs,
		rootAdminTokens, 10*time.Second, common.DefaultMaxAuthorizedUsers, testAccounting, sugar)
	if err := svc.Bootstrap(context.Background(), nil, nil); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	lookup := func(_ context.Context, secret string) (common.TokenLookupResult, error) {
		switch secret {
		case readOnlySecret:
			return common.TokenLookupResult{Found: true, Subject: "root.admin@uni.example", ReadOnly: true}, nil
		case writeSecret:
			return common.TokenLookupResult{Found: true, Subject: "root.admin@uni.example"}, nil
		}
		return common.TokenLookupResult{Found: false}, nil
	}
	resolve := func(_ context.Context, _ *common.UserClaims) (common.TokenList, error) {
		return rootAdminTokens, nil
	}

	h := webserver.SetupGinWebserver(webserver.SetupConfig{
		DevMode:         true,
		Log:             sugar,
		StaticConfig:    webserver.StaticConfig{},
		API:             webserver.APIConfig{Service: svc, RoleSwitchGroups: rootAdminTokens},
		RootAdminTokens: rootAdminTokens,
		AuthMiddleware:  webserver.CombinedAuthMiddleware(nil, lookup, resolve, sugar),
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, svc
}

// bearerTransport puts the API token on every request, the way an MCP client
// configured with one does.
type bearerTransport struct {
	secret string
	base   http.RoundTripper
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+b.secret)
	return b.base.RoundTrip(clone)
}

func mcpSession(t *testing.T, srv *httptest.Server, secret string) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	transport := &mcp.StreamableClientTransport{
		Endpoint:   srv.URL + "/mcp",
		HTTPClient: &http.Client{Transport: bearerTransport{secret: secret, base: http.DefaultTransport}},
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).
		Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect to /mcp with %s: %v", secret, err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func toolNames(t *testing.T, session *mcp.ClientSession) []string {
	t.Helper()
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	return names
}

// The whole reason the read-only rule moved off the HTTP method: every MCP call
// is a POST, so under the old code a read-only token could not even list tools.
func TestMCP_ReadOnlyTokenCanRead(t *testing.T) {
	session := mcpSession(t, mcpTestServer(t), readOnlySecret)

	names := toolNames(t, session)
	for _, want := range []string{"get_project", "list_my_budgets", "list_my_projects", "search_projects"} {
		if !slices.Contains(names, want) {
			t.Errorf("read tool %q missing from %v", want, names)
		}
	}

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "list_my_projects", Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("call list_my_projects: %v", err)
	}
	if res.IsError {
		t.Fatalf("list_my_projects reported an error: %+v", res.Content)
	}
}

// A mutating tool is left out rather than offered and refused: a model picks
// from what it is shown, and one that always fails invites retries.
func TestMCP_ReadOnlyTokenIsNotOfferedMutatingTools(t *testing.T) {
	session := mcpSession(t, mcpTestServer(t), readOnlySecret)

	if names := toolNames(t, session); slices.Contains(names, "rename_project") {
		t.Errorf("rename_project must not be offered to a read-only token, got %v", names)
	}
}

func TestMCP_WriteTokenIsOfferedMutatingTools(t *testing.T) {
	session := mcpSession(t, mcpTestServer(t), writeSecret)

	names := toolNames(t, session)
	for _, want := range []string{
		"create_project", "request_project_change", "approve_request", "reject_request", "rename_project",
		"create_budget", "move_to_budget", "transfer_ownership", "adopt_imported_project",
	} {
		if !slices.Contains(names, want) {
			t.Errorf("write tool %q missing for a write token, got %v", want, names)
		}
	}
}

// The read-only token must lose ALL of them, not just the one that was easy to
// remember when the list was short.
func TestMCP_ReadOnlyTokenLosesEveryWriteTool(t *testing.T) {
	readOnly := toolNames(t, mcpSession(t, mcpTestServer(t), readOnlySecret))
	write := toolNames(t, mcpSession(t, mcpTestServer(t), writeSecret))

	for _, name := range write {
		if !slices.Contains(readOnly, name) {
			continue // correctly withheld
		}
		// Present for both: it must be one of the reads.
		if !slices.Contains([]string{"get_project", "list_my_budgets", "list_my_projects", "search_projects"}, name) {
			t.Errorf("%q is offered to a read-only token but is not a read tool", name)
		}
	}
	if len(readOnly) >= len(write) {
		t.Errorf("read-only got %d tools, write got %d — the gate is not doing anything", len(readOnly), len(write))
	}
}

// Destructive tools exist, but a read-only token must never see them.
func TestMCP_DestructiveToolsNeedAWriteToken(t *testing.T) {
	write := toolNames(t, mcpSession(t, mcpTestServer(t), writeSecret))
	readOnly := toolNames(t, mcpSession(t, mcpTestServer(t), readOnlySecret))

	for _, name := range []string{"release_project", "delete_budget"} {
		if !slices.Contains(write, name) {
			t.Errorf("%q missing for a write token, got %v", name, write)
		}
		if slices.Contains(readOnly, name) {
			t.Errorf("%q offered to a read-only token", name)
		}
	}
}

// The confirmation is the point: a wrong id with a plausible-sounding name must
// not destroy anything. This is what catches a model that resolved "the old one"
// to the wrong node — not prompt injection, which can echo a name as easily as
// an id.
//
// Runs against a budget the caller manages, and asserts the CONFIRMATION is what
// refused it: the service would reject this particular delete anyway, and a test
// that accepted any error would pass even with the check removed.
func TestMCP_DestructiveToolsRefuseAMismatchedName(t *testing.T) {
	session := mcpSession(t, mcpTestServer(t), writeSecret)

	budgets, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "list_my_budgets", Arguments: map[string]any{},
	})
	if err != nil || budgets.IsError {
		t.Fatalf("list budgets: %v %+v", err, budgets)
	}
	var list struct {
		Projects []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(mustJSON(t, budgets.StructuredContent), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list.Projects) == 0 {
		t.Fatal("the test identity manages no budget; the fixture cannot exercise this")
	}
	target := list.Projects[0]

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "delete_budget",
		Arguments: map[string]any{"id": target.ID, "confirm_name": target.Name + " (wrong)"},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError {
		t.Fatalf("a mismatched confirm_name deleted %q anyway", target.ID)
	}
	if msg := mustText(t, res); !strings.Contains(msg, "confirm_name") {
		t.Errorf("refused for the wrong reason: %s", msg)
	}
}

// mustText returns the text a failing tool call reported.
func mustText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// Without a token the endpoint must not answer at all — the same middleware
// that guards /v1.
func TestMCP_RequiresAuthentication(t *testing.T) {
	srv := mcpTestServer(t)

	res, err := http.Post(srv.URL+"/mcp", "application/json", http.NoBody)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("got %d, want 401", res.StatusCode)
	}
}

// What a tool declares as required is the difference between a model getting it
// right first time and finding out by being refused. create_budget is the case
// that taught this: admin_scope was optional in the schema and mandatory in the
// service, so the first real call failed.
func TestMCP_ToolsDeclareTheirRequiredArguments(t *testing.T) {
	session := mcpSession(t, mcpTestServer(t), writeSecret)
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}

	want := map[string][]string{
		"create_budget":          {"parent_id", "name", "reason", "limit", "admin_scope"},
		"create_project":         {"budget_id", "name", "reason", "limit"},
		"release_project":        {"id", "confirm_name"},
		"delete_budget":          {"id", "confirm_name"},
		"rename_project":         {"id", "name"},
		"move_to_budget":         {"id", "new_budget_id"},
		"transfer_ownership":     {"id", "new_owner"},
		"request_project_change": {"id", "limit"},
		"get_project":            {"id"},
		"search_projects":        {"query"},
		"list_my_projects":       {},
		"list_my_budgets":        {},
	}

	for _, tool := range res.Tools {
		expected, checked := want[tool.Name]
		if !checked {
			continue
		}
		t.Run(tool.Name, func(t *testing.T) {
			schema, err := json.Marshal(tool.InputSchema)
			if err != nil {
				t.Fatalf("marshal schema: %v", err)
			}
			var parsed struct {
				Required []string `json:"required"`
			}
			if err := json.Unmarshal(schema, &parsed); err != nil {
				t.Fatalf("decode schema: %v", err)
			}
			got := slices.Clone(parsed.Required)
			slices.Sort(got)
			expectedSorted := slices.Clone(expected)
			slices.Sort(expectedSorted)
			if !slices.Equal(got, expectedSorted) {
				t.Errorf("required = %v, want %v", got, expectedSorted)
			}
		})
	}
}

// The provenance half of the audit trail, end to end: a change made through a
// tool has to land in the history marked as such, while the same change from
// the UI does not.
//
// Worth going through the real endpoint rather than testing tree.Actor alone:
// the mechanism is one line in the tree package, but WHICH actor the MCP path
// builds is decided here, in serviceActor, and that is the part a future tool
// could get wrong.
func TestMCP_ChangesAreRecordedAsComingFromAnAgent(t *testing.T) {
	srv, svc := mcpTestServerWithService(t)
	session := mcpSession(t, srv, writeSecret)

	budgets, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "list_my_budgets", Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("list budgets: %v", err)
	}
	var list struct {
		Projects []struct {
			ID string `json:"id"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(mustJSON(t, budgets.StructuredContent), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list.Projects) == 0 {
		t.Fatal("the test identity manages no budget; the fixture cannot exercise this")
	}

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "create_project",
		Arguments: map[string]any{
			"budget_id": list.Projects[0].ID,
			"name":      "provenance-probe",
			"reason":    "checking that the history says where this came from",
			"limit":     map[string]any{"cores": 1},
		},
	})
	if err != nil {
		t.Fatalf("create_project: %v", err)
	}
	if res.IsError {
		t.Fatalf("create_project failed: %+v", res.Content)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(mustJSON(t, res.StructuredContent), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	node, err := svc.GetNode(created.ID, rootAdminTokens)
	if err != nil || node == nil {
		t.Fatalf("read back %q: %v", created.ID, err)
	}
	if len(node.History) == 0 {
		t.Fatal("the new project has no history")
	}
	entry := node.History[0]
	if entry.Via != tree.ChannelMCP {
		t.Errorf("history says via=%q, want %q — an agent's change is indistinguishable from a person's",
			entry.Via, tree.ChannelMCP)
	}
	if entry.Actor == "" {
		t.Error("history has no actor")
	}
}
