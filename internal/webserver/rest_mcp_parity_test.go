package webserver

import (
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/pfisterer/cloud-self-service-golib/mcpserve/mcptest"
	"github.com/pfisterer/cloud-self-service-golib/token"
	"go.uber.org/zap"
)

// The REST routes and the MCP tools are two doors to the same service, and a door added on one side is easy to forget on the other. Like mcp_input_drift_test.go for the fields of a request, this is what says so for the operations: every route under /v1 is answered for, by the tools that do what it does or with a written reason why an agent does not get it. A new route matches neither and turns this red, which is the point: not to force a tool, but to force somebody to decide. A tool no route stands behind, and an entry for a route that is gone, are drift of their own.
//
// routeDecisions maps every route under /v1, "METHOD /path" as gin registers it, to the tools that do what it does or to the reason an agent is not given it.
var routeDecisions = map[string]mcptest.Route{
	"GET /v1/config":            {Omitted: "the UI's form configuration (resources, roles, caps); a budget tells a model its available_resources itself"},
	"GET /v1/act-as":            {Omitted: "whether the person may act for somebody else; an agent acts as the owner of its token, and only as them"},
	"GET /v1/principals/search": {Omitted: "the directory is not browsed by an agent; admins are given as the tokens the person names"},

	"GET /v1/nodes/mine":                            {Tools: []string{"list_my_projects"}},
	"GET /v1/nodes/to-manage":                       {Tools: []string{"list_requests_to_decide"}},
	"GET /v1/nodes/my-budgets":                      {Tools: []string{"list_my_budgets"}},
	"GET /v1/nodes/eligible-for-me":                 {Tools: []string{"list_requestable_budgets"}},
	"GET /v1/nodes/eligible-for-owner":              {Omitted: "the root admins' promote flow in the UI; adopt_imported_project takes the budget by id"},
	"GET /v1/nodes/search":                          {Tools: []string{"search_projects"}},
	"GET /v1/nodes/:id":                             {Tools: []string{"get_project"}},
	"GET /v1/nodes/:id/children":                    {Tools: []string{"list_budget_contents"}},
	"POST /v1/nodes":                                {Tools: []string{"create_project", "create_budget"}},
	"PUT /v1/nodes/:id":                             {Tools: []string{"rename_project", "set_project_admins"}},
	"DELETE /v1/nodes/:id":                          {Tools: []string{"delete_budget"}},
	"POST /v1/nodes/:id/request-change":             {Tools: []string{"request_project_change"}},
	"POST /v1/nodes/:id/approve":                    {Tools: []string{"approve_request"}},
	"POST /v1/nodes/:id/reject":                     {Tools: []string{"reject_request"}},
	"POST /v1/nodes/:id/release":                    {Tools: []string{"release_project"}},
	"POST /v1/nodes/:id/deletion":                   {Tools: []string{"delete_project"}},
	"POST /v1/nodes/:id/reparent":                   {Tools: []string{"move_to_budget"}},
	"POST /v1/nodes/:id/transfer-owner":             {Tools: []string{"transfer_ownership"}},
	"PUT /v1/nodes/:id/allocations":                 {Tools: []string{"set_project_allocation"}},
	"PUT /v1/nodes/:id/attributes":                  {Omitted: "billing attributes are set by managers in the UI, where they see what the nodes below inherit"},
	"DELETE /v1/nodes/:id/external-groups/:groupId": {Omitted: "a group given access in OpenStack is removed in the UI, where the person sees who loses access"},
	"GET /v1/nodes/:id/allocation-sources":          {Omitted: "set_project_allocation names the budget; which ones the person manages, list_my_budgets tells"},
	"GET /v1/nodes/:id/usage":                       {Omitted: "usage reports are not offered to an agent yet"},
	"POST /v1/nodes/:id/promote":                    {Tools: []string{"adopt_imported_project"}},

	"GET /v1/tokens":        {Omitted: "an agent authenticates with a token; it neither lists, mints nor revokes them (see createToken: a token that mints tokens outlives every expiry)"},
	"POST /v1/tokens":       {Omitted: "see GET /v1/tokens"},
	"DELETE /v1/tokens/:id": {Omitted: "see GET /v1/tokens"},

	"GET /v1/admin/reconcile/status":      {Omitted: "root admin operation; not offered to an agent"},
	"POST /v1/admin/reconcile/trigger":    {Omitted: "root admin operation; not offered to an agent"},
	"GET /v1/admin/usage":                 {Omitted: "root admin operation; not offered to an agent"},
	"GET /v1/admin/catalog":               {Omitted: "root admin operation; not offered to an agent"},
	"POST /v1/admin/catalog":              {Omitted: "root admin operation; not offered to an agent"},
	"GET /v1/admin/catalog/:id":           {Omitted: "root admin operation; not offered to an agent"},
	"PUT /v1/admin/catalog/:id":           {Omitted: "root admin operation; not offered to an agent"},
	"POST /v1/admin/catalog/:id/withdraw": {Omitted: "root admin operation; not offered to an agent"},
	"POST /v1/admin/catalog/:id/restore":  {Omitted: "root admin operation; not offered to an agent"},
	"DELETE /v1/admin/catalog/:id":        {Omitted: "root admin operation; not offered to an agent"},
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

func TestRESTRoutesAreAnsweredForByMCPTools(t *testing.T) {
	routes := slices.Sorted(maps.Keys(registeredRoutes()))
	// The tools a write-capable caller is offered, read back over an in-memory session.
	tools := mcptest.ToolNames(t, mcptest.InMemory(t, newMCPServer(APIConfig{}, mcpCaller{}, zap.NewNop().Sugar())))
	mcptest.CheckRoutes(t, routes, tools, routeDecisions)
}
