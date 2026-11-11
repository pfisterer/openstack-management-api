package webserver

import (
	"testing"

	"github.com/pfisterer/cloud-self-service-golib/mcpserve/mcptest"

	"github.com/pfisterer/openstack-management-api/internal/core/tree"
)

// The MCP tools take hand-written input structs rather than the tree request
// types themselves. That is a deliberate trade — feeding tree.CreateNodeRequest
// straight in would hand a model `kind`, `admin_scope` and `auto_approve` on a
// tool that must not set them, and would push LLM-facing prose into the domain
// types — but it buys a way to drift: a field added to a request type simply
// never reaches the tools, and nothing says so.
//
// This test is what says so. Every field of the domain type must be answered
// for: offered under the same name, offered under a different one, or left out
// with a reason written down. A new field matches none of the three and turns
// this red, which is the point — not to force it into the tool, but to force
// somebody to decide.
//
// Renamed maps a domain field to the tool's name for it, where the tool can be clearer than the storage layer; Omitted lists what the tool deliberately does not offer, and why.
var toolInputContracts = []mcptest.Input{
	{
		Tool: "create_project", Domain: tree.CreateNodeRequest{}, Input: mcpRequestInput{},
		Renamed: map[string]string{"parent_id": "budget_id", "admin_scope": "admins"},
		Omitted: map[string]string{
			"kind":                               "the tool sets it; offering it would let a model create a budget through the project tool",
			"authorized_users":                   "participants are managed in the UI, where the person can see who they are adding",
			"eligible_requesters":                "budget-only",
			"auto_approve":                       "budget-only",
			"allow_sub_budget_requests":          "budget-only",
			"allow_requests_beyond_auto_approve": "budget-only",
			"auto_approve_extensions":            "budget-only",
			"max_project_term_days":              "budget-only",
			"inherits_limit":                     "budget-only",
			"caps":                               "budget-only",
		},
	},
	{
		Tool: "create_budget", Domain: tree.CreateNodeRequest{}, Input: mcpCreateBudgetInput{},
		Renamed: map[string]string{"auto_approve": "auto_approve_limit"},
		Omitted: map[string]string{
			"kind":                               "the tool sets it",
			"termination_date":                   "budgets are not given an end date through this tool",
			"authorized_users":                   "leaf-only",
			"allow_requests_beyond_auto_approve": "defaults to allowed; a hard limit is a policy decision made in the UI",
			"caps":                               "the caps of the providers (release times, local storage, batch jobs at the AI service) are set in the UI; without them the budgets above apply",
		},
	},
	{
		Tool: "request_project_change", Domain: tree.ChangeNodeRequest{}, Input: mcpChangeInput{},
		Omitted: map[string]string{
			"termination_date": "changing the end date is not offered yet",
			"authorized_users": "participants are managed in the UI",
		},
	},
	{
		Tool: "approve_request", Domain: tree.ApproveNodeRequest{}, Input: mcpApproveInput{},
	},
	{
		Tool: "reject_request", Domain: tree.RejectNodeRequest{}, Input: mcpRejectInput{},
	},
	{
		Tool: "move_to_budget", Domain: tree.ReparentNodeRequest{}, Input: mcpMoveInput{},
		Renamed: map[string]string{"new_parent_id": "new_budget_id"},
	},
	{
		Tool: "transfer_ownership", Domain: tree.TransferOwnerRequest{}, Input: mcpTransferInput{},
	},
	{
		Tool: "adopt_imported_project", Domain: tree.PromoteNodeRequest{}, Input: mcpAdoptInput{},
		Renamed: map[string]string{"new_parent_id": "new_budget_id"},
		Omitted: map[string]string{
			"termination_date": "not offered yet",
			"authorized_users": "participants are managed in the UI",
		},
	},
	{
		Tool: "rename_project", Domain: tree.UpdateNodeRequest{}, Input: mcpRenameInput{},
		Omitted: map[string]string{
			"admin_scope":                        "set_project_admins offers it for projects; who may approve on a budget is not changed by an agent",
			"eligible_requesters":                "changing who may request is not done by an agent",
			"auto_approve":                       "granting automatic approval is not done by an agent",
			"allow_sub_budget_requests":          "policy, not a value",
			"allow_requests_beyond_auto_approve": "policy, not a value",
			"auto_approve_extensions":            "policy, not a value",
			"max_project_term_days":              "policy, not a value; it shortens projects below when lowered",
			"caps":                               "policy of a budget, set in the UI",
			"limit":                              "resources change through request_project_change, which records a reason",
			"termination_date":                   "not offered yet",
			"inherits_limit":                     "hands a budget the whole limit above it; a manager decides that in the UI",
		},
	},
	{
		Tool: "release_project", Domain: tree.ReleaseNodeRequest{}, Input: mcpReleaseInput{},
	},
	{
		Tool: "set_project_allocation", Domain: tree.AllocationRequest{}, Input: mcpAllocationInput{},
	},
	{
		Tool: "set_project_admins", Domain: tree.UpdateNodeRequest{}, Input: mcpSetAdminsInput{},
		Renamed: map[string]string{"admin_scope": "admins"},
		Omitted: map[string]string{
			"name":                               "rename_project",
			"eligible_requesters":                "budget-only",
			"auto_approve":                       "budget-only",
			"allow_sub_budget_requests":          "budget-only",
			"allow_requests_beyond_auto_approve": "budget-only",
			"auto_approve_extensions":            "budget-only",
			"max_project_term_days":              "budget-only",
			"caps":                               "budget-only",
			"inherits_limit":                     "budget-only",
			"limit":                              "request_project_change",
			"termination_date":                   "not offered yet",
		},
	},
}

func TestMCPToolInputsCoverTheirRequestTypes(t *testing.T) {
	mcptest.CheckInputs(t, toolInputContracts)
}
