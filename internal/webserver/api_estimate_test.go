package webserver_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pfisterer/cloud-self-service-golib/mcpserve/mcptest"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/estimate"
	"github.com/pfisterer/openstack-management-api/internal/identity"
	"github.com/pfisterer/openstack-management-api/internal/webserver"
)

// The flavours of the tests: two public, one private granted by the catalogue's only availability (dhbw-ipv4 stands in for a flavour availability here: what counts is that a budget holds it).
var estimateFlavors = estimate.StaticFlavors{
	{ID: "f-small", Name: "m1.small", VCPUs: 1, RAMMB: 2048, DiskGB: 20, Public: true},
	{ID: "f-large", Name: "m1.large", VCPUs: 4, RAMMB: 8192, DiskGB: 40, Public: true},
	{ID: "f-gpu", Name: "g1.gpu", VCPUs: 8, RAMMB: 32768, GPUs: 1},
}

type stubCompleter struct {
	answer string
	err    error
}

func (s stubCompleter) Complete(context.Context, string, string, map[string]any) (string, error) {
	return s.answer, s.err
}

func estimateRouter(t *testing.T, llm estimate.Completer) http.Handler {
	t.Helper()
	cfg := estimate.DefaultConfig()
	ec := &webserver.EstimateConfig{
		Estimator:    estimate.New(cfg, nil),
		Flavors:      estimateFlavors,
		FlavorGrants: func() map[string]string { return map[string]string{"f-gpu": "dhbw-ipv4"} },
	}
	if llm != nil {
		ec.Describer = estimate.NewDescriber(cfg, llm)
	}
	return newTestTree(t, mockNodes(), testAccounting).router(func(c *webserver.SetupConfig) { c.API.Estimate = ec })
}

func flavorNames(fs []estimate.Flavor) string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Name)
	}
	return strings.Join(out, ",")
}

func TestEstimate_OptionsListOnlyAllowedFlavors(t *testing.T) {
	h := estimateRouter(t, nil)
	rr := do(t, h, http.MethodGet, "/v1/estimate/options", userStudent, nil)
	assertStatus(t, rr, http.StatusOK)
	var opts webserver.EstimateOptions
	mustDecode(t, rr, &opts)
	if got := flavorNames(opts.Flavors); got != "m1.small,m1.large" {
		t.Errorf("a student's flavours = %s, want the public ones only", got)
	}
	if len(opts.Tools) != 4 || opts.DescribeEnabled || !opts.FlavorsAvailable || opts.ProfilesVersion == "" {
		t.Errorf("options = %+v", opts)
	}
	if opts.Tools[2].ID != "code" || !strings.Contains(opts.Tools[2].Name, "Code-Assistent") {
		t.Errorf("tool names in German by default: %+v", opts.Tools[2])
	}

	// The root holds the whole catalogue, so under it the private flavour is allowed.
	rr = do(t, h, http.MethodGet, "/v1/estimate/options?budget_id="+model.RootNodeID, "", nil)
	assertStatus(t, rr, http.StatusOK)
	mustDecode(t, rr, &opts)
	if got := flavorNames(opts.Flavors); got != "m1.small,m1.large,g1.gpu" {
		t.Errorf("flavours under the root = %s", got)
	}

	// A budget the caller may not read is refused, not silently ignored.
	rr = do(t, h, http.MethodGet, "/v1/estimate/options?budget_id=b_dept_bio", userStudent, nil)
	if rr.Code != http.StatusForbidden && rr.Code != http.StatusNotFound {
		t.Errorf("foreign budget: %d %s", rr.Code, rr.Body.String())
	}
}

func TestEstimate_CloudWithBudgetCheck(t *testing.T) {
	h := estimateRouter(t, nil)
	body := map[string]any{
		"cloud":     map[string]any{"flavors": []map[string]any{{"flavor": "m1.small", "count": 1}}, "volumes": []map[string]any{{"size_gb": 10, "count": 1}}},
		"budget_id": "b_cs_students",
	}
	rr := do(t, h, http.MethodPost, "/v1/estimate", userStudent, body)
	assertStatus(t, rr, http.StatusOK)
	var out webserver.EstimateResponse
	mustDecode(t, rr, &out)
	if out.Result.Limit["cores"] != 1 || out.Result.Limit["ram"] != 2 || out.Result.Limit["storage"] != 10 {
		t.Errorf("limit = %v", out.Result.Limit)
	}
	if out.Budget == nil || out.Budget.Plan == nil || !out.Budget.Fits || !out.Budget.AutoApproved {
		t.Fatalf("within the students' per-person limit everything is granted on the spot: %+v", out.Budget)
	}

	// Three large machines are beyond what the budget grants on its own: the rest would wait.
	body["cloud"] = map[string]any{"flavors": []map[string]any{{"flavor": "m1.large", "count": 3}}}
	rr = do(t, h, http.MethodPost, "/v1/estimate", userStudent, body)
	assertStatus(t, rr, http.StatusOK)
	mustDecode(t, rr, &out)
	if out.Result.Limit["cores"] != 12 || out.Budget == nil || out.Budget.AutoApproved {
		t.Errorf("12 cores: limit %v, budget %+v", out.Result.Limit, out.Budget)
	}

	// The private flavour is flagged for a student and its availability not asked for.
	body["cloud"] = map[string]any{"flavors": []map[string]any{{"flavor": "g1.gpu", "count": 1}}}
	delete(body, "budget_id")
	rr = do(t, h, http.MethodPost, "/v1/estimate", userStudent, body)
	assertStatus(t, rr, http.StatusOK)
	out = webserver.EstimateResponse{}
	mustDecode(t, rr, &out)
	if out.Result.Limit["dhbw-ipv4"] != 0 || out.Result.Limit["cores"] != 8 || len(out.Result.Warnings) == 0 || out.Result.Warnings[0].Code != "flavor_not_allowed" || out.Budget != nil {
		t.Errorf("private flavour for a student: %+v", out.Result)
	}

	// An unknown flavour is a 400 naming the possible ones.
	body["cloud"] = map[string]any{"flavors": []map[string]any{{"flavor": "m9.huge", "count": 1}}}
	rr = do(t, h, http.MethodPost, "/v1/estimate", userStudent, body)
	assertStatus(t, rr, http.StatusBadRequest)
	if !strings.Contains(rr.Body.String(), "m1.large") {
		t.Errorf("refusal names the allowed flavours: %s", rr.Body.String())
	}
}

func TestEstimate_LLMInEnglishAndUnknownResources(t *testing.T) {
	h := estimateRouter(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/estimate", strings.NewReader(`{"llm": {"tool": "code", "persons": 20, "sessions_per_week": 1, "hours_per_session": 3.5, "weeks": 12, "together": true}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Language", "en")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	assertStatus(t, rr, http.StatusOK)
	var out webserver.EstimateResponse
	mustDecode(t, rr, &out)
	if out.Result.LLM == nil || out.Result.LLM.MTokens != 49 || out.Result.LLM.ProjectTPM != 630000 {
		t.Fatalf("llm = %+v", out.Result.LLM)
	}
	// The test catalogue has no LLM resources: nothing proposed, each named.
	if len(out.Result.Limit) != 0 || len(out.Result.Warnings) != 3 || !strings.Contains(out.Result.Warnings[0].Text, "does not exist") {
		t.Errorf("limit %v, warnings %+v", out.Result.Limit, out.Result.Warnings)
	}
	if !strings.HasPrefix(out.Result.Assumptions[0].Text, "Tool code") {
		t.Errorf("assumptions in English: %q", out.Result.Assumptions[0].Text)
	}

	rr = do(t, h, http.MethodPost, "/v1/estimate", "", map[string]any{"llm": map[string]any{"tool": "code", "persons": 0}})
	assertStatus(t, rr, http.StatusBadRequest)
}

func TestEstimate_Describe(t *testing.T) {
	body := map[string]any{"description": "Ein Kurs mit 10 Personen, jeweils ein m1.small"}

	rr := do(t, estimateRouter(t, nil), http.MethodPost, "/v1/estimate/describe", userStudent, body)
	assertStatus(t, rr, http.StatusServiceUnavailable)
	if !strings.Contains(rr.Body.String(), "describe_unavailable") {
		t.Errorf("off without a model: %s", rr.Body.String())
	}

	answer := `{"cloud": {"flavors": [{"flavor": "m1.small", "count": 1}, {"flavor": "g1.gpu", "count": 1}], "per_person": true, "participants": 10}, "assumptions": ["Jede Person bekommt eine Maschine"]}`
	rr = do(t, estimateRouter(t, stubCompleter{answer: answer}), http.MethodPost, "/v1/estimate/describe", userStudent, body)
	assertStatus(t, rr, http.StatusOK)
	var out webserver.DescribeResponse
	mustDecode(t, rr, &out)
	if out.Result.Limit["cores"] != 10 || out.Result.Limit["ram"] != 20 {
		t.Errorf("computed from the model's input: %v", out.Result.Limit)
	}
	if len(out.Described.Corrections) != 1 || out.Described.Corrections[0].Code != "flavor_dropped" || out.Described.Assumptions[0] != "Jede Person bekommt eine Maschine" {
		t.Errorf("the flavour the student may not use is dropped: %+v", out.Described)
	}

	rr = do(t, estimateRouter(t, stubCompleter{err: errors.New("connection refused")}), http.MethodPost, "/v1/estimate/describe", userStudent, body)
	assertStatus(t, rr, http.StatusBadGateway)
	if strings.Contains(rr.Body.String(), "connection refused") {
		t.Error("the cause stays in the log")
	}

	rr = do(t, estimateRouter(t, stubCompleter{answer: "Keine Ahnung."}), http.MethodPost, "/v1/estimate/describe", userStudent, body)
	assertStatus(t, rr, http.StatusBadRequest)
}

func TestEstimate_NotSetUp(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodGet, "/v1/estimate/options", "", nil)
	assertStatus(t, rr, http.StatusServiceUnavailable)
	rr = do(t, h, http.MethodPost, "/v1/estimate", "", map[string]any{"llm": map[string]any{"tool": "chat", "persons": 1, "sessions_per_week": 1, "hours_per_session": 1, "weeks": 1}})
	assertStatus(t, rr, http.StatusServiceUnavailable)
	rr = do(t, h, http.MethodPost, "/v1/estimate/describe", "", map[string]any{"description": "x"})
	assertStatus(t, rr, http.StatusServiceUnavailable)
}

// The MCP tool runs the same estimate as the REST route, as the token's owner; a read-only token may use it.
func TestEstimate_MCPTool(t *testing.T) {
	tr := newTestTree(t, mockNodes(), testAccounting)
	lookup := func(_ context.Context, secret string) (identity.TokenLookupResult, error) {
		return identity.TokenLookupResult{Found: secret == readOnlySecret, Subject: "root.admin@uni.example", ReadOnly: true}, nil
	}
	resolve := func(_ context.Context, _ *common.UserClaims) (common.TokenList, error) { return rootAdminTokens, nil }
	h := tr.router(func(c *webserver.SetupConfig) {
		c.AuthMiddleware = identity.CombinedAuthMiddleware(nil, lookup, resolve, tr.log)
		c.API.Estimate = &webserver.EstimateConfig{Estimator: estimate.New(estimate.DefaultConfig(), nil), Flavors: estimateFlavors}
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	session := mcptest.Dial(t, srv.URL+"/mcp", readOnlySecret)

	res := mcptest.Call(t, session, "estimate_resources", map[string]any{
		"workspaces": map[string]any{"kind": "cpu", "persons": 20, "sessions_per_week": 1, "hours_per_session": 3.5, "weeks": 12, "together": true},
		"budget_id":  model.RootNodeID,
	})
	if res.IsError {
		t.Fatalf("estimate_resources: %s", mcptest.Text(res))
	}
	var out webserver.EstimateResponse
	decodeResult(t, res, &out)
	if out.Result.Workspaces == nil || out.Result.Workspaces.VCPUHours != 180 || out.Budget == nil {
		t.Errorf("estimate = %+v", out)
	}

	res = mcptest.Call(t, session, "estimate_resources", map[string]any{"llm": map[string]any{"tool": "nonsense", "persons": 1, "sessions_per_week": 1, "hours_per_session": 1, "weeks": 1, "together": false}})
	if !res.IsError || !strings.Contains(mcptest.Text(res), "llm.tool") {
		t.Errorf("an invalid input is reported: %s", mcptest.Text(res))
	}

	res = mcptest.Call(t, session, "estimate_options", map[string]any{})
	if res.IsError {
		t.Fatalf("estimate_options: %s", mcptest.Text(res))
	}
}

// The options say which measurement the estimate uses and what was measured of each tool's alias.
func TestEstimate_OptionsCarryTheMeasurement(t *testing.T) {
	est := estimate.New(estimate.DefaultConfig(), nil)
	est.SetMeasured(&estimate.Measured{MinKeys: 5, Models: map[string]estimate.MeasuredModel{"code": {Keys: 12, RequestsPerKeyHour: &estimate.MeasuredStat{Median: 6, P90: 20, Samples: 40, Keys: 12}}}})
	h := newTestTree(t, mockNodes(), testAccounting).router(func(c *webserver.SetupConfig) {
		c.API.Estimate = &webserver.EstimateConfig{Estimator: est, Flavors: estimateFlavors}
	})
	rr := do(t, h, http.MethodGet, "/v1/estimate/options", "", nil)
	assertStatus(t, rr, http.StatusOK)
	var opts webserver.EstimateOptions
	mustDecode(t, rr, &opts)
	if opts.Measurement == nil || opts.Measurement.MinKeys != 5 {
		t.Fatalf("measurement = %+v", opts.Measurement)
	}
	for _, tool := range opts.Tools {
		if (tool.ID == "code") != (tool.Measured != nil) {
			t.Errorf("tool %s measured = %+v", tool.ID, tool.Measured)
		}
	}
}
