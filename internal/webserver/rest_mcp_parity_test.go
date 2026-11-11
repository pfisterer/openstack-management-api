package webserver

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pfisterer/cloud-self-service-golib/token"
	"go.uber.org/zap"
)

// The REST routes and the MCP tools are two doors to the same service, and a door added on one side is easy to forget on the other. Like mcp_input_drift_test.go for the fields of a request, this is what says so for the operations: every route under /v1 is answered for, by the tools that do what it does or with a written reason why an agent does not get it. A new route matches neither and turns this red, which is the point: not to force a tool, but to force somebody to decide. A tool no route stands behind, and an entry for a route that is gone, are drift of their own.
type routeContract struct {
	// route is the method and path as gin registers them.
	route string
	// tools do what the route does.
	tools []string
	// omitted says why an agent is not given it.
	omitted string
}

var routeContracts = []routeContract{
	{route: "GET /v1/config", omitted: "the UI's form configuration (resources, roles, caps); a budget tells a model its available_resources itself"},
	{route: "GET /v1/act-as", omitted: "whether the person may act for somebody else; an agent acts as the owner of its token, and only as them"},
	{route: "GET /v1/principals/search", omitted: "the directory is not browsed by an agent; admins are given as the tokens the person names"},

	{route: "GET /v1/nodes/mine", tools: []string{"list_my_projects"}},
	{route: "GET /v1/nodes/to-manage", tools: []string{"list_requests_to_decide"}},
	{route: "GET /v1/nodes/my-budgets", tools: []string{"list_my_budgets"}},
	{route: "GET /v1/nodes/eligible-for-me", tools: []string{"list_requestable_budgets"}},
	{route: "GET /v1/nodes/eligible-for-owner", omitted: "the root admins' promote flow in the UI; adopt_imported_project takes the budget by id"},
	{route: "GET /v1/nodes/search", tools: []string{"search_projects"}},
	{route: "GET /v1/nodes/:id", tools: []string{"get_project"}},
	{route: "GET /v1/nodes/:id/children", tools: []string{"list_budget_contents"}},
	{route: "POST /v1/nodes", tools: []string{"create_project", "create_budget"}},
	{route: "PUT /v1/nodes/:id", tools: []string{"rename_project", "set_project_admins"}},
	{route: "DELETE /v1/nodes/:id", tools: []string{"delete_budget"}},
	{route: "POST /v1/nodes/:id/request-change", tools: []string{"request_project_change"}},
	{route: "POST /v1/nodes/:id/approve", tools: []string{"approve_request"}},
	{route: "POST /v1/nodes/:id/reject", tools: []string{"reject_request"}},
	{route: "POST /v1/nodes/:id/release", tools: []string{"release_project"}},
	{route: "POST /v1/nodes/:id/deletion", tools: []string{"delete_project"}},
	{route: "POST /v1/nodes/:id/reparent", tools: []string{"move_to_budget"}},
	{route: "POST /v1/nodes/:id/transfer-owner", tools: []string{"transfer_ownership"}},
	{route: "PUT /v1/nodes/:id/allocations", tools: []string{"set_project_allocation"}},
	{route: "PUT /v1/nodes/:id/attributes", omitted: "billing attributes are set by managers in the UI, where they see what the nodes below inherit"},
	{route: "DELETE /v1/nodes/:id/external-groups/:groupId", omitted: "a group given access in OpenStack is removed in the UI, where the person sees who loses access"},
	{route: "GET /v1/nodes/:id/allocation-sources", omitted: "set_project_allocation names the budget; which ones the person manages, list_my_budgets tells"},
	{route: "GET /v1/nodes/:id/usage", omitted: "usage reports are not offered to an agent yet"},
	{route: "POST /v1/nodes/:id/promote", tools: []string{"adopt_imported_project"}},

	{route: "GET /v1/tokens", omitted: "an agent authenticates with a token; it neither lists, mints nor revokes them (see createToken: a token that mints tokens outlives every expiry)"},
	{route: "POST /v1/tokens", omitted: "see GET /v1/tokens"},
	{route: "DELETE /v1/tokens/:id", omitted: "see GET /v1/tokens"},

	{route: "GET /v1/admin/reconcile/status", omitted: "root admin operation; not offered to an agent"},
	{route: "POST /v1/admin/reconcile/trigger", omitted: "root admin operation; not offered to an agent"},
	{route: "GET /v1/admin/usage", omitted: "root admin operation; not offered to an agent"},
	{route: "GET /v1/admin/catalog", omitted: "root admin operation; not offered to an agent"},
	{route: "POST /v1/admin/catalog", omitted: "root admin operation; not offered to an agent"},
	{route: "GET /v1/admin/catalog/:id", omitted: "root admin operation; not offered to an agent"},
	{route: "PUT /v1/admin/catalog/:id", omitted: "root admin operation; not offered to an agent"},
	{route: "POST /v1/admin/catalog/:id/withdraw", omitted: "root admin operation; not offered to an agent"},
	{route: "POST /v1/admin/catalog/:id/restore", omitted: "root admin operation; not offered to an agent"},
	{route: "DELETE /v1/admin/catalog/:id", omitted: "root admin operation; not offered to an agent"},
}

// registeredRoutes are the routes under /v1 as the router registers them, "METHOD /path". The handlers never run, so the config only has to make every group register: a token service, or /v1/tokens stays off.
func registeredRoutes() map[string]bool {
	router := SetupGinWebserver(SetupConfig{
		Log:    zap.NewNop().Sugar(),
		Tokens: TokenConfig{Service: token.NewService("os_mgt_", token.NewMemoryStore())},
	})
	routes := map[string]bool{}
	for _, r := range router.Routes() {
		// OPTIONS /v1/*path is the CORS preflight catch-all, not an operation.
		if strings.HasPrefix(r.Path, "/v1/") && r.Method != http.MethodOptions {
			routes[r.Method+" "+r.Path] = true
		}
	}
	return routes
}

// registeredTools are the names of the tools a write-capable caller is offered, read back over an in-memory session.
func registeredTools(t *testing.T) []string {
	t.Helper()
	ctx := context.Background()
	clientSide, serverSide := mcp.NewInMemoryTransports()
	if _, err := newMCPServer(APIConfig{}, mcpCaller{}, zap.NewNop().Sugar()).Connect(ctx, serverSide, nil); err != nil {
		t.Fatalf("connect server: %v", err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientSide, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	defer session.Close()
	res, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func TestRESTRoutesAreAnsweredForByMCPTools(t *testing.T) {
	routes := registeredRoutes()
	tools := registeredTools(t)

	contracted := map[string]bool{}
	behindARoute := map[string]bool{}
	for _, c := range routeContracts {
		contracted[c.route] = true
		if !routes[c.route] {
			t.Errorf("%s is listed, but no such route is registered", c.route)
		}
		if (len(c.tools) == 0) == (c.omitted == "") {
			t.Errorf("%s: name the tools that do what it does, or the reason an agent does not get it; one of the two", c.route)
		}
		for _, tool := range c.tools {
			if !slices.Contains(tools, tool) {
				t.Errorf("%s is mapped to %q, but no such tool is registered", c.route, tool)
			}
			behindARoute[tool] = true
		}
	}

	for route := range routes {
		if !contracted[route] {
			t.Errorf("%s reaches no tool.\nMap it to the tools that do what it does, or write down under omitted why an agent does not get it.", route)
		}
	}
	// A tool with no route behind it does something the API itself cannot, which is not what the tools are for.
	for _, tool := range tools {
		if !behindARoute[tool] {
			t.Errorf("tool %q stands behind no REST route", tool)
		}
	}
}
