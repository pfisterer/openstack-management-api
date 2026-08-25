package webserver

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pfisterer/cloud-self-service-golib/mcpserve"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/tree"
	"go.uber.org/zap"
)

// The MCP endpoint, so an LLM can work with this API on a person's behalf.
//
// It lives IN this service rather than in a server of its own, because one line
// buys everything such a server would have to rebuild: /mcp is mounted with the
// same auth middleware as /v1, and therefore gets token resolution, the
// read-only flag, the role-switch protection on the token subject and rights
// fetched fresh from the role provider on every request. A process in front
// would have to answer each of those again, or pass them through and be a hop
// with no content.
//
// The one thing it does NOT inherit is the read-only rule: for REST, "not a GET"
// stands in for "changes something", which is why RejectWritesForReadOnlyTokens
// sits on the /v1 group. Every MCP call is a POST, reads included, so the tool
// says what it does and the check runs against that — the `mutates` argument to
// mcpserve.AddTool, which is where that rule now lives for both services.
//
// The transport, the per-request server and that gate come from
// github.com/pfisterer/cloud-self-service-golib/mcpserve. What stays here is
// what is about projects: the tools, their payloads, and who the caller is.

// mcpCaller is the identity a tool acts as, lifted out of the Gin context and
// into the request context, which is the only thing the SDK hands to getServer.
type mcpCaller struct {
	// actorEmail is the real caller, for the audit trail; userEmail is the
	// effective one a role switch may have replaced. Same distinction the REST
	// handlers make, and for the same reason.
	actorEmail string
	userEmail  string
	tokens     common.TokenList
	readOnly   bool
}

// serviceActor is the identity handed to the service as the one making the
// change. Every REST write passes auth.UserEmail (api_nodes.go), i.e. the
// EFFECTIVE identity — under a role switch the change is recorded as the
// impersonated user, not as whoever is really typing. Right or wrong, the same
// action through MCP has to land in the history the same way, or which channel
// was used becomes visible in the audit trail by accident.
//
// actorEmail stays for OUR log lines, where the real caller is the useful one.
func (c mcpCaller) serviceActor() string { return c.userEmail }

// ReadOnly satisfies mcpserve.Caller: whether this request's credential may
// change anything. It is the only thing the shared wiring knows about a caller —
// the identity above never travels through it.
func (c mcpCaller) ReadOnly() bool { return c.readOnly }

// RegisterMCPRoutes mounts the MCP endpoint on the given group. The group must
// already carry the authentication middleware and must NOT carry
// RejectWritesForReadOnlyTokens.
func RegisterMCPRoutes(group *gin.RouterGroup, cfg APIConfig, log *zap.SugaredLogger) {
	if cfg.Service == nil {
		return
	}
	group.Use(EffectiveAuthMiddleware(cfg.Service))

	// A server per request, built around the caller: a tool closes over the
	// identity that called it, so there is no way for one request's tools to run
	// with another's rights. That, the transport and the read-only gate are the
	// same in every service doing this and live in the shared library; what is
	// left here is turning a Gin context into a caller.
	handler := mcpserve.Handler(func(caller mcpCaller) *mcp.Server {
		return newMCPServer(cfg, caller, log)
	})

	group.Any("", func(c *gin.Context) {
		auth, err := mustGetAuthContext(c)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		caller := mcpCaller{
			actorEmail: auth.ActorEmail,
			userEmail:  auth.UserEmail,
			tokens:     auth.EffectiveTokens,
			readOnly:   IsReadOnlyToken(c),
		}
		ctx := mcpserve.WithCaller(c.Request.Context(), caller)
		handler.ServeHTTP(c.Writer, c.Request.WithContext(ctx))
	})
}

func newMCPServer(cfg APIConfig, caller mcpCaller, log *zap.SugaredLogger) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		Name:    "dhbw-cloud-projects",
		Title:   "DHBW Cloud — projects and budgets",
		Version: "1",
	}, nil)

	registerProjectTools(s, cfg, caller, log)
	return s
}

// ── Tool payloads ───────────────────────────────────────────────────────────
// Deliberately not tree.Node: that carries history, participant lists and the
// policy fields, which would fill a model's context with things it did not ask
// for and cannot use. These are the fields a person asks about.

type mcpProject struct {
	ID     string `json:"id" jsonschema:"the project's id, used to refer to it in other tools"`
	Name   string `json:"name" jsonschema:"human-readable name"`
	Status string `json:"status" jsonschema:"one of pending, approved, change_pending, rejected, released, imported"`
	Owner  string `json:"owner,omitempty" jsonschema:"the person responsible, as user:<email>"`
	// PaidFrom is the id of the budget this project is charged to, not its name:
	// the name is not unique and cannot be fed back into another tool.
	PaidFrom        string         `json:"paid_from,omitempty" jsonschema:"id of the budget this is charged to"`
	Limit           map[string]int `json:"limit,omitempty" jsonschema:"granted resources, e.g. cores, ram, storage"`
	InUse           map[string]int `json:"in_use,omitempty" jsonschema:"what OpenStack reports as actually used; a missing key means not measured, not zero"`
	OSProjectID     string         `json:"os_project_id,omitempty" jsonschema:"the OpenStack project, empty while it is still being created"`
	TerminationDate string         `json:"termination_date,omitempty" jsonschema:"intended end of life"`
}

func toMCPProject(n tree.Node) mcpProject {
	p := mcpProject{
		ID:          n.ID,
		Name:        n.Name,
		Status:      n.Status,
		Owner:       n.Owner,
		Limit:       n.Limit,
		InUse:       n.OSInUse,
		OSProjectID: n.OSProjectID,
	}
	if n.ParentID != nil {
		p.PaidFrom = *n.ParentID
	}
	if n.TerminationDate != nil {
		p.TerminationDate = *n.TerminationDate
	}
	return p
}

func toMCPProjects(page tree.NodePage) []mcpProject {
	out := make([]mcpProject, 0, len(page.Items))
	for _, n := range page.Items {
		out = append(out, toMCPProject(n))
	}
	return out
}

// mcpPageInput is the paging every listing tool takes. Zero means "the default",
// so a model that omits it still gets a sensible answer.
type mcpPageInput struct {
	Limit  int `json:"limit,omitempty" jsonschema:"how many to return, default 50"`
	Offset int `json:"offset,omitempty" jsonschema:"how many to skip, for paging"`
}

func (p mcpPageInput) resolve() (int, int) {
	limit := p.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	return limit, max(p.Offset, 0)
}

// mcpProjectList is what a listing tool answers with. Total travels along so a
// model can tell "that is all of them" from "there is more behind this page".
type mcpProjectList struct {
	Projects []mcpProject `json:"projects"`
	Total    int          `json:"total" jsonschema:"how many exist in total, which may exceed the number returned"`
}

// ── Tool inputs ─────────────────────────────────────────────────────────────
// Package-level so a test can reflect over them: mcp_input_drift_test.go holds
// each against the tree request type it feeds, and fails when a field is added
// there without a decision being made here.

type mcpGetInput struct {
	ID string `json:"id" jsonschema:"id of the project or budget"`
}

type mcpSearchInput struct {
	Query string `json:"query" jsonschema:"text to search for in names"`
	mcpPageInput
}

type mcpRequestInput struct {
	BudgetID string `json:"budget_id" jsonschema:"id of the budget to charge this to; use list_my_budgets or search_projects to find it"`
	Name     string `json:"name" jsonschema:"name for the new project"`
	Reason   string `json:"reason" jsonschema:"why it is needed; a manager reads this when deciding"`
	// A map, not named fields: which resources exist is deployment
	// configuration (get_project shows what an existing one uses), and
	// hard-coding cores/ram/storage here would be a second place to change.
	Limit           map[string]int `json:"limit" jsonschema:"requested resources by id, e.g. {\"cores\": 4, \"ram\": 8192}"`
	TerminationDate string         `json:"termination_date,omitempty" jsonschema:"optional intended end of life, RFC3339"`
}

type mcpChangeInput struct {
	ID     string         `json:"id" jsonschema:"id of the project to change"`
	Limit  map[string]int `json:"limit" jsonschema:"the new resource amounts, given in full rather than as a delta"`
	Reason string         `json:"reason,omitempty" jsonschema:"why the change is needed"`
}

type mcpApproveInput struct {
	ID string `json:"id" jsonschema:"id of the request to approve"`
	// Approving with less than was asked for is a normal outcome, and it has
	// to be expressible or the only options are all or nothing.
	ModifiedLimit map[string]int `json:"modified_limit,omitempty" jsonschema:"optional: approve with these amounts instead of the requested ones"`
}

type mcpRejectInput struct {
	ID     string `json:"id" jsonschema:"id of the request to reject"`
	Reason string `json:"reason,omitempty" jsonschema:"why it was turned down; the requester sees this"`
}

type mcpRenameInput struct {
	ID   string `json:"id" jsonschema:"id of the project or budget to rename"`
	Name string `json:"name" jsonschema:"the new name"`
}

type mcpCreateBudgetInput struct {
	ParentID string         `json:"parent_id" jsonschema:"id of the budget this one is carved out of"`
	Name     string         `json:"name" jsonschema:"name of the new budget"`
	Reason   string         `json:"reason" jsonschema:"why it is needed"`
	Limit    map[string]int `json:"limit" jsonschema:"the cap for everything under it; -1 means unlimited"`
	// Two token lists, and the distinction is the one the old model got
	// wrong: managing is not the same as being allowed to spend. Keeping
	// them apart is why allowance members can no longer approve each other.
	//
	// admin_scope carries no omitempty, which is what makes the SDK mark it
	// required: a budget without one is refused by the service, and leaving it
	// optional in the schema only means a model finds that out by being turned
	// down once. Found the first time this ran against staging.
	AdminScope         []string       `json:"admin_scope" jsonschema:"tokens that may approve requests here, e.g. group:dept_cs_admin or user:a@b.c"`
	EligibleRequesters []string       `json:"eligible_requesters,omitempty" jsonschema:"tokens that may request something here, without any say over decisions"`
	AutoApproveLimit   map[string]int `json:"auto_approve_limit,omitempty" jsonschema:"optional: requests up to this size per requester are approved without a human"`
}

type mcpMoveInput struct {
	ID          string `json:"id" jsonschema:"id of the project or budget to move"`
	NewBudgetID string `json:"new_budget_id" jsonschema:"id of the budget it should be charged to from now on"`
}

type mcpTransferInput struct {
	ID       string `json:"id" jsonschema:"id of the project to hand over"`
	NewOwner string `json:"new_owner" jsonschema:"the new owner's email address"`
}

type mcpAdoptInput struct {
	ID          string         `json:"id" jsonschema:"id of the imported project"`
	NewBudgetID string         `json:"new_budget_id" jsonschema:"budget it should be charged to"`
	Owner       string         `json:"owner" jsonschema:"email of the person who will be responsible for it"`
	Reason      string         `json:"reason" jsonschema:"why it is being taken over"`
	Limit       map[string]int `json:"limit,omitempty" jsonschema:"optional: grant these amounts instead of keeping the quota it already has in OpenStack"`
}

type mcpReleaseInput struct {
	ID          string `json:"id" jsonschema:"id of the project to give up"`
	ConfirmName string `json:"confirm_name" jsonschema:"the project's exact current name, as a confirmation that this is the right one"`
}

type mcpDeleteBudgetInput struct {
	ID          string `json:"id" jsonschema:"id of the budget to delete"`
	ConfirmName string `json:"confirm_name" jsonschema:"the budget's exact current name, as a confirmation that this is the right one"`
}

// ── Tools ───────────────────────────────────────────────────────────────────

func registerProjectTools(s *mcp.Server, cfg APIConfig, caller mcpCaller, log *zap.SugaredLogger) {
	mcpserve.AddTool(s, caller, false, &mcp.Tool{
		Name:        "list_my_projects",
		Description: "List the cloud projects the calling user owns, with their status and granted resources.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in mcpPageInput) (*mcp.CallToolResult, mcpProjectList, error) {
		limit, offset := in.resolve()
		page, err := cfg.Service.ListMine(caller.userEmail, limit, offset)
		if err != nil {
			return nil, mcpProjectList{}, fmt.Errorf("list projects: %w", err)
		}
		return nil, mcpProjectList{Projects: toMCPProjects(page), Total: page.Total}, nil
	})

	mcpserve.AddTool(s, caller, false, &mcp.Tool{
		Name:        "list_my_budgets",
		Description: "List the budgets the calling user manages. A budget is what projects are charged against.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in mcpPageInput) (*mcp.CallToolResult, mcpProjectList, error) {
		limit, offset := in.resolve()
		page, err := cfg.Service.ListMyBudgets(caller.tokens, limit, offset)
		if err != nil {
			return nil, mcpProjectList{}, fmt.Errorf("list budgets: %w", err)
		}
		return nil, mcpProjectList{Projects: toMCPProjects(page), Total: page.Total}, nil
	})

	mcpserve.AddTool(s, caller, false, &mcp.Tool{
		Name:        "get_project",
		Description: "Look up one project or budget by id.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in mcpGetInput) (*mcp.CallToolResult, mcpProject, error) {
		node, err := cfg.Service.GetNode(in.ID, caller.tokens)
		if err != nil {
			return nil, mcpProject{}, fmt.Errorf("get %q: %w", in.ID, err)
		}
		if node == nil {
			return nil, mcpProject{}, fmt.Errorf("no project or budget with id %q, or it is not visible to you", in.ID)
		}
		return nil, toMCPProject(*node), nil
	})

	mcpserve.AddTool(s, caller, false, &mcp.Tool{
		Name:        "search_projects",
		Description: "Search the projects and budgets visible to the calling user by name.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in mcpSearchInput) (*mcp.CallToolResult, mcpProjectList, error) {
		limit, offset := in.resolve()
		page, err := cfg.Service.SearchNodes(caller.tokens, in.Query, limit, offset)
		if err != nil {
			return nil, mcpProjectList{}, fmt.Errorf("search: %w", err)
		}
		return nil, mcpProjectList{Projects: toMCPProjects(page), Total: page.Total}, nil
	})

	registerProjectWriteTools(s, cfg, caller, log)
	registerTreeAdminTools(s, cfg, caller, log)
	registerDestructiveTools(s, cfg, caller, log)
}

// The writes. All of them are authorised exactly as the same action from the UI
// is — the token carries the person's identity and their rights are fetched
// fresh per request — so the question is not "may an agent write" but "which
// writes can be undone".
//
// These can: a request waits for a decision, a change proposal leaves the
// approved limits in force until someone accepts it, a rejection can be
// re-requested, a rename renames. Every one of them is capped by the quota tree,
// which is the last line of defence and is meant to be.
//
// What is NOT here is release and delete. Not because writing is dangerous, but
// because those two cannot be taken back: releasing deletes the OpenStack
// project wherever deleteReleasedProjects is on, within one reconcile interval,
// and an agent derives its calls from text people wrote — project names,
// reasons, token labels are all free text. Those need their own scope and a
// confirmation step (k6) before they are worth offering.
func registerProjectWriteTools(s *mcp.Server, cfg APIConfig, caller mcpCaller, log *zap.SugaredLogger) {
	// Named for what it does, not for one of its outcomes: the same call creates
	// the project outright when the caller manages the budget, leaves it waiting
	// for a decision when they may only request, and approves it immediately
	// when the budget auto-approves a request that size. Calling it
	// "request_project" would have made a model report "waiting for approval"
	// for something that is already running.
	mcpserve.AddTool(s, caller, true, &mcp.Tool{
		Name: "create_project",
		Description: "Create a cloud project under a budget. If you manage that budget it is created directly; " +
			"otherwise it becomes a request waiting for a decision, unless the budget auto-approves this size. " +
			"The returned status says which of the three happened.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in mcpRequestInput) (*mcp.CallToolResult, mcpProject, error) {
		if in.Reason == "" {
			return nil, mcpProject{}, fmt.Errorf("reason must not be empty — a manager decides on it")
		}
		req := tree.CreateNodeRequest{
			ParentID: in.BudgetID,
			Kind:     tree.KindProject,
			Name:     in.Name,
			Reason:   in.Reason,
			Limit:    common.ProjectQuota(in.Limit),
		}
		if in.TerminationDate != "" {
			req.TerminationDate = &in.TerminationDate
		}
		log.Infow("MCP create_project", "actor", caller.actorEmail, "budget_id", in.BudgetID)
		node, err := cfg.Service.CreateNode(req, caller.serviceActor(), caller.userEmail, caller.tokens)
		if err != nil {
			return nil, mcpProject{}, fmt.Errorf("request project: %w", err)
		}
		return nil, toMCPProject(node), nil
	})

	mcpserve.AddTool(s, caller, true, &mcp.Tool{
		Name: "request_project_change",
		Description: "Propose new resource amounts for an existing project. The project keeps running on its " +
			"currently approved limits until someone accepts the proposal.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in mcpChangeInput) (*mcp.CallToolResult, mcpProject, error) {
		limit := common.ProjectQuota(in.Limit)
		req := tree.ChangeNodeRequest{Limit: &limit}
		if in.Reason != "" {
			req.Reason = &in.Reason
		}
		log.Infow("MCP request_project_change", "actor", caller.actorEmail, "node_id", in.ID)
		node, err := cfg.Service.RequestChange(in.ID, req, caller.serviceActor(), caller.tokens)
		if err != nil {
			return nil, mcpProject{}, fmt.Errorf("request change on %q: %w", in.ID, err)
		}
		return nil, toMCPProject(node), nil
	})

	mcpserve.AddTool(s, caller, true, &mcp.Tool{
		Name: "approve_request",
		Description: "Approve a pending project request or change. Requires managing the budget it is charged to. " +
			"An approved project is created in OpenStack within a few minutes.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in mcpApproveInput) (*mcp.CallToolResult, mcpProject, error) {
		var req tree.ApproveNodeRequest
		if len(in.ModifiedLimit) > 0 {
			limit := common.ProjectQuota(in.ModifiedLimit)
			req.ModifiedLimit = &limit
		}
		log.Infow("MCP approve_request", "actor", caller.actorEmail, "node_id", in.ID)
		node, err := cfg.Service.ApproveNode(in.ID, req, caller.serviceActor(), caller.tokens)
		if err != nil {
			return nil, mcpProject{}, fmt.Errorf("approve %q: %w", in.ID, err)
		}
		return nil, toMCPProject(node), nil
	})

	mcpserve.AddTool(s, caller, true, &mcp.Tool{
		Name: "reject_request",
		Description: "Turn down a pending project request or change. Nothing is deleted — the requester can ask " +
			"again. Requires managing the budget it is charged to.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in mcpRejectInput) (*mcp.CallToolResult, mcpProject, error) {
		var req tree.RejectNodeRequest
		if in.Reason != "" {
			req.Reason = &in.Reason
		}
		log.Infow("MCP reject_request", "actor", caller.actorEmail, "node_id", in.ID)
		node, err := cfg.Service.RejectNode(in.ID, req, caller.serviceActor(), caller.tokens)
		if err != nil {
			return nil, mcpProject{}, fmt.Errorf("reject %q: %w", in.ID, err)
		}
		return nil, toMCPProject(node), nil
	})

	mcpserve.AddTool(s, caller, true, &mcp.Tool{
		Name:        "rename_project",
		Description: "Change the name of a project or budget. Does not touch its resources, status or ownership.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in mcpRenameInput) (*mcp.CallToolResult, mcpProject, error) {
		if in.Name == "" {
			return nil, mcpProject{}, fmt.Errorf("name must not be empty")
		}
		log.Infow("MCP rename_project", "actor", caller.actorEmail, "node_id", in.ID)
		node, err := cfg.Service.UpdateNode(in.ID, tree.UpdateNodeRequest{Name: &in.Name},
			caller.serviceActor(), caller.tokens)
		if err != nil {
			return nil, mcpProject{}, fmt.Errorf("rename %q: %w", in.ID, err)
		}
		return nil, toMCPProject(node), nil
	})
}

// The structural tools: they change the SHAPE of the tree rather than what one
// project holds. All of them are managerial acts, and the service refuses each
// one to a caller without rights on both ends of the move — mcp.go performs no
// authorization of its own, on purpose (see the note at registerProjectTools).
//
// They are offered for the same reason as the writes above: none of them
// destroys anything. A budget can be emptied, a move can be moved back, an
// ownership transfer can be transferred again. Deleting a budget cannot, and is
// therefore absent along with release.
func registerTreeAdminTools(s *mcp.Server, cfg APIConfig, caller mcpCaller, log *zap.SugaredLogger) {
	mcpserve.AddTool(s, caller, true, &mcp.Tool{
		Name: "create_budget",
		Description: "Create a budget under an existing one. A budget caps what everything below it may hold " +
			"together, and says who may approve and who may request there.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in mcpCreateBudgetInput) (*mcp.CallToolResult, mcpProject, error) {
		if in.Reason == "" {
			return nil, mcpProject{}, fmt.Errorf("reason must not be empty")
		}
		req := tree.CreateNodeRequest{
			ParentID:           in.ParentID,
			Kind:               tree.KindBudget,
			Name:               in.Name,
			Reason:             in.Reason,
			Limit:              common.ProjectQuota(in.Limit),
			AdminScope:         common.TokenList(in.AdminScope),
			EligibleRequesters: common.TokenList(in.EligibleRequesters),
		}
		if len(in.AutoApproveLimit) > 0 {
			req.AutoApprove = &tree.AutoApprove{PerRequesterLimit: common.ProjectQuota(in.AutoApproveLimit)}
		}
		log.Infow("MCP create_budget", "actor", caller.actorEmail, "parent_id", in.ParentID)
		node, err := cfg.Service.CreateNode(req, caller.serviceActor(), caller.userEmail, caller.tokens)
		if err != nil {
			return nil, mcpProject{}, fmt.Errorf("create budget: %w", err)
		}
		return nil, toMCPProject(node), nil
	})

	mcpserve.AddTool(s, caller, true, &mcp.Tool{
		Name: "move_to_budget",
		Description: "Charge an existing project or budget to a different budget. Requires managing both the one " +
			"it leaves and the one it joins, and the new one must have room for it.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in mcpMoveInput) (*mcp.CallToolResult, mcpProject, error) {
		log.Infow("MCP move_to_budget", "actor", caller.actorEmail, "node_id", in.ID, "new_parent", in.NewBudgetID)
		node, err := cfg.Service.ReparentNode(in.ID, tree.ReparentNodeRequest{NewParentID: in.NewBudgetID},
			caller.serviceActor(), caller.tokens)
		if err != nil {
			return nil, mcpProject{}, fmt.Errorf("move %q: %w", in.ID, err)
		}
		return nil, toMCPProject(node), nil
	})

	mcpserve.AddTool(s, caller, true, &mcp.Tool{
		Name: "transfer_ownership",
		Description: "Make someone else the responsible owner of a project. The previous owner keeps no special " +
			"claim on it afterwards.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in mcpTransferInput) (*mcp.CallToolResult, mcpProject, error) {
		log.Infow("MCP transfer_ownership", "actor", caller.actorEmail, "node_id", in.ID)
		node, err := cfg.Service.TransferOwner(in.ID, tree.TransferOwnerRequest{NewOwner: in.NewOwner},
			caller.serviceActor(), caller.tokens)
		if err != nil {
			return nil, mcpProject{}, fmt.Errorf("transfer %q: %w", in.ID, err)
		}
		return nil, toMCPProject(node), nil
	})

	mcpserve.AddTool(s, caller, true, &mcp.Tool{
		Name: "adopt_imported_project",
		Description: "Take an imported project — one that exists in OpenStack but was created outside the " +
			"self-service — into a budget, so it is managed and counted like the others.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in mcpAdoptInput) (*mcp.CallToolResult, mcpProject, error) {
		if in.Reason == "" {
			return nil, mcpProject{}, fmt.Errorf("reason must not be empty")
		}
		req := tree.PromoteNodeRequest{
			NewParentID: in.NewBudgetID,
			Owner:       in.Owner,
			Reason:      in.Reason,
			Limit:       common.ProjectQuota(in.Limit),
		}
		log.Infow("MCP adopt_imported_project", "actor", caller.actorEmail, "node_id", in.ID)
		node, err := cfg.Service.PromoteNode(in.ID, req, caller.serviceActor(), caller.tokens)
		if err != nil {
			return nil, mcpProject{}, fmt.Errorf("adopt %q: %w", in.ID, err)
		}
		return nil, toMCPProject(node), nil
	})
}

// The two that cannot be taken back.
//
// They are offered — leaving them out was the wrong line. An agent that can
// create and approve but never clean up is a ratchet, and in a system whose last
// defence is a capacity cap that is the worse asymmetry. The authorisation is
// the same as for every other write: the person's own rights, and a token they
// issued with writing enabled.
//
// What they get on top is a name that has to be echoed back. That is NOT a
// defence against prompt injection — injected text can contain the name as
// easily as the id — and it is not sold as one. It catches the likelier
// failure: a model that resolved "delete the old one" to the wrong id. Echoing
// the name means it had to read the thing first, and it puts the name in front
// of the person whose client is asking them to approve the call.
func registerDestructiveTools(s *mcp.Server, cfg APIConfig, caller mcpCaller, log *zap.SugaredLogger) {
	// confirmName fails unless the caller echoed the node's current name back.
	confirmName := func(id, confirm string) (*tree.Node, error) {
		node, err := cfg.Service.GetNode(id, caller.tokens)
		if err != nil {
			return nil, fmt.Errorf("look up %q: %w", id, err)
		}
		if node == nil {
			return nil, fmt.Errorf("no project or budget with id %q, or it is not visible to you", id)
		}
		if err := mcpserve.ConfirmEcho("confirm_name", confirm, node.Name, fmt.Sprintf("%q", id)); err != nil {
			return nil, err
		}
		return node, nil
	}

	mcpserve.AddTool(s, caller, true, &mcp.Tool{
		Name: "release_project",
		Description: "Give up a project. This cannot be undone: depending on the deployment the OpenStack " +
			"project and everything in it — servers, volumes, data — is deleted within minutes, or marked for " +
			"deletion and removed later. Ask the person before calling this.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in mcpReleaseInput) (*mcp.CallToolResult, mcpProject, error) {
		if _, err := confirmName(in.ID, in.ConfirmName); err != nil {
			return nil, mcpProject{}, err
		}
		log.Infow("MCP release_project", "actor", caller.actorEmail, "node_id", in.ID)
		node, err := cfg.Service.ReleaseNode(in.ID, caller.serviceActor(), caller.tokens)
		if err != nil {
			return nil, mcpProject{}, fmt.Errorf("release %q: %w", in.ID, err)
		}
		return nil, toMCPProject(node), nil
	})

	mcpserve.AddTool(s, caller, true, &mcp.Tool{
		Name: "delete_budget",
		Description: "Delete a budget. Only budgets, never projects — those are released instead. This cannot " +
			"be undone. Ask the person before calling this.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in mcpDeleteBudgetInput) (*mcp.CallToolResult, mcpProject, error) {
		node, err := confirmName(in.ID, in.ConfirmName)
		if err != nil {
			return nil, mcpProject{}, err
		}
		log.Infow("MCP delete_budget", "actor", caller.actorEmail, "node_id", in.ID)
		if err := cfg.Service.DeleteNode(in.ID, caller.serviceActor(), caller.tokens); err != nil {
			return nil, mcpProject{}, fmt.Errorf("delete %q: %w", in.ID, err)
		}
		return nil, toMCPProject(*node), nil
	})
}
