package webserver

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/cloud-self-service-golib/apierr"
	"github.com/pfisterer/cloud-self-service-golib/ginweb"
	"github.com/pfisterer/cloud-self-service-golib/msg"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/tree"
	"github.com/pfisterer/openstack-management-api/internal/estimate"
	"github.com/pfisterer/openstack-management-api/internal/identity"
)

// The resource estimator (Schätzhelfer, package estimate): computes what a project would ask for from a description of its use, and what a budget would grant of it. Nothing is written. The handlers gather what the estimate needs to know of the person — the flavours they may use (public ones, and private ones a budget of theirs holds), the catalogue — and ask the tree's split for the budget check, so the eligibility and limit rules are the ones a request meets.

// EstimateConfig wires the estimator.
type EstimateConfig struct {
	Estimator *estimate.Estimator
	// Describer reads free-text descriptions; nil when no model is configured.
	Describer *estimate.Describer
	// Flavors lists OpenStack's flavours; nil for none.
	Flavors estimate.FlavorSource
	// FlavorGrants maps a private flavour's OpenStack id to the catalogue availability granting it, read per request (root admins change the catalogue at runtime).
	FlavorGrants func() map[string]string
}

// EstimateRequest is an estimate's input, optionally with the budget to check it against.
type EstimateRequest struct {
	estimate.Input
	// BudgetID is the budget to check the estimate against (what it would grant on its own, what would wait for a decision); empty for no check. Its private flavours count as allowed.
	BudgetID string `json:"budget_id,omitempty"`
	// TerminationDate is the intended end, for the budget check.
	TerminationDate *string `json:"termination_date,omitempty"`
}

// DescribeRequest is a description in free text.
type DescribeRequest struct {
	Description     string  `json:"description" binding:"required" example:"20 students, R course with Positron, Mondays 3.5 h, 12 weeks"`
	BudgetID        string  `json:"budget_id,omitempty"`
	TerminationDate *string `json:"termination_date,omitempty"`
}

// BudgetCheck is what the budget makes of the estimate: the split the request would get there (tree's split, restricted to this budget).
type BudgetCheck struct {
	BudgetID string `json:"budget_id"`
	// Plan is the allocation the budget would take: auto_approved is granted on the spot, the rest of limit waits for its managers; unplaced is what it would not even take as a request. Missing when Error is set.
	Plan *tree.SplitPlan `json:"plan,omitempty"`
	// Fits: everything is placed with this budget. AutoApproved: everything is granted without a decision.
	Fits         bool `json:"fits"`
	AutoApproved bool `json:"auto_approved"`
	// Error is why the budget could not be asked (no right to request there, a value beyond what a project may hold).
	Error string `json:"error,omitempty"`
}

// EstimateResponse is the estimate with its input and the budget check.
type EstimateResponse struct {
	Input  estimate.Input  `json:"input"`
	Result estimate.Result `json:"result"`
	Budget *BudgetCheck    `json:"budget,omitempty"`
}

// DescribeResponse is the model's reading of a description and the estimate computed from it.
type DescribeResponse struct {
	Described estimate.Described `json:"described"`
	Result    estimate.Result    `json:"result"`
	Budget    *BudgetCheck       `json:"budget,omitempty"`
}

// EstimateOptions is what an estimate form offers: the tools and workspace kinds with their profile numbers, the flavours the person may use, the GPU classes, whether the description is available.
type EstimateOptions struct {
	ProfilesVersion  string                  `json:"profiles_version"`
	Source           string                  `json:"source"`
	Tools            []EstimateTool          `json:"tools"`
	WorkspaceKinds   []EstimateWorkspaceKind `json:"workspace_kinds"`
	Flavors          []estimate.Flavor       `json:"flavors"`
	FlavorsAvailable bool                    `json:"flavors_available"`
	GPUClasses       []string                `json:"gpu_classes"`
	DescribeEnabled  bool                    `json:"describe_enabled"`
	KeyWindow        estimate.KeyWindow      `json:"key_window"`
	WindowDays       float64                 `json:"window_days"`
	Pricing          estimate.PricingConfig  `json:"pricing"`
	// Measurement is the measured usage profiles in use; nil = only the configured profiles.
	Measurement *EstimateMeasurement `json:"measurement,omitempty"`
}

type EstimateTool struct {
	ID                     string        `json:"id"`
	Name                   string        `json:"name"`
	Model                  string        `json:"model"`
	InputTokensPerRequest  float64       `json:"input_tokens_per_request"`
	OutputTokensPerRequest float64       `json:"output_tokens_per_request"`
	RequestsPerHour        estimate.Pair `json:"requests_per_hour"`
	PeakTPMPerPerson       estimate.Pair `json:"peak_tpm_per_person"`
	ConcurrentShare        float64       `json:"concurrent_share"`
	Source                 string        `json:"source"`
	// Measured is what the AI service measured of the tool's model alias (figures with too few keys missing); nil when nothing was. Where a figure is there, the estimate uses it instead of the profile's number.
	Measured *estimate.MeasuredModel `json:"measured,omitempty"`
}

// EstimateMeasurement says which measurement the estimate uses: the time range of LiteLLM's spend logs, when the AI service computed it, the keys a figure needs, when it was fetched.
type EstimateMeasurement struct {
	From       time.Time `json:"from"`
	To         time.Time `json:"to"`
	ComputedAt time.Time `json:"computed_at"`
	FetchedAt  time.Time `json:"fetched_at"`
	MinKeys    int       `json:"min_keys"`
}

type EstimateWorkspaceKind struct {
	ID string `json:"id"`
	estimate.WorkspaceKind
}

// estimateErrors answers the estimator's own conditions, everything else as the tree's operations.
var estimateErrors = apierr.Classifier{
	Rules: append([]apierr.Rule{
		apierr.Is(estimate.ErrUnavailable, http.StatusServiceUnavailable, "estimator_unavailable", msg.T("Der Schätzhelfer ist hier nicht eingerichtet.", "The estimator is not set up here.")),
		apierr.Is(estimate.ErrDescriptionOff, http.StatusServiceUnavailable, "describe_unavailable", msg.T("Die Beschreibung in Freitext ist hier nicht eingerichtet; die Felder lassen sich direkt ausfüllen.", "The free-text description is not set up here; the fields can be filled in directly.")),
		apierr.Is(estimate.ErrModelUnavailable, http.StatusBadGateway, "model_unavailable", msg.T("Das Sprachmodell antwortet gerade nicht. Bitte später noch einmal versuchen oder die Felder direkt ausfüllen.", "The language model does not answer right now. Please try again later or fill in the fields directly.")),
	}, serviceErrors.Rules...),
	Fallback: serviceErrors.Fallback,
}

// estimateHandle is handle with the estimator's error answers.
func estimateHandle[Req, Out any](cfg APIConfig, read bodyReader[Req], call func(c *gin.Context, req Req, auth identity.AuthContext) (Out, error)) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req Req
		if !read(c, &req) {
			return
		}
		auth, ok := caller(c)
		if !ok {
			return
		}
		out, err := call(c, req, auth)
		if err != nil {
			e := estimateErrors.Classify(err)
			// Not set up is a state of the deployment, not a failure worth a log line per request.
			if e.Status == http.StatusServiceUnavailable {
				ginweb.AbortError(c, e)
				return
			}
			cfg.respond(c, e)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

// getEstimateOptions lists what an estimate can be made of.
//
//	@Summary		Options of the resource estimator
//	@Description	The tools (LLM usage profiles with their numbers, date and source), workspace kinds, the flavours the caller may use (public ones and private ones held by budget_id, or by any budget the caller may request from or manages), GPU classes, and whether the free-text description is available. Names in the language of Accept-Language.
//	@Tags			estimate
//	@Produce		json
//	@Security		Bearer
//	@Param			budget_id	query		string	false	"Budget whose private flavours count"
//	@Success		200			{object}	EstimateOptions
//	@Failure		401			{object}	map[string]any	"Unauthorized."
//	@Failure		403			{object}	map[string]any	"Forbidden."
//	@Failure		503			{object}	map[string]any	"Estimator not set up."
//	@ID				getEstimateOptions
//	@Router			/v1/estimate/options [get]
func getEstimateOptions(cfg APIConfig) gin.HandlerFunc {
	return estimateHandle(cfg, noBody[struct{}], func(c *gin.Context, _ struct{}, auth identity.AuthContext) (EstimateOptions, error) {
		return cfg.estimateOptions(c.Request.Context(), auth.EffectiveTokens, c.Query("budget_id"), msg.FromRequest(c.Request))
	})
}

// postEstimate computes an estimate.
//
//	@Summary		Estimate the resources of a project
//	@Description	Computes deterministically what a project would ask for from its intended use: LLM (tool, people, sessions per week × hours, weeks → tokens per 30 days as llm-mtokens at the reference price, project tokens and requests per minute, a key's 4-hour share), cloud (flavours × count, volumes, optionally per participant and with hours → cores, RAM, storage, private flavour availabilities, vCPU hours for information) and workspaces (kind, people, times, storage → vCPU, RAM and GPU hours per 30 days, GPUs at once, storage). result.limit holds the proposed amounts by resource id, ready for a request; result.assumptions and result.warnings say what was assumed, in the language of Accept-Language. With budget_id, budget says what that budget would grant on its own and what would wait for a decision (the split of POST /v1/nodes/split). Nothing is written.
//	@Tags			estimate
//	@Accept			json
//	@Produce		json
//	@Security		Bearer
//	@Param			request	body		EstimateRequest	true	"Intended use"
//	@Success		200		{object}	EstimateResponse
//	@Failure		400		{object}	map[string]any	"Invalid input."
//	@Failure		401		{object}	map[string]any	"Unauthorized."
//	@Failure		403		{object}	map[string]any	"Forbidden."
//	@Failure		503		{object}	map[string]any	"Estimator not set up."
//	@ID				postEstimate
//	@Router			/v1/estimate [post]
func postEstimate(cfg APIConfig) gin.HandlerFunc {
	return estimateHandle(cfg, jsonBody[EstimateRequest](msg.Text{}), func(c *gin.Context, req EstimateRequest, auth identity.AuthContext) (EstimateResponse, error) {
		return cfg.runEstimate(c.Request.Context(), auth, req, msg.FromRequest(c.Request))
	})
}

// postEstimateDescribe estimates from a description in free text.
//
//	@Summary		Estimate from a description in free text
//	@Description	The platform's language model reads the description and fills only the estimator's input (tool, people, times, flavours from the allowed list) with its assumptions; the numbers are computed by the estimator as in POST /v1/estimate. What the model returns is checked: a part that does not make sense is dropped and named in described.corrections. 503 while no model is configured, 502 when the model does not answer, 400 when nothing usable came back.
//	@Tags			estimate
//	@Accept			json
//	@Produce		json
//	@Security		Bearer
//	@Param			request	body		DescribeRequest	true	"Description"
//	@Success		200		{object}	DescribeResponse
//	@Failure		400		{object}	map[string]any	"Empty, too long or not understood."
//	@Failure		401		{object}	map[string]any	"Unauthorized."
//	@Failure		502		{object}	map[string]any	"The model did not answer."
//	@Failure		503		{object}	map[string]any	"Not configured."
//	@ID				postEstimateDescribe
//	@Router			/v1/estimate/describe [post]
func postEstimateDescribe(cfg APIConfig) gin.HandlerFunc {
	return estimateHandle(cfg, jsonBody[DescribeRequest](msg.Text{}), func(c *gin.Context, req DescribeRequest, auth identity.AuthContext) (DescribeResponse, error) {
		return cfg.runDescribe(c.Request.Context(), auth, req, msg.FromRequest(c.Request))
	})
}

// estimateEnv is what the estimate knows of the deployment and the caller: the catalogue, all flavours and those the caller may use under budgetID (or under any budget of theirs).
func (cfg APIConfig) estimateEnv(ctx context.Context, tokens common.TokenList, budgetID string) (estimate.Env, bool, error) {
	if cfg.Estimate == nil || cfg.Estimate.Estimator == nil {
		return estimate.Env{}, false, estimate.ErrUnavailable
	}
	env := estimate.Env{}
	if cfg.Catalog != nil {
		env.Catalog = cfg.Catalog.Resources()
	}
	held, err := cfg.heldAvailabilities(tokens, budgetID)
	if err != nil {
		return estimate.Env{}, false, err
	}
	available := false
	if cfg.Estimate.Flavors != nil {
		if fs, err := cfg.Estimate.Flavors.Flavors(ctx); err == nil {
			env.AllFlavors, available = fs, true
		}
	}
	var grants map[string]string
	if cfg.Estimate.FlavorGrants != nil {
		grants = cfg.Estimate.FlavorGrants()
	}
	env.AllowedFlavors = estimate.AllowedFlavors(env.AllFlavors, grants, held)
	return env, available, nil
}

// heldAvailabilities are the availabilities of the budget (which the caller has to be able to read), or of every budget the caller may request from or manages.
func (cfg APIConfig) heldAvailabilities(tokens common.TokenList, budgetID string) (map[string]bool, error) {
	held := map[string]bool{}
	add := func(n model.Node) {
		for id, v := range n.Limit {
			if v == 1 {
				held[id] = true
			}
		}
	}
	if cfg.Service == nil {
		return held, nil
	}
	if budgetID = strings.TrimSpace(budgetID); budgetID != "" {
		n, err := cfg.Service.GetNode(budgetID, tokens)
		if err != nil {
			return nil, err
		}
		if n.Kind != model.KindBudget {
			return nil, msg.Errorf("%s ist kein Budget.", "%s is not a budget.", budgetID)
		}
		add(*n)
		return held, nil
	}
	for _, list := range []func(common.TokenList, int, int) (tree.NodePage, error){cfg.Service.ListEligibleForMe, cfg.Service.ListMyBudgets} {
		p, err := list(tokens, 500, 0)
		if err != nil {
			return nil, err
		}
		for _, n := range p.Items {
			add(n)
		}
	}
	return held, nil
}

func (cfg APIConfig) estimateOptions(ctx context.Context, tokens common.TokenList, budgetID string, lang msg.Lang) (EstimateOptions, error) {
	env, available, err := cfg.estimateEnv(ctx, tokens, budgetID)
	if err != nil {
		return EstimateOptions{}, err
	}
	ec := cfg.Estimate.Estimator.Config()
	out := EstimateOptions{
		ProfilesVersion: ec.Version, Source: ec.Source,
		Flavors: env.AllowedFlavors, FlavorsAvailable: available,
		GPUClasses:      gpuClasses(env.Catalog, ec.Resources.WSGPUsPrefix),
		DescribeEnabled: cfg.Estimate.Describer != nil,
		KeyWindow:       ec.LLM.KeyWindow, WindowDays: ec.Resources.WindowDays, Pricing: ec.LLM.Pricing,
		Tools: []EstimateTool{}, WorkspaceKinds: []EstimateWorkspaceKind{},
	}
	if out.Flavors == nil {
		out.Flavors = []estimate.Flavor{}
	}
	m := cfg.Estimate.Estimator.Measured()
	if m != nil {
		out.Measurement = &EstimateMeasurement{From: m.From, To: m.To, ComputedAt: m.ComputedAt, FetchedAt: m.FetchedAt, MinKeys: m.MinKeys}
	}
	for _, id := range ec.ToolIDs() {
		t := ec.LLM.Tools[id]
		tool := EstimateTool{ID: id, Name: msg.T(t.NameDE, t.NameEN).In(lang), Model: t.Model,
			InputTokensPerRequest: t.InputTokensPerRequest, OutputTokensPerRequest: t.OutputTokensPerRequest,
			RequestsPerHour: t.RequestsPerHour, PeakTPMPerPerson: t.PeakTPMPerPerson, ConcurrentShare: t.ConcurrentShare, Source: t.Source}
		if m != nil {
			if mm, ok := m.Models[t.Model]; ok {
				tool.Measured = &mm
			}
		}
		out.Tools = append(out.Tools, tool)
	}
	for _, id := range ec.WorkspaceKindIDs() {
		out.WorkspaceKinds = append(out.WorkspaceKinds, EstimateWorkspaceKind{ID: id, WorkspaceKind: ec.Workspaces.Kinds[id]})
	}
	return out, nil
}

// gpuClasses are the GPU classes the catalogue has (ws-gpus-<class>).
func gpuClasses(catalog []common.Resource, prefix string) []string {
	out := []string{}
	for _, r := range catalog {
		if !r.Withdrawn && strings.HasPrefix(r.ID, prefix) && len(r.ID) > len(prefix) {
			out = append(out, strings.TrimPrefix(r.ID, prefix))
		}
	}
	sort.Strings(out)
	return out
}

func (cfg APIConfig) runEstimate(ctx context.Context, auth identity.AuthContext, req EstimateRequest, lang msg.Lang) (EstimateResponse, error) {
	env, _, err := cfg.estimateEnv(ctx, auth.EffectiveTokens, req.BudgetID)
	if err != nil {
		return EstimateResponse{}, err
	}
	res, err := cfg.Estimate.Estimator.Estimate(req.Input, env, lang)
	if err != nil {
		return EstimateResponse{}, err
	}
	return EstimateResponse{Input: req.Input, Result: res, Budget: cfg.checkBudget(auth, req.BudgetID, req.TerminationDate, res.Limit, lang)}, nil
}

func (cfg APIConfig) runDescribe(ctx context.Context, auth identity.AuthContext, req DescribeRequest, lang msg.Lang) (DescribeResponse, error) {
	if cfg.Estimate == nil || cfg.Estimate.Estimator == nil {
		return DescribeResponse{}, estimate.ErrUnavailable
	}
	if cfg.Estimate.Describer == nil {
		return DescribeResponse{}, estimate.ErrDescriptionOff
	}
	env, _, err := cfg.estimateEnv(ctx, auth.EffectiveTokens, req.BudgetID)
	if err != nil {
		return DescribeResponse{}, err
	}
	ec := cfg.Estimate.Estimator.Config()
	described, err := cfg.Estimate.Describer.Describe(ctx, req.Description, estimate.DescribeContext{AllowedFlavors: env.AllowedFlavors, GPUClasses: gpuClasses(env.Catalog, ec.Resources.WSGPUsPrefix)}, lang)
	if err != nil {
		return DescribeResponse{}, err
	}
	res, err := cfg.Estimate.Estimator.Estimate(described.Input, env, lang)
	if err != nil {
		return DescribeResponse{}, err
	}
	return DescribeResponse{Described: described, Result: res, Budget: cfg.checkBudget(auth, req.BudgetID, req.TerminationDate, res.Limit, lang)}, nil
}

// checkBudget asks the tree's split what the budget would make of limit; nil without a budget. A refusal of the split (no right to request there, a value beyond a resource's maximum) is reported, not raised: the estimate stands on its own.
func (cfg APIConfig) checkBudget(auth identity.AuthContext, budgetID string, end *string, limit common.ProjectQuota, lang msg.Lang) *BudgetCheck {
	budgetID = strings.TrimSpace(budgetID)
	if budgetID == "" || cfg.Service == nil {
		return nil
	}
	out := &BudgetCheck{BudgetID: budgetID}
	plan, err := cfg.Service.PlanSplit(tree.SplitRequest{Limit: limit, TerminationDate: end, BudgetIDs: []string{budgetID}}, auth.UserEmail, auth.EffectiveTokens)
	if err != nil {
		out.Error = estimateErrors.Classify(err).Msg.In(lang)
		return out
	}
	out.Plan = &plan
	out.Fits = len(plan.Unplaced) == 0
	granted := common.ProjectQuota{}
	for _, p := range plan.Parts {
		for k, v := range p.AutoApproved {
			granted[k] += v
		}
	}
	out.AutoApproved = out.Fits
	for k, v := range limit {
		if v != 0 && granted[k] < v {
			out.AutoApproved = false
		}
	}
	return out
}
